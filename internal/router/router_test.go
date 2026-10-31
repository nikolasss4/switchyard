package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/provider"
)

func setup(t *testing.T, yaml string, fakes ...*fake) *Router {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ps := map[string]provider.Provider{}
	for _, f := range fakes {
		ps[f.name] = f
	}
	return New(cfg, ps)
}

const chain = `
retry: {max_attempts: 1}
allow_direct: true
providers:
  - {name: a, type: openai, base_url: http://a}
  - {name: b, type: openai, base_url: http://b}
  - {name: c, type: anthropic, base_url: http://c}
models:
  - alias: smart
    targets: [{provider: a, model: a-1}, {provider: b, model: b-1}, {provider: c, model: c-1}]
  - alias: fast
    targets: [{provider: b, model: b-fast}]
  - alias: batch
    targets: [{provider: c, model: c-cheap}]
rules:
  - when: {tenant: acme, model: smart}
    use: fast
  - when: {headers: {X-Switchyard-Tier: batch}}
    use: batch
`

var req = &llm.Request{Model: "smart", MaxTokens: 10}

func TestResolveRules(t *testing.T) {
	r := setup(t, chain, &fake{name: "a"}, &fake{name: "b"}, &fake{name: "c"})
	batch := http.Header{}
	batch.Set("X-Switchyard-Tier", "batch")
	cases := []struct {
		model, tenant string
		h             http.Header
		want          string
	}{
		{"smart", "other", nil, "smart"},
		{"smart", "acme", nil, "fast"},             // tenant + model rule
		{"fast", "acme", nil, "fast"},              // rule needs model=smart
		{"smart", "other", batch, "batch"},         // header rule
		{"smart", "acme", batch, "fast"},           // first matching rule wins
		{"b/some-model", "x", nil, "b/some-model"}, // direct provider/model
	}
	for _, c := range cases {
		route, err := r.Resolve(c.model, c.tenant, c.h)
		if err != nil || route.Alias != c.want {
			t.Errorf("Resolve(%s, %s) = %v %v, want %s", c.model, c.tenant, route, err, c.want)
		}
	}
	if _, err := r.Resolve("nope", "", nil); !errors.Is(err, ErrUnknownModel) {
		t.Fatal(err)
	}
	if _, err := r.Resolve("zz/model", "", nil); !errors.Is(err, ErrUnknownModel) {
		t.Fatal("unknown provider prefix must not resolve")
	}
	r.direct = false
	if _, err := r.Resolve("b/some-model", "", nil); !errors.Is(err, ErrUnknownModel) {
		t.Fatal("provider/model must not resolve unless allow_direct is set")
	}
}

func TestFallbackChain(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(503)}}
	b := &fake{name: "b", script: []behaviour{fail(401)}} // bad upstream key: provider's problem
	c := &fake{name: "c", script: []behaviour{ok()}}
	r := setup(t, chain, a, b, c)
	route, _ := r.Resolve("smart", "", nil)
	resp, res, err := r.Complete(context.Background(), route, req)
	if err != nil || resp.ID != "c" || resp.Model != "c-1" || res.FallbackDepth != 2 || res.Attempts != 3 {
		t.Fatalf("resp %+v res %+v err %v", resp, res, err)
	}
}

func TestBadRequestDoesNotFallBack(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(400)}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, chain, a, b, &fake{name: "c", script: []behaviour{ok()}})
	route, _ := r.Resolve("smart", "", nil)
	_, res, err := r.Complete(context.Background(), route, req)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 400 || b.calls.Load() != 0 || res.Attempts != 1 {
		t.Fatalf("err %v, b called %d times", err, b.calls.Load())
	}
}

func TestAllTargetsFail(t *testing.T) {
	r := setup(t, chain, &fake{name: "a", script: []behaviour{fail(500)}}, &fake{name: "b", script: []behaviour{fail(502)}}, &fake{name: "c", script: []behaviour{fail(529)}})
	route, _ := r.Resolve("smart", "", nil)
	_, res, err := r.Complete(context.Background(), route, req)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 529 || res.Attempts != 3 {
		t.Fatalf("want the last error (529) after 3 attempts, got %v %+v", err, res)
	}
}

func TestStreamFallsBackBeforeFirstToken(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(529)}} // fails right after message_start
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, chain, a, b, &fake{name: "c", script: []behaviour{ok()}})
	route, _ := r.Resolve("smart", "", nil)
	s, res, err := r.Stream(context.Background(), route, req)
	if err != nil || res.FallbackDepth != 1 {
		t.Fatalf("res %+v err %v", res, err)
	}
	defer s.Close()
	first, _ := s.Next()
	if first.Message.ID != "b" {
		t.Fatalf("stream served by %s, want b (a's message_start must not leak)", first.Message.ID)
	}
}

func TestStreamNeverRetriedAfterFirstToken(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{{midStream: &provider.Error{Provider: "a", Status: 529}}}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, chain, a, b, &fake{name: "c", script: []behaviour{ok()}})
	route, _ := r.Resolve("smart", "", nil)
	s, _, err := r.Stream(context.Background(), route, req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got error
	for got == nil {
		_, got = s.Next()
	}
	var pe *provider.Error
	if !errors.As(got, &pe) || pe.Status != 529 || b.calls.Load() != 0 {
		t.Fatalf("mid-stream error must reach the client without a retry: %v (b calls %d)", got, b.calls.Load())
	}
	if errors.Is(got, io.EOF) {
		t.Fatal("stream ended cleanly")
	}
}

func TestCancelledClientStopsTheChain(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(503)}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, chain, a, b, &fake{name: "c", script: []behaviour{ok()}})
	route, _ := r.Resolve("smart", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := r.Complete(ctx, route, req)
	if !errors.Is(err, context.Canceled) || b.calls.Load() != 0 {
		t.Fatalf("err %v, b calls %d", err, b.calls.Load())
	}
}
