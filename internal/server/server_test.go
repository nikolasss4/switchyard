package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/mock"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
	"github.com/nikolasss4/switchyard/internal/sse"
)

// harness runs the gateway against one mock, exposed both as an OpenAI-
// compatible provider ("mock-oai") and an Anthropic one ("mock-anth").
type harness struct {
	mock    *mock.Server
	gateway *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	m := mock.New("mock", mock.DefaultConfig())
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)
	cfg, err := config.Parse([]byte(`
providers:
  - {name: mock-oai, type: openai, base_url: ` + upstream.URL + `/v1}
  - {name: mock-anth, type: anthropic, base_url: ` + upstream.URL + `}
models:
  - alias: via-openai
    targets: [{provider: mock-oai, model: m1}]
  - alias: via-anthropic
    targets: [{provider: mock-anth, model: m2}]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(cfg, provider.FromConfig(cfg, provider.NewHTTPClient()))
	gw := httptest.NewServer(New(r, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(gw.Close)
	return &harness{mock: m, gateway: gw}
}

func (h *harness) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(h.gateway.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestChatCompletionsBothUpstreams(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		resp := h.post(t, "/v1/chat/completions", `{"model":"`+model+`","max_tokens":7,"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"}]}`)
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: status %d %s", model, resp.StatusCode, b)
		}
		var out oai.ChatResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if *out.Choices[0].FinishReason != "length" || out.Usage.CompletionTokens != 7 || len(strings.Fields(*out.Choices[0].Message.Content)) != 7 {
			t.Fatalf("%s: %+v %+v", model, out.Choices[0], out.Usage)
		}
	}
}

func TestChatCompletionsStream(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		resp := h.post(t, "/v1/chat/completions", `{"model":"`+model+`","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`)
		r := sse.NewReader(resp.Body)
		var text strings.Builder
		var finish string
		var usage *oai.Usage
		sawDone := false
		for {
			ev, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(ev.Data) == "[DONE]" {
				sawDone = true
				continue
			}
			var c oai.Chunk
			if err := json.Unmarshal(ev.Data, &c); err != nil {
				t.Fatalf("%s: %s", model, ev.Data)
			}
			if c.Usage != nil {
				usage = c.Usage
			}
			for _, ch := range c.Choices {
				if ch.Delta.Content != nil {
					text.WriteString(*ch.Delta.Content)
				}
				if ch.FinishReason != nil {
					finish = *ch.FinishReason
				}
			}
		}
		if !sawDone || finish != "stop" || len(strings.Fields(text.String())) != 32 || usage == nil || usage.CompletionTokens != 32 {
			t.Fatalf("%s: done=%v finish=%q words=%d usage=%+v", model, sawDone, finish, len(strings.Fields(text.String())), usage)
		}
	}
}

func TestUnknownModelIs404(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, "/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEmbeddings(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, "/v1/embeddings", `{"model":"via-openai","input":["a b","c"]}`)
	var out oai.EmbeddingResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 2 || len(out.Data[0].Embedding) != mock.EmbeddingDims || out.Model != "via-openai" {
		t.Fatalf("got %d embeddings, model %q", len(out.Data), out.Model)
	}
}
