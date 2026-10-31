package mock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/sse"
)

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatCompletion(t *testing.T) {
	ts := httptest.NewServer(New("m", DefaultConfig()))
	defer ts.Close()
	resp := post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi there"}],"max_tokens":5}`)
	defer resp.Body.Close()
	var out oai.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if *out.Choices[0].FinishReason != "length" || out.Usage.CompletionTokens != 5 || out.Usage.PromptTokens != 2 {
		t.Fatalf("unexpected response %+v usage %+v", out.Choices[0], out.Usage)
	}
}

func TestMessagesStreamIsWellFormed(t *testing.T) {
	ts := httptest.NewServer(New("m", DefaultConfig()))
	defer ts.Close()
	resp := post(t, ts.URL+"/v1/messages", `{"model":"m","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	defer resp.Body.Close()
	r := sse.NewReader(resp.Body)
	var types []string
	var text strings.Builder
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var e llm.Event
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			t.Fatal(err)
		}
		if e.Type != ev.Name {
			t.Fatalf("event name %q != type %q", ev.Name, e.Type)
		}
		types = append(types, e.Type)
		if e.Delta != nil {
			text.WriteString(e.Delta.Text)
		}
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("bad event order %v", types)
	}
	if got := len(strings.Fields(text.String())); got != 32 {
		t.Fatalf("streamed %d words, want 32", got)
	}
}

func TestMessagesToolUse(t *testing.T) {
	ts := httptest.NewServer(New("m", DefaultConfig()))
	defer ts.Close()
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"weather?"}],
	  "tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
	resp := post(t, ts.URL+"/v1/messages", body)
	defer resp.Body.Close()
	var out llm.Response
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.StopReason != "tool_use" || out.Content[0].Name != "get_weather" || string(out.Content[0].Input) != `{"city":"mock"}` {
		t.Fatalf("unexpected %+v", out)
	}
}

func TestInjectedErrorsAndRateLimit(t *testing.T) {
	m := New("m", Config{ErrorRate: 1, ErrorStatus: 503})
	ts := httptest.NewServer(m)
	defer ts.Close()
	resp := post(t, ts.URL+"/v1/messages", `{"model":"m","max_tokens":1,"messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	m.SetConfig(Config{RPM: 1})
	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`).Body.Close()
	resp = post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("want 429 with Retry-After, got %d", resp.StatusCode)
	}
}

func TestDownDropsConnection(t *testing.T) {
	ts := httptest.NewServer(New("m", Config{Down: true}))
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err == nil {
		resp.Body.Close()
		t.Fatal("want a connection error")
	}
}

func TestClientCancelIsCounted(t *testing.T) {
	m := New("m", Config{LatencyMS: 2000})
	ts := httptest.NewServer(m)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"m","max_tokens":1,"messages":[]}`))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	deadline := time.Now().Add(time.Second)
	for m.Stats().Cancelled == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.Stats().Cancelled != 1 {
		t.Fatalf("stats %+v", m.Stats())
	}
}

func TestHashEmbeddingSimilarity(t *testing.T) {
	cos := func(a, b []float32) float64 {
		var s float64
		for i := range a {
			s += float64(a[i] * b[i])
		}
		return s
	}
	a := HashEmbedding("what is the capital of france", 256)
	b := HashEmbedding("What is the capital of France?", 256)
	c := HashEmbedding("how do i bake sourdough bread", 256)
	if cos(a, b) < 0.99 || cos(a, c) > 0.5 {
		t.Fatalf("cos(a,b)=%.2f cos(a,c)=%.2f", cos(a, b), cos(a, c))
	}
}
