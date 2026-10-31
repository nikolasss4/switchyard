package server

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/sse"
)

const weatherTool = `"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`

func TestMessagesBothUpstreams(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		resp := h.post(t, "/v1/messages", `{"model":"`+model+`","max_tokens":5,"system":"s","messages":[{"role":"user","content":"hi"}]}`)
		var out llm.Response
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 200 || out.StopReason != "max_tokens" || out.Usage.OutputTokens != 5 || out.Content[0].Type != "text" {
			t.Fatalf("%s: %d %+v", model, resp.StatusCode, out)
		}
	}
}

func TestMessagesToolUseBothUpstreams(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		resp := h.post(t, "/v1/messages", `{"model":"`+model+`","max_tokens":50,`+weatherTool+`,"messages":[{"role":"user","content":"weather?"}]}`)
		var out llm.Response
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.StopReason != "tool_use" || out.Content[0].Name != "get_weather" || string(out.Content[0].Input) != `{"city":"mock"}` {
			t.Fatalf("%s: %+v", model, out)
		}
	}
}

// collect reads a Messages stream and checks the event grammar.
func collect(t *testing.T, body io.Reader) (types []string, text, toolJSON, stop string) {
	t.Helper()
	r := sse.NewReader(body)
	open := map[int]bool{}
	for {
		raw, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var ev llm.Event
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != raw.Name {
			t.Fatalf("event name %q != type %q", raw.Name, ev.Type)
		}
		types = append(types, ev.Type)
		switch ev.Type {
		case llm.EventContentBlockStart:
			open[*ev.Index] = true
		case llm.EventContentBlockDelta:
			if !open[*ev.Index] {
				t.Fatalf("delta for unopened block %d", *ev.Index)
			}
			text += ev.Delta.Text
			toolJSON += ev.Delta.PartialJSON
		case llm.EventContentBlockStop:
			delete(open, *ev.Index)
		case llm.EventMessageDelta:
			stop = ev.Delta.StopReason
		}
	}
	if len(open) != 0 {
		t.Fatalf("blocks left open: %v", open)
	}
	if types[0] != llm.EventMessageStart || types[len(types)-1] != llm.EventMessageStop {
		t.Fatalf("bad framing: %v", types)
	}
	return types, text, toolJSON, stop
}

func TestMessagesStreamBothUpstreams(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		resp := h.post(t, "/v1/messages", `{"model":"`+model+`","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		_, text, _, stop := collect(t, resp.Body)
		if len(strings.Fields(text)) != 32 || stop != "end_turn" {
			t.Fatalf("%s: text=%q stop=%q", model, text, stop)
		}
	}
}

func TestMessagesStreamToolUseBothUpstreams(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"via-openai", "via-anthropic"} {
		resp := h.post(t, "/v1/messages", `{"model":"`+model+`","max_tokens":100,"stream":true,`+weatherTool+`,"messages":[{"role":"user","content":"weather?"}]}`)
		_, _, toolJSON, stop := collect(t, resp.Body)
		if toolJSON != `{"city":"mock"}` || stop != "tool_use" {
			t.Fatalf("%s: tool json %q stop %q", model, toolJSON, stop)
		}
	}
}

func TestMessagesValidation(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"no max_tokens": `{"model":"via-anthropic","messages":[{"role":"user","content":"x"}]}`,
		"no messages":   `{"model":"via-anthropic","max_tokens":5,"messages":[]}`,
		"system role":   `{"model":"via-anthropic","max_tokens":5,"messages":[{"role":"system","content":"x"}]}`,
		"bad json":      `{"model":`,
	} {
		resp := h.post(t, "/v1/messages", body)
		var env struct {
			Type  string       `json:"type"`
			Error llm.APIError `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		if resp.StatusCode != 400 || env.Type != "error" || env.Error.Type != "invalid_request_error" {
			t.Errorf("%s: %d %+v", name, resp.StatusCode, env)
		}
	}
}
