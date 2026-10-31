package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/mock"
	"github.com/nikolasss4/switchyard/internal/sse"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A client that gives up must cancel the upstream request too.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	h := newHarness(t)
	cfg := mock.DefaultConfig()
	cfg.LatencyMS = 3000
	h.mock.SetConfig(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.gateway.URL+"/v1/chat/completions", strings.NewReader(hello))
	req.Header.Set("Authorization", "Bearer "+h.key)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("expected the client to time out")
	}
	waitFor(t, "upstream cancellation", func() bool { return h.mock.Stats().Cancelled == 1 })
	waitFor(t, "usage record", func() bool { return len(h.usage.Records()) == 1 })
	if st := h.usage.Records()[0].Status; st != 499 {
		t.Fatalf("status recorded %d, want 499", st)
	}
}

func TestClientDisconnectMidStreamCancelsUpstream(t *testing.T) {
	h := newHarness(t)
	cfg := mock.DefaultConfig()
	cfg.TokensPerSecond, cfg.OutputTokens = 50, 500 // a 10 s stream
	h.mock.SetConfig(cfg)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		before := h.mock.Stats().Cancelled
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.gateway.URL+"/v1/messages",
			strings.NewReader(`{"model":"`+model+`","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("x-api-key", h.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r := sse.NewReader(resp.Body)
		for range 5 { // read a few events, then hang up
			if _, err := r.Next(); err != nil {
				t.Fatal(err)
			}
		}
		cancel()
		resp.Body.Close()
		waitFor(t, model+" upstream cancellation", func() bool { return h.mock.Stats().Cancelled == before+1 })
	}
}
