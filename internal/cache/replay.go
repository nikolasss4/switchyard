package cache

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/nikolasss4/switchyard/internal/llm"
)

// Replay is a stream of a cached response, for streaming requests that hit.
type Replay struct {
	events []*llm.Event
}

// NewReplay turns a response into the event sequence a provider would send.
func NewReplay(r *llm.Response) *Replay {
	start := *r
	start.Content, start.StopReason, start.StopSequence = []llm.Block{}, "", nil
	start.Usage = llm.Usage{InputTokens: r.Usage.InputTokens, CacheReadInputTokens: r.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: r.Usage.CacheCreationInputTokens}
	evs := []*llm.Event{{Type: llm.EventMessageStart, Message: &start}}
	for i, b := range r.Content {
		switch b.Type {
		case "text":
			evs = append(evs,
				&llm.Event{Type: llm.EventContentBlockStart, Index: llm.Idx(i), ContentBlock: &llm.Block{Type: "text"}},
				&llm.Event{Type: llm.EventContentBlockDelta, Index: llm.Idx(i), Delta: &llm.Delta{Type: "text_delta", Text: b.Text}})
		case "tool_use":
			evs = append(evs,
				&llm.Event{Type: llm.EventContentBlockStart, Index: llm.Idx(i), ContentBlock: &llm.Block{Type: "tool_use", ID: b.ID, Name: b.Name, Input: json.RawMessage("{}")}},
				&llm.Event{Type: llm.EventContentBlockDelta, Index: llm.Idx(i), Delta: &llm.Delta{Type: "input_json_delta", PartialJSON: string(b.Input)}})
		default:
			continue
		}
		evs = append(evs, &llm.Event{Type: llm.EventContentBlockStop, Index: llm.Idx(i)})
	}
	u := r.Usage
	evs = append(evs,
		&llm.Event{Type: llm.EventMessageDelta, Delta: &llm.Delta{StopReason: r.StopReason, StopSequence: r.StopSequence}, Usage: &llm.Usage{OutputTokens: u.OutputTokens}},
		&llm.Event{Type: llm.EventMessageStop})
	return &Replay{events: evs}
}

func (r *Replay) Next() (*llm.Event, error) {
	if len(r.events) == 0 {
		return nil, io.EOF
	}
	ev := r.events[0]
	r.events = r.events[1:]
	return ev, nil
}

func (r *Replay) Close() error { return nil }

// Assembler rebuilds a complete response from stream events, so a streamed
// miss can be stored in the cache.
type Assembler struct {
	resp    *llm.Response
	text    map[int]*strings.Builder
	json    map[int]*strings.Builder
	order   []int
	blocks  map[int]*llm.Block
	invalid bool
}

// NewAssembler returns an empty assembler.
func NewAssembler() *Assembler {
	return &Assembler{text: map[int]*strings.Builder{}, json: map[int]*strings.Builder{}, blocks: map[int]*llm.Block{}}
}

// Add feeds one event.
func (a *Assembler) Add(e *llm.Event) {
	switch e.Type {
	case llm.EventMessageStart:
		if e.Message != nil {
			m := *e.Message
			a.resp = &m
		}
	case llm.EventContentBlockStart:
		if e.Index == nil || e.ContentBlock == nil {
			a.invalid = true
			return
		}
		switch e.ContentBlock.Type {
		case "text", "tool_use":
		default:
			a.invalid = true // thinking, server tools: not worth replaying faithfully
		}
		b := llm.Block{Type: e.ContentBlock.Type, ID: e.ContentBlock.ID, Name: e.ContentBlock.Name}
		a.blocks[*e.Index] = &b
		a.order = append(a.order, *e.Index)
		a.text[*e.Index], a.json[*e.Index] = &strings.Builder{}, &strings.Builder{}
	case llm.EventContentBlockDelta:
		if e.Index == nil || e.Delta == nil || a.text[*e.Index] == nil {
			a.invalid = true
			return
		}
		a.text[*e.Index].WriteString(e.Delta.Text)
		a.json[*e.Index].WriteString(e.Delta.PartialJSON)
	case llm.EventMessageDelta:
		if a.resp != nil && e.Delta != nil {
			a.resp.StopReason, a.resp.StopSequence = e.Delta.StopReason, e.Delta.StopSequence
		}
		if a.resp != nil && e.Usage != nil {
			a.resp.Usage.OutputTokens = e.Usage.OutputTokens
			if e.Usage.InputTokens > 0 {
				a.resp.Usage.InputTokens = e.Usage.InputTokens
			}
		}
	}
}

// Response returns the assembled response, or false if the stream had block
// types the cache does not replay.
func (a *Assembler) Response() (*llm.Response, bool) {
	if a.resp == nil || a.invalid || a.resp.StopReason == "" {
		return nil, false
	}
	out := *a.resp
	out.Content = nil
	for _, i := range a.order {
		b := *a.blocks[i]
		switch b.Type {
		case "text":
			b.Text = a.text[i].String()
		case "tool_use":
			in := a.json[i].String()
			if in == "" {
				in = "{}"
			}
			if !json.Valid([]byte(in)) {
				return nil, false
			}
			b.Input = json.RawMessage(in)
		}
		out.Content = append(out.Content, b)
	}
	return &out, true
}
