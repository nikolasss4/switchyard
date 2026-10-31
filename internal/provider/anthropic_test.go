package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nikolasss4/switchyard/internal/llm"
)

func TestAnthropicForwardsRawBodyAndHeaders(t *testing.T) {
	var got map[string]json.RawMessage
	var header http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"hi","citations":null}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer ts.Close()

	raw := []byte(`{"model":"fast","max_tokens":9,"temperature":0.5,"top_k":3,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`)
	var req llm.Request
	_ = json.Unmarshal(raw, &req)
	req.Raw, req.Model, req.Betas = raw, "claude-haiku-5-5", []string{"b1", "b2"}

	p := NewAnthropic(AnthropicConfig{Name: "a", BaseURL: ts.URL + "/v1", APIKey: "sk-test"}, ts.Client())
	resp, err := p.Complete(context.Background(), &req)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"claude-haiku-5-5"` || string(got["thinking"]) != `{"type":"adaptive"}` || string(got["output_config"]) != `{"effort":"low"}` {
		t.Fatalf("body = %v", got)
	}
	for _, k := range []string{"temperature", "top_k", "stream"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must be dropped", k)
		}
	}
	if header.Get("x-api-key") != "sk-test" || header.Get("anthropic-version") != AnthropicVersion || header.Get("anthropic-beta") != "b1,b2" {
		t.Fatalf("headers = %v", header)
	}
	// Unmodelled block fields come back untouched.
	out, _ := json.Marshal(resp.Content[0])
	if string(out) != `{"type":"text","text":"hi","citations":null}` {
		t.Fatalf("block = %s", out)
	}
}

func TestAnthropicErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	}))
	defer ts.Close()
	p := NewAnthropic(AnthropicConfig{Name: "a", BaseURL: ts.URL}, ts.Client())
	_, err := p.Complete(context.Background(), &llm.Request{Model: "m", MaxTokens: 1})
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 529 || pe.Type != "overloaded_error" || !pe.Retryable() || pe.RetryAfter.Seconds() != 3 {
		t.Fatalf("err = %#v", err)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer ts.Close()
	p := NewAnthropic(AnthropicConfig{Name: "a", BaseURL: ts.URL}, ts.Client())
	s, err := p.Stream(context.Background(), &llm.Request{Model: "m", MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ev, err := s.Next(); err != nil || ev.Type != llm.EventMessageStart {
		t.Fatalf("first = %v %v", ev, err)
	}
	_, err = s.Next()
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 529 || !pe.Retryable() {
		t.Fatalf("want retryable 529, got %v", err)
	}
}
