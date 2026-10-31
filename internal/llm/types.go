// Package llm holds the gateway's canonical request, response and stream event
// types. They follow the Anthropic Messages API shape, which is the richer of the
// two formats switchyard speaks: content blocks, tool_use/tool_result pairs and
// typed stream events. The OpenAI surface and OpenAI-compatible upstreams are
// translated to and from these types in package translate.
package llm

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Request is a Messages API request.
type Request struct {
	Model         string          `json:"model"`
	System        []Block         `json:"-"`
	Messages      []Message       `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`

	// Sampling parameters. The Anthropic API (version 2026-10-21) rejects them,
	// so the Anthropic adapter drops them; OpenAI-compatible upstreams receive them.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	// Raw is the original body when the request arrived on the Anthropic
	// surface. The Anthropic adapter forwards it (with the model rewritten) so
	// fields the gateway does not model, such as thinking or output_config,
	// survive the round trip.
	Raw json.RawMessage `json:"-"`
	// Betas are anthropic-beta header values from the client, forwarded to
	// Anthropic upstreams only.
	Betas []string `json:"-"`
}

type requestAlias Request

type requestWire struct {
	*requestAlias
	System json.RawMessage `json:"system,omitempty"`
}

// UnmarshalJSON accepts system as a string or a list of text blocks.
func (r *Request) UnmarshalJSON(b []byte) error {
	w := requestWire{requestAlias: (*requestAlias)(r)}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	sys, err := parseBlocks(w.System)
	if err != nil {
		return errors.New("system: " + err.Error())
	}
	r.System = sys
	return nil
}

// MarshalJSON writes system as a list of blocks.
func (r Request) MarshalJSON() ([]byte, error) {
	w := requestWire{requestAlias: (*requestAlias)(&r)}
	if len(r.System) > 0 {
		s, err := json.Marshal(r.System)
		if err != nil {
			return nil, err
		}
		w.System = s
	}
	return json.Marshal(w)
}

// SystemText joins the system prompt's text blocks.
func (r *Request) SystemText() string {
	return joinText(r.System)
}

// Message is one conversation turn.
type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

// UnmarshalJSON accepts content as a string or a list of blocks.
func (m *Message) UnmarshalJSON(b []byte) error {
	var w struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	blocks, err := parseBlocks(w.Content)
	if err != nil {
		return errors.New("content: " + err.Error())
	}
	m.Role, m.Content = w.Role, blocks
	return nil
}

// Text joins the message's text blocks.
func (m *Message) Text() string { return joinText(m.Content) }

