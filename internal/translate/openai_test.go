package translate

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
)

func mustOpenAI(t *testing.T, s string) *oai.ChatRequest {
	t.Helper()
	var r oai.ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestFromOpenAISystemAndToolTurns(t *testing.T) {
	in := mustOpenAI(t, `{
	  "model": "smart", "max_tokens": 50, "temperature": 0.2,
	  "messages": [
	    {"role": "system", "content": "be brief"},
	    {"role": "developer", "content": [{"type":"text","text":"use tools"}]},
	    {"role": "user", "content": "weather in paris and rome?"},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id":"c1","type":"function","function":{"name":"w","arguments":"{\"city\":\"paris\"}"}},
	      {"id":"c2","type":"function","function":{"name":"w","arguments":"{\"city\":\"rome\"}"}}]},
	    {"role": "tool", "tool_call_id": "c1", "content": "sunny"},
	    {"role": "tool", "tool_call_id": "c2", "content": "rain"}
	  ],
	  "tools": [{"type":"function","function":{"name":"w","parameters":{"type":"object"}}}],
	  "tool_choice": "required", "parallel_tool_calls": false
	}`)
	out, err := FromOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.SystemText() != "be brief\nuse tools" {
		t.Fatalf("system = %q", out.SystemText())
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want user, assistant, user turns; got %d", len(out.Messages))
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 2 || asst.Content[1].Type != "tool_use" || string(asst.Content[1].Input) != `{"city":"rome"}` {
		t.Fatalf("assistant turn = %+v", asst)
	}
	results := out.Messages[2]
	if len(results.Content) != 2 || results.Content[0].ToolUseID != "c1" || results.Content[1].ToolResultText() != "rain" {
		t.Fatalf("tool results must merge into one user turn: %+v", results)
	}
	if out.ToolChoice.Type != "any" || !out.ToolChoice.DisableParallelToolUse {
		t.Fatalf("tool_choice = %+v", out.ToolChoice)
	}
	if out.MaxTokens != 50 || *out.Temperature != 0.2 {
		t.Fatalf("max_tokens %d temperature %v", out.MaxTokens, out.Temperature)
	}
}

func TestFromOpenAIDefaultsAndErrors(t *testing.T) {
	out, err := FromOpenAI(mustOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || out.MaxTokens != DefaultMaxTokens {
		t.Fatalf("default max_tokens: %v %v", out, err)
	}
	cases := map[string]string{
		"bad role":      `{"model":"m","messages":[{"role":"robot","content":"hi"}]}`,
		"no messages":   `{"model":"m","messages":[{"role":"system","content":"hi"}]}`,
		"bad args":      `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"x","function":{"name":"f","arguments":"{nope"}}]}]}`,
		"n>1":           `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`,
		"bad image url": `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"file:///etc/passwd"}}]}]}`,
	}
	for name, body := range cases {
		if _, err := FromOpenAI(mustOpenAI(t, body)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestImageDataURL(t *testing.T) {
	out, err := FromOpenAI(mustOpenAI(t, `{"model":"m","messages":[{"role":"user","content":[
	  {"type":"text","text":"what is this"},
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0K"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	src := out.Messages[0].Content[1].Source
	if src.Type != "base64" || src.MediaType != "image/png" || src.Data != "iVBORw0K" {
		t.Fatalf("source = %+v", src)
	}
	back := ToOpenAIRequest(out, "")
	var parts []oai.Part
	_ = json.Unmarshal(back.Messages[0].Content, &parts)
	if parts[1].ImageURL.URL != "data:image/png;base64,iVBORw0K" {
		t.Fatalf("round trip = %+v", parts)
	}
}

func TestStopReasonMapping(t *testing.T) {
	for anth, oa := range map[string]string{"end_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter", "stop_sequence": "stop"} {
		if got := StopToOpenAI(anth); got != oa {
			t.Errorf("StopToOpenAI(%s) = %s", anth, got)
		}
	}
	for oa, anth := range map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "refusal"} {
		if got := StopFromOpenAI(oa); got != anth {
			t.Errorf("StopFromOpenAI(%s) = %s", oa, got)
		}
	}
}

func TestUsageCachedTokens(t *testing.T) {
	u := llm.Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 90}
	o := UsageToOpenAI(u)
	if o.PromptTokens != 100 || o.PromptTokensDetails.CachedTokens != 90 || o.TotalTokens != 105 {
		t.Fatalf("to openai = %+v", o)
	}
	if back := UsageFromOpenAI(o); back.InputTokens != 10 || back.CacheReadInputTokens != 90 {
		t.Fatalf("from openai = %+v", back)
	}
}

func TestToOpenAIRequestToolRoundTrip(t *testing.T) {
	req := &llm.Request{
		Model: "m", MaxTokens: 10,
		System: []llm.Block{{Type: "text", Text: "sys"}},
		Messages: []llm.Message{
			{Role: "user", Content: []llm.Block{{Type: "text", Text: "q"}}},
			{Role: "assistant", Content: []llm.Block{{Type: "tool_use", ID: "t1", Name: "f", Input: json.RawMessage(`{"a":1}`)}}},
			{Role: "user", Content: []llm.Block{{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`[{"type":"text","text":"42"}]`)}}},
		},
		ToolChoice: &llm.ToolChoice{Type: "tool", Name: "f"},
	}
	out := ToOpenAIRequest(req, "max_completion_tokens")
	if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != 10 || out.MaxTokens != nil {
		t.Fatal("max_completion_tokens not used")
	}
	roles := []string{}
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	if want := "system user assistant tool"; join(roles) != want {
		t.Fatalf("roles = %v", roles)
	}
	if out.Messages[2].ToolCalls[0].Function.Arguments != `{"a":1}` || string(out.Messages[3].Content) != `"42"` {
		t.Fatalf("tool turn = %+v / %s", out.Messages[2], out.Messages[3].Content)
	}
	if string(out.ToolChoice) != `{"function":{"name":"f"},"type":"function"}` {
		t.Fatalf("tool_choice = %s", out.ToolChoice)
	}
}

func TestFromOpenAIResponseInvalidToolArgs(t *testing.T) {
	finish := "tool_calls"
	resp, err := FromOpenAIResponse(&oai.ChatResponse{ID: "x", Choices: []oai.Choice{{Message: &oai.RespMsg{
		ToolCalls: []oai.ToolCall{{ID: "c", Function: oai.FunctionCall{Name: "f", Arguments: "not json"}}},
	}, FinishReason: &finish}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "tool_use" || string(resp.Content[0].Input) != `{"_raw":"not json"}` {
		t.Fatalf("got %+v", resp.Content[0])
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
