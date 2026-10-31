package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/llm"
)

func TestExactCacheHitAcrossSurfacesAndStreaming(t *testing.T) {
	h := newHarness(t)
	body := `{"model":"cached","max_tokens":20,"messages":[{"role":"user","content":"what is a gateway"}]}`
	first := h.post(t, "/v1/messages", body)
	var a llm.Response
	_ = json.NewDecoder(first.Body).Decode(&a)
	if first.Header.Get("X-Switchyard-Cache") != "" {
		t.Fatal("first request cannot be a hit")
	}
	waitFor(t, "cache fill", func() bool {
		resp := h.post(t, "/v1/messages", body)
		return resp.Header.Get("X-Switchyard-Cache") == "exact"
	})
	upstream := h.mock.Stats().Requests

	// The same request streamed is replayed from the cache.
	resp := h.post(t, "/v1/messages", strings.Replace(body, `"max_tokens"`, `"stream":true,"max_tokens"`, 1))
	_, text, _, stop := collect(t, resp.Body)
	if resp.Header.Get("X-Switchyard-Cache") != "exact" || text != a.Content[0].Text || stop != a.StopReason {
		t.Fatalf("replay: header %q text %q stop %q", resp.Header.Get("X-Switchyard-Cache"), text, stop)
	}
	if h.mock.Stats().Requests != upstream {
		t.Fatal("a hit must not reach the upstream")
	}
	// Hits are free.
	recs := h.usage.Records()
	if last := recs[len(recs)-1]; last.Cache != "exact" || last.CostUSD != 0 {
		t.Fatalf("hit record = %+v", last)
	}
}

func TestExactCacheIsPerTenant(t *testing.T) {
	h := newHarness(t)
	body := `{"model":"cached","max_tokens":20,"messages":[{"role":"user","content":"tenant isolation"}]}`
	h.post(t, "/v1/messages", body)
	waitFor(t, "cache fill", func() bool { return h.post(t, "/v1/messages", body).Header.Get("X-Switchyard-Cache") == "exact" })
	other, _ := h.keys.NewStaticKey(auth.Key{TenantID: "t2"})
	if resp := h.postKey(t, other, "/v1/messages", body); resp.Header.Get("X-Switchyard-Cache") != "" {
		t.Fatal("another tenant got a cached response")
	}
}

func TestSemanticCache(t *testing.T) {
	h := newHarness(t)
	ask := func(q string) string {
		resp := h.post(t, "/v1/chat/completions", `{"model":"semantic","max_tokens":20,"messages":[{"role":"user","content":"`+q+`"}]}`)
		return resp.Header.Get("X-Switchyard-Cache")
	}
	ask("What is the capital of France?")
	waitFor(t, "semantic fill", func() bool { return ask("what is the capital of france") == "semantic" })
	if got := ask("How do I bake sourdough bread at home?"); got != "" {
		t.Fatalf("unrelated question hit the semantic cache (%q)", got)
	}
}