// Block is a content block. Only the fields of its Type are set.
type Block struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result; Content is a string or a list of blocks, kept raw.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// image and document
	Source *Source `json:"source,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	CacheControl json.RawMessage `json:"cache_control,omitempty"`

	// raw is the block as it arrived, re-sent unchanged so block types and
	// fields the gateway does not model (citations, server tool results) survive.
	raw json.RawMessage
}

type blockAlias Block

// UnmarshalJSON keeps the original bytes alongside the parsed fields.
func (b *Block) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, (*blockAlias)(b)); err != nil {
		return err
	}
	b.raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON re-sends a parsed block unchanged, and writes a constructed block
// with exactly the fields its type has on the wire.
func (b Block) MarshalJSON() ([]byte, error) {
	if b.raw != nil {
		return b.raw, nil
	}
	m := map[string]any{"type": b.Type}
	switch b.Type {
	case "text":
		m["text"] = b.Text
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		m["id"], m["name"], m["input"] = b.ID, b.Name, input
	case "tool_result":
		m["tool_use_id"] = b.ToolUseID
		if len(b.Content) > 0 {
			m["content"] = b.Content
		}
		if b.IsError {
			m["is_error"] = true
		}
	case "image", "document":
		m["source"] = b.Source
	case "thinking":
		m["thinking"], m["signature"] = b.Thinking, b.Signature
	default:
		return json.Marshal((blockAlias)(b))
	}
	if len(b.CacheControl) > 0 {
		m["cache_control"] = b.CacheControl
	}
	return json.Marshal(m)
}

// Source is an image or document source.
type Source struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ToolResultText flattens a tool_result's content to text.
func (b *Block) ToolResultText() string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var blocks []Block
	if json.Unmarshal(b.Content, &blocks) == nil {
		return joinText(blocks)
	}
	return string(b.Content)
}

// Tool is a client tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolChoice is auto, any, tool or none.
type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// Response is a complete (non-streamed) message.
type Response struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []Block `json:"content"`
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        Usage   `json:"usage"`
}

type responseAlias Response

// MarshalJSON writes an unset stop_reason as null, as the API does in
// message_start.
func (r Response) MarshalJSON() ([]byte, error) {
	var stop any
	if r.StopReason != "" {
		stop = r.StopReason
	}
	if r.Content == nil {
		r.Content = []Block{}
	}
	return json.Marshal(struct {
		responseAlias
		StopReason any `json:"stop_reason"`
	}{responseAlias(r), stop})
}

// UnmarshalJSON accepts a null stop_reason.
func (r *Response) UnmarshalJSON(b []byte) error {
	var w struct {
		*responseAlias
		StopReason *string `json:"stop_reason"`
	}
	w.responseAlias = (*responseAlias)(r)
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if w.StopReason != nil {
		r.StopReason = *w.StopReason
	}
	return nil
}

// Usage is what a provider reports. InputTokens excludes cache reads and writes,
// as on the Anthropic API.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
	u.CacheCreationInputTokens += u2.CacheCreationInputTokens
	u.CacheReadInputTokens += u2.CacheReadInputTokens
}

// Event is one Messages API stream event.
type Event struct {
	Type         string    `json:"type"`
	Message      *Response `json:"message,omitempty"`
	Index        *int      `json:"index,omitempty"`
	ContentBlock *Block    `json:"content_block,omitempty"`
	Delta        *Delta    `json:"delta,omitempty"`
	Usage        *Usage    `json:"usage,omitempty"`
	Error        *APIError `json:"error,omitempty"`

	// Raw is the event's data as an Anthropic upstream sent it; the Anthropic
	// surface relays it unchanged.
	Raw []byte `json:"-"`
}

// Delta is the delta of a content_block_delta or message_delta event.
type Delta struct {
	Type         string  `json:"type,omitempty"`
	Text         string  `json:"text,omitempty"`
	PartialJSON  string  `json:"partial_json,omitempty"`
	Thinking     string  `json:"thinking,omitempty"`
	Signature    string  `json:"signature,omitempty"`
	StopReason   string  `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`

	raw json.RawMessage
}

type deltaAlias Delta

// UnmarshalJSON keeps the original bytes so unmodelled delta types pass through.
func (d *Delta) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, (*deltaAlias)(d)); err != nil {
		return err
	}
	d.raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON re-sends a parsed delta unchanged.
func (d Delta) MarshalJSON() ([]byte, error) {
	if d.raw != nil {
		return d.raw, nil
	}
	if d.Type == "text_delta" {
		return json.Marshal(map[string]string{"type": d.Type, "text": d.Text})
	}
	if d.Type == "input_json_delta" {
		return json.Marshal(map[string]string{"type": d.Type, "partial_json": d.PartialJSON})
	}
	if d.Type == "" { // message_delta
		return json.Marshal(struct {
			StopReason   string  `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		}{d.StopReason, d.StopSequence})
	}
	return json.Marshal((deltaAlias)(d))
}

// APIError is the error object of the Anthropic error envelope.
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Stream event types.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// Idx returns a pointer to i, for Event.Index.
func Idx(i int) *int { return &i }

// IsContent reports whether e carries generated content, i.e. whether a client
// that received it has seen tokens. Retries and fallbacks are only allowed
// before the first such event.
func (e *Event) IsContent() bool {
	switch e.Type {
	case EventContentBlockDelta:
		return true
	case EventContentBlockStart:
		// A tool_use start already names the tool the model chose.
		return e.ContentBlock != nil && e.ContentBlock.Type == "tool_use"
	}
	return false
}

func parseBlocks(raw json.RawMessage) ([]Block, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []Block{{Type: "text", Text: s}}, nil
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func joinText(blocks []Block) string {
	var buf bytes.Buffer
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(b.Text)
	}
	return buf.String()
}
