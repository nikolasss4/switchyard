package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
)

func chunks(t *testing.T, lines ...string) []*oai.Chunk {
	t.Helper()
	var out []*oai.Chunk
	for _, l := range lines {
		var c oai.Chunk
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		out = append(out, &c)
	}
	return out
}

func describe(evs []*llm.Event) string {
	var parts []string
	for _, e := range evs {
		s := e.Type
		if e.Index != nil {
			s += "#" + itoa(*e.Index)
		}
		if e.ContentBlock != nil {
			s += ":" + e.ContentBlock.Type
		}
		if e.Delta != nil {
			s += "(" + e.Delta.Text + e.Delta.PartialJSON + e.Delta.StopReason + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestOpenAIStreamReaderTextThenParallelToolCalls(t *testing.T) {
	r := NewOpenAIStreamReader()
	var evs []*llm.Event
	for _, c := range chunks(t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Let me"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":" check"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"w","arguments":""}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"c\":1}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"w","arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4}}}`,
	) {
		evs = append(evs, r.Chunk(c)...)
	}
	evs = append(evs, r.Finish()...)
	want := "message_start content_block_start#0:text content_block_delta#0(Let me) content_block_delta#0( check) " +
		"content_block_stop#0 content_block_start#1:tool_use content_block_delta#1({\"c\":1}) content_block_stop#1 " +
		"content_block_start#2:tool_use content_block_delta#2({}) content_block_stop#2 message_delta(tool_use) message_stop"
	if got := describe(evs); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	u := evs[len(evs)-2].Usage
	if u.InputTokens != 7 || u.CacheReadInputTokens != 4 || u.OutputTokens != 7 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestOpenAIStreamReaderNoUsageNoFinish(t *testing.T) {
	r := NewOpenAIStreamReader()
	evs := r.Chunk(chunks(t, `{"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`)[0])
	evs = append(evs, r.Finish()...)
	if got := describe(evs); got != "message_start content_block_start#0:text content_block_delta#0(hi) content_block_stop#0 message_delta(end_turn) message_stop" {
		t.Fatal(got)
	}
}

func TestOpenAIStreamWriter(t *testing.T) {
	w := NewOpenAIStreamWriter(100, true)
	evs := []*llm.Event{
		{Type: llm.EventMessageStart, Message: &llm.Response{ID: "msg_abc", Model: "claude-haiku-5-5", Usage: llm.Usage{InputTokens: 9, OutputTokens: 1}}},
		{Type: llm.EventContentBlockStart, Index: llm.Idx(0), ContentBlock: &llm.Block{Type: "thinking"}},
		{Type: llm.EventContentBlockDelta, Index: llm.Idx(0), Delta: &llm.Delta{Type: "thinking_delta", Thinking: "hmm"}},
		{Type: llm.EventContentBlockStop, Index: llm.Idx(0)},
		{Type: llm.EventContentBlockStart, Index: llm.Idx(1), ContentBlock: &llm.Block{Type: "text"}},
		{Type: llm.EventContentBlockDelta, Index: llm.Idx(1), Delta: &llm.Delta{Type: "text_delta", Text: "ok"}},
		{Type: llm.EventContentBlockStart, Index: llm.Idx(2), ContentBlock: &llm.Block{Type: "tool_use", ID: "toolu_1", Name: "f"}},
		{Type: llm.EventContentBlockDelta, Index: llm.Idx(2), Delta: &llm.Delta{Type: "input_json_delta", PartialJSON: `{"a":`}},
		{Type: llm.EventContentBlockDelta, Index: llm.Idx(2), Delta: &llm.Delta{Type: "input_json_delta", PartialJSON: `1}`}},
		{Type: llm.EventMessageDelta, Delta: &llm.Delta{StopReason: "tool_use"}, Usage: &llm.Usage{OutputTokens: 12}},
		{Type: llm.EventMessageStop},
	}
	var out []string
	done := false
	for _, e := range evs {
		cs, d := w.Event(e)
		done = d
		for _, c := range cs {
			b, _ := json.Marshal(c)
			out = append(out, string(b))
		}
	}
	if !done {
		t.Fatal("message_stop must end the stream")
	}
	all := strings.Join(out, "\n")
	for _, want := range []string{
		`"id":"chatcmpl-abc"`,
		`"delta":{"role":"assistant","content":""}`,
		`"delta":{"content":"ok"}`,
		`"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"f","arguments":""}}]`,
		`"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]`,
		`"finish_reason":"tool_calls"`,
		`"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":12,"total_tokens":21}`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in\n%s", want, all)
		}
	}
	if strings.Contains(all, "hmm") {
		t.Error("thinking must not leak into OpenAI chunks")
	}
}
