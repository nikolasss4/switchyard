package translate

import (
	"encoding/json"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
)

// OpenAIStreamReader turns OpenAI chat completion chunks into canonical stream
// events. OpenAI streams have no block boundaries, so it opens a text block on
// the first content delta and a tool_use block whenever a new tool call index
// appears, closing the previous block each time. Usage arrives in a final chunk
// after finish_reason, so message_delta is only emitted by Finish.
type OpenAIStreamReader struct {
	started   bool
	open      bool // a content block is open
	openType  string
	index     int // index of the open (or next) block
	toolBlock map[int]int
	stop      string
	usage     llm.Usage
	id, model string
}

// NewOpenAIStreamReader returns a reader for one stream.
func NewOpenAIStreamReader() *OpenAIStreamReader {
	return &OpenAIStreamReader{toolBlock: map[int]int{}, index: -1}
}

// Chunk converts one chunk into zero or more events.
func (s *OpenAIStreamReader) Chunk(c *oai.Chunk) []*llm.Event {
	var out []*llm.Event
	if !s.started {
		s.started, s.id, s.model = true, c.ID, c.Model
		out = append(out, &llm.Event{Type: llm.EventMessageStart, Message: &llm.Response{
			ID: c.ID, Type: "message", Role: "assistant", Model: c.Model, Content: []llm.Block{},
		}})
	}
	if c.Usage != nil {
		s.usage = UsageFromOpenAI(c.Usage)
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		if d := ch.Delta; d != nil {
			if d.Content != nil && *d.Content != "" {
				if !s.open || s.openType != "text" {
					out = append(out, s.closeBlock()...)
					out = append(out, s.openBlock(&llm.Block{Type: "text"}))
				}
				out = append(out, &llm.Event{Type: llm.EventContentBlockDelta, Index: llm.Idx(s.index), Delta: &llm.Delta{Type: "text_delta", Text: *d.Content}})
			}
			for _, tc := range d.ToolCalls {
				ti := 0
				if tc.Index != nil {
					ti = *tc.Index
				}
				bi, seen := s.toolBlock[ti]
				if !seen {
					out = append(out, s.closeBlock()...)
					out = append(out, s.openBlock(&llm.Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage("{}")}))
					bi = s.index
					s.toolBlock[ti] = bi
				}
				if tc.Function.Arguments != "" && bi == s.index {
					out = append(out, &llm.Event{Type: llm.EventContentBlockDelta, Index: llm.Idx(bi), Delta: &llm.Delta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments}})
				}
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.stop = StopFromOpenAI(*ch.FinishReason)
			out = append(out, s.closeBlock()...)
		}
	}
	return out
}

// Finish closes the message after the last chunk.
func (s *OpenAIStreamReader) Finish() []*llm.Event {
	if !s.started {
		return nil
	}
	out := s.closeBlock()
	stop := s.stop
	if stop == "" {
		stop = "end_turn"
	}
	u := s.usage
	out = append(out,
		&llm.Event{Type: llm.EventMessageDelta, Delta: &llm.Delta{StopReason: stop}, Usage: &u},
		&llm.Event{Type: llm.EventMessageStop},
	)
	s.started = false
	return out
}

func (s *OpenAIStreamReader) openBlock(b *llm.Block) *llm.Event {
	s.index++
	s.open, s.openType = true, b.Type
	return &llm.Event{Type: llm.EventContentBlockStart, Index: llm.Idx(s.index), ContentBlock: b}
}

func (s *OpenAIStreamReader) closeBlock() []*llm.Event {
	if !s.open {
		return nil
	}
	s.open = false
	return []*llm.Event{{Type: llm.EventContentBlockStop, Index: llm.Idx(s.index)}}
}

// OpenAIStreamWriter turns canonical events into OpenAI chat completion chunks
// for an OpenAI client.
type OpenAIStreamWriter struct {
	id, model    string
	created      int64
	includeUsage bool
	toolIndex    map[int]int // canonical block index -> OpenAI tool_calls index
	usage        llm.Usage
}

// NewOpenAIStreamWriter returns a writer; includeUsage mirrors the client's
// stream_options.include_usage.
func NewOpenAIStreamWriter(created int64, includeUsage bool) *OpenAIStreamWriter {
	return &OpenAIStreamWriter{created: created, includeUsage: includeUsage, toolIndex: map[int]int{}}
}

// Usage is the usage accumulated from message_start and message_delta.
func (w *OpenAIStreamWriter) Usage() llm.Usage { return w.usage }

// Event converts one canonical event into zero or more chunks. done is true
// after message_stop; the caller then sends "data: [DONE]".
func (w *OpenAIStreamWriter) Event(e *llm.Event) (chunks []*oai.Chunk, done bool) {
	switch e.Type {
	case llm.EventMessageStart:
		if e.Message != nil {
			w.id, w.model = openAIID(e.Message.ID), e.Message.Model
			w.usage = e.Message.Usage
		}
		empty := ""
		return []*oai.Chunk{w.chunk(&oai.RespMsg{Role: "assistant", Content: &empty}, nil)}, false
	case llm.EventContentBlockStart:
		if b := e.ContentBlock; b != nil && b.Type == "tool_use" && e.Index != nil {
			ti := len(w.toolIndex)
			w.toolIndex[*e.Index] = ti
			return []*oai.Chunk{w.chunk(&oai.RespMsg{ToolCalls: []oai.ToolCall{{
				Index: llm.Idx(ti), ID: b.ID, Type: "function", Function: oai.FunctionCall{Name: b.Name, Arguments: ""},
			}}}, nil)}, false
		}
	case llm.EventContentBlockDelta:
		d := e.Delta
		if d == nil {
			return nil, false
		}
		switch d.Type {
		case "text_delta":
			text := d.Text
			return []*oai.Chunk{w.chunk(&oai.RespMsg{Content: &text}, nil)}, false
		case "input_json_delta":
			if e.Index == nil {
				return nil, false
			}
			ti, ok := w.toolIndex[*e.Index]
			if !ok {
				return nil, false
			}
			return []*oai.Chunk{w.chunk(&oai.RespMsg{ToolCalls: []oai.ToolCall{{
				Index: llm.Idx(ti), Function: oai.FunctionCall{Arguments: d.PartialJSON},
			}}}, nil)}, false
		}
	case llm.EventMessageDelta:
		if e.Usage != nil {
			// message_delta usage is cumulative for output; keep input from message_start.
			w.usage.OutputTokens = e.Usage.OutputTokens
			if e.Usage.InputTokens > 0 {
				w.usage.InputTokens = e.Usage.InputTokens
			}
		}
		if e.Delta != nil && e.Delta.StopReason != "" {
			finish := StopToOpenAI(e.Delta.StopReason)
			return []*oai.Chunk{w.chunk(&oai.RespMsg{}, &finish)}, false
		}
	case llm.EventMessageStop:
		if w.includeUsage {
			c := w.chunk(nil, nil)
			c.Choices = []oai.Choice{}
			c.Usage = UsageToOpenAI(w.usage)
			return []*oai.Chunk{c}, true
		}
		return nil, true
	}
	return nil, false
}

func (w *OpenAIStreamWriter) chunk(delta *oai.RespMsg, finish *string) *oai.Chunk {
	c := &oai.Chunk{ID: w.id, Object: "chat.completion.chunk", Created: w.created, Model: w.model}
	if delta != nil {
		c.Choices = []oai.Choice{{Index: 0, Delta: delta, FinishReason: finish}}
	}
	return c
}
