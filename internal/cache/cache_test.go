package cache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/nikolasss4/switchyard/internal/llm"
)

func req(t *testing.T, s string) *llm.Request {
	t.Helper()
	var r llm.Request
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestFingerprint(t *testing.T) {
	a := req(t, `{"model":"x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	b := req(t, `{"model":"y","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c := req(t, `{"model":"x","max_tokens":6,"messages":[{"role":"user","content":"hi"}]}`)
	fa, _ := Fingerprint("t1", "fast", a)
	fb, _ := Fingerprint("t1", "fast", b)
	fc, _ := Fingerprint("t1", "fast", c)
	fd, _ := Fingerprint("t2", "fast", a)
	fe, _ := Fingerprint("t1", "smart", a)
	if fa != fb {
		t.Error("model name and stream flag must not change the fingerprint")
	}
	if fa == fc || fa == fd || fa == fe {
		t.Error("max_tokens, tenant and alias must change the fingerprint")
	}
	// Raw bodies: key order does not matter, unmodelled fields do.
	r1 := req(t, `{"max_tokens":5,"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	r1.Raw = []byte(`{"max_tokens":5,"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	r2 := req(t, `{"model":"x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	r2.Raw = []byte(`{"model":"x","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	r3 := req(t, `{"model":"x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	r3.Raw = []byte(`{"model":"x","max_tokens":5,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`)
	f1, _ := Fingerprint("t", "a", r1)
	f2, _ := Fingerprint("t", "a", r2)
	f3, _ := Fingerprint("t", "a", r3)
	if f1 != f2 || f1 == f3 {
		t.Errorf("raw fingerprints: %v %v %v", f1 == f2, f1 == f3, f3)
	}
}

func TestSemanticInputEligibilityAndScope(t *testing.T) {
	r := req(t, `{"model":"x","max_tokens":5,"system":"s","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"what is 2+2"}]}`)
	text, scope, ok := SemanticInput("t", "a", r)
	if !ok || text != "what is 2+2" || scope == "" {
		t.Fatalf("%q %q %v", text, scope, ok)
	}
	r2 := req(t, `{"model":"x","max_tokens":5,"system":"other","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"what is 2+2"}]}`)
	if _, s2, _ := SemanticInput("t", "a", r2); s2 == scope {
		t.Fatal("a different system prompt must be a different scope")
	}
	for _, body := range []string{
		`{"model":"x","max_tokens":5,"tools":[{"name":"f","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"x","max_tokens":5,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x"}}]}]}`,
		`{"model":"x","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prefill"}]}`,
	} {
		if _, _, ok := SemanticInput("t", "a", req(t, body)); ok {
			t.Errorf("must not be eligible: %s", body)
		}
	}
}

func sample() *llm.Response {
	return &llm.Response{ID: "msg_1", Type: "message", Role: "assistant", Model: "m", StopReason: "tool_use",
		Content: []llm.Block{{Type: "text", Text: "checking"}, {Type: "tool_use", ID: "t1", Name: "f", Input: json.RawMessage(`{"a":1}`)}},
		Usage:   llm.Usage{InputTokens: 4, OutputTokens: 9}}
}

func TestReplayAssemblerRoundTrip(t *testing.T) {
	r := NewReplay(sample())
	a := NewAssembler()
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		a.Add(ev)
	}
	got, ok := a.Response()
	if !ok {
		t.Fatal("not assembled")
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(sample())
	if string(gb) != string(wb) {
		t.Fatalf("round trip\n got %s\nwant %s", gb, wb)
	}
}

func TestAssemblerRefusesThinkingAndIncomplete(t *testing.T) {
	a := NewAssembler()
	a.Add(&llm.Event{Type: llm.EventMessageStart, Message: &llm.Response{ID: "m"}})
	a.Add(&llm.Event{Type: llm.EventContentBlockStart, Index: llm.Idx(0), ContentBlock: &llm.Block{Type: "thinking"}})
	a.Add(&llm.Event{Type: llm.EventMessageDelta, Delta: &llm.Delta{StopReason: "end_turn"}})
	if _, ok := a.Response(); ok {
		t.Fatal("thinking streams are not cached")
	}
	b := NewAssembler()
	b.Add(&llm.Event{Type: llm.EventMessageStart, Message: &llm.Response{ID: "m"}})
	if _, ok := b.Response(); ok {
		t.Fatal("a stream without a stop reason was cut short")
	}
}

func TestRedisExactTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	c := NewRedisExact(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "c:")
	ctx := context.Background()
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("empty cache hit")
	}
	_ = c.Set(ctx, "k", sample(), time.Minute)
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || got.Content[1].Name != "f" {
		t.Fatalf("%v %v %v", got, ok, err)
	}
	mr.FastForward(2 * time.Minute)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("expired entry returned")
	}
}

func TestMemoryVectorsNearestInScope(t *testing.T) {
	m := NewMemoryVectors()
	ctx := context.Background()
	_ = m.Put(ctx, "s1", []float32{1, 0, 0}, &llm.Response{ID: "x"}, time.Minute)
	_ = m.Put(ctx, "s1", []float32{0, 1, 0}, &llm.Response{ID: "y"}, time.Minute)
	_ = m.Put(ctx, "s2", []float32{0.9, 0.1, 0}, &llm.Response{ID: "other-scope"}, time.Minute)
	r, sim, ok, _ := m.Nearest(ctx, "s1", []float32{0.95, 0.05, 0})
	if !ok || r.ID != "x" || sim < 0.99 {
		t.Fatalf("%v %v %v", r, sim, ok)
	}
	if _, _, ok, _ := m.Nearest(ctx, "s3", []float32{1, 0, 0}); ok {
		t.Fatal("empty scope hit")
	}
}
