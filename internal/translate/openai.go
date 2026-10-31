// Package translate converts between the OpenAI Chat Completions format and the
// gateway's canonical (Anthropic Messages) format, for whole messages and for
// streams.
package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
)

// DefaultMaxTokens is used when an OpenAI request sets no limit; the Messages
// API requires one.
const DefaultMaxTokens = 4096

// ErrInvalid wraps request validation errors (HTTP 400).
var ErrInvalid = errors.New("invalid request")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// FromOpenAI converts a chat completions request to the canonical format.
// System and developer messages become the system prompt, consecutive tool
// messages become one user turn of tool_result blocks, and assistant tool_calls
// become tool_use blocks.
func FromOpenAI(in *oai.ChatRequest) (*llm.Request, error) {
	if in.N != nil && *in.N > 1 {
		return nil, invalid("n > 1 is not supported")
	}
	out := &llm.Request{
		Model:         in.Model,
		StopSequences: in.Stop,
		Stream:        in.Stream,
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		MaxTokens:     DefaultMaxTokens,
	}
	if in.MaxCompletionTokens != nil {
		out.MaxTokens = *in.MaxCompletionTokens
	} else if in.MaxTokens != nil {
		out.MaxTokens = *in.MaxTokens
	}
	if in.User != "" {
		out.Metadata, _ = json.Marshal(map[string]string{"user_id": in.User})
	}

	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			parts, err := m.Parts()
			if err != nil {
				return nil, invalid("messages[%d]: %v", i, err)
			}
			for _, p := range parts {
				if p.Type == "text" {
					out.System = append(out.System, llm.Block{Type: "text", Text: p.Text})
				}
			}
		case "user":
			blocks, err := userBlocks(&m)
			if err != nil {
				return nil, invalid("messages[%d]: %v", i, err)
			}
			appendTurn(out, "user", blocks)
		case "assistant":
			var blocks []llm.Block
			parts, err := m.Parts()
			if err != nil {
				return nil, invalid("messages[%d]: %v", i, err)
			}
			for _, p := range parts {
				if p.Type == "text" && p.Text != "" {
					blocks = append(blocks, llm.Block{Type: "text", Text: p.Text})
				}
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return nil, invalid("messages[%d]: tool call %s has invalid JSON arguments", i, tc.ID)
				}
				blocks = append(blocks, llm.Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)})
			}
			appendTurn(out, "assistant", blocks)
		case "tool":
			parts, err := m.Parts()
			if err != nil {
				return nil, invalid("messages[%d]: %v", i, err)
			}
			var text strings.Builder
			for _, p := range parts {
				text.WriteString(p.Text)
			}
			content, _ := json.Marshal(text.String())
			appendTurn(out, "user", []llm.Block{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content}})
		default:
			return nil, invalid("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(out.Messages) == 0 {
		return nil, invalid("messages must contain at least one user or assistant message")
	}

	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, invalid("unsupported tool type %q", t.Type)
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, llm.Tool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	tc, err := toolChoiceFromOpenAI(in.ToolChoice)
	if err != nil {
		return nil, err
	}
	if tc != nil && in.ParallelToolCalls != nil && !*in.ParallelToolCalls && tc.Type != "none" {
		tc.DisableParallelToolUse = true
	} else if tc == nil && in.ParallelToolCalls != nil && !*in.ParallelToolCalls && len(out.Tools) > 0 {
		tc = &llm.ToolChoice{Type: "auto", DisableParallelToolUse: true}
	}
	out.ToolChoice = tc
	return out, nil
}

// appendTurn adds blocks as a turn, merging into the previous turn when it has
// the same role (Anthropic expects tool results for parallel calls in one turn).
func appendTurn(r *llm.Request, role string, blocks []llm.Block) {
	if len(blocks) == 0 {
		return
	}
	if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == role {
		r.Messages[n-1].Content = append(r.Messages[n-1].Content, blocks...)
		return
	}
	r.Messages = append(r.Messages, llm.Message{Role: role, Content: blocks})
}

func userBlocks(m *oai.Message) ([]llm.Block, error) {
	parts, err := m.Parts()
	if err != nil {
		return nil, err
	}
	var blocks []llm.Block
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, llm.Block{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				return nil, errors.New("image_url part without a url")
			}
			src, err := imageSource(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, llm.Block{Type: "image", Source: src})
		default:
			return nil, fmt.Errorf("unsupported content part %q", p.Type)
		}
	}
	return blocks, nil
}

// imageSource turns a data: URL into a base64 source and passes http(s) URLs
// through. The gateway never fetches image URLs itself.
func imageSource(url string) (*llm.Source, error) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		mediaType, isB64 := strings.CutSuffix(meta, ";base64")
		if !found || !isB64 {
			return nil, errors.New("image data URLs must be base64")
		}
		return &llm.Source{Type: "base64", MediaType: mediaType, Data: data}, nil
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		return &llm.Source{Type: "url", URL: url}, nil
	}
	return nil, errors.New("image url must be http(s) or a data URL")
}

func toolChoiceFromOpenAI(raw json.RawMessage) (*llm.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return &llm.ToolChoice{Type: "auto"}, nil
		case "none":
			return &llm.ToolChoice{Type: "none"}, nil
		case "required":
			return &llm.ToolChoice{Type: "any"}, nil
		}
		return nil, invalid("unsupported tool_choice %q", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Function.Name == "" {
		return nil, invalid("tool_choice must be a string or {type: function, function: {name}}")
	}
	return &llm.ToolChoice{Type: "tool", Name: obj.Function.Name}, nil
}

// StopToOpenAI maps an Anthropic stop_reason to an OpenAI finish_reason.
func StopToOpenAI(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

// StopFromOpenAI maps an OpenAI finish_reason to an Anthropic stop_reason.
func StopFromOpenAI(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// UsageToOpenAI converts usage; OpenAI prompt_tokens include cached tokens.
func UsageToOpenAI(u llm.Usage) *oai.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out := &oai.Usage{PromptTokens: prompt, CompletionTokens: u.OutputTokens, TotalTokens: prompt + u.OutputTokens}
	if u.CacheReadInputTokens > 0 {
		out.PromptTokensDetails = &oai.TokenDetails{CachedTokens: u.CacheReadInputTokens}
	}
	return out
}

// UsageFromOpenAI converts usage, splitting cached tokens out of input tokens.
func UsageFromOpenAI(u *oai.Usage) llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return llm.Usage{InputTokens: u.PromptTokens - cached, OutputTokens: u.CompletionTokens, CacheReadInputTokens: cached}
}

// ToOpenAIResponse converts a canonical response for an OpenAI client.
// Thinking blocks are dropped; OpenAI chat completions have no place for them.
func ToOpenAIResponse(r *llm.Response, created int64) *oai.ChatResponse {
	msg := &oai.RespMsg{Role: "assistant"}
	var text strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			input := string(b.Input)
			if input == "" {
				input = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, oai.ToolCall{ID: b.ID, Type: "function", Function: oai.FunctionCall{Name: b.Name, Arguments: input}})
		}
	}
	if text.Len() > 0 || len(msg.ToolCalls) == 0 {
		s := text.String()
		msg.Content = &s
	}
	finish := StopToOpenAI(r.StopReason)
	return &oai.ChatResponse{
		ID: openAIID(r.ID), Object: "chat.completion", Created: created, Model: r.Model,
		Choices: []oai.Choice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   UsageToOpenAI(r.Usage),
	}
}

func openAIID(id string) string {
	if strings.HasPrefix(id, "chatcmpl") {
		return id
	}
	return "chatcmpl-" + strings.TrimPrefix(id, "msg_")
}

// ToOpenAIRequest converts a canonical request for an OpenAI-compatible
// upstream. maxTokensField is "max_tokens" or "max_completion_tokens".
func ToOpenAIRequest(r *llm.Request, maxTokensField string) *oai.ChatRequest {
	out := &oai.ChatRequest{Model: r.Model, Stop: r.StopSequences, Stream: r.Stream, Temperature: r.Temperature, TopP: r.TopP}
	mt := r.MaxTokens
	if maxTokensField == "max_completion_tokens" {
		out.MaxCompletionTokens = &mt
	} else {
		out.MaxTokens = &mt
	}
	if r.Stream {
		out.StreamOptions = &oai.StreamOptions{IncludeUsage: true}
	}
	if sys := r.SystemText(); sys != "" {
		out.Messages = append(out.Messages, oai.Message{Role: "system", Content: oai.TextContent(sys)})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			var parts []oai.Part
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					parts = append(parts, oai.Part{Type: "text", Text: b.Text})
				case "image":
					if b.Source == nil {
						continue
					}
					url := b.Source.URL
					if b.Source.Type == "base64" {
						url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
					}
					parts = append(parts, oai.Part{Type: "image_url", ImageURL: &oai.ImageURL{URL: url}})
				case "tool_result":
					out.Messages = append(out.Messages, oai.Message{Role: "tool", ToolCallID: b.ToolUseID, Content: oai.TextContent(b.ToolResultText())})
				}
			}
			if len(parts) == 0 {
				continue
			}
			if len(parts) == 1 && parts[0].Type == "text" {
				out.Messages = append(out.Messages, oai.Message{Role: "user", Content: oai.TextContent(parts[0].Text)})
			} else {
				c, _ := json.Marshal(parts)
				out.Messages = append(out.Messages, oai.Message{Role: "user", Content: c})
			}
		case "assistant":
			msg := oai.Message{Role: "assistant"}
			text := m.Text()
			for _, b := range m.Content {
				if b.Type == "tool_use" {
					input := string(b.Input)
					if input == "" {
						input = "{}"
					}
					msg.ToolCalls = append(msg.ToolCalls, oai.ToolCall{ID: b.ID, Type: "function", Function: oai.FunctionCall{Name: b.Name, Arguments: input}})
				}
			}
			if text != "" || len(msg.ToolCalls) == 0 {
				msg.Content = oai.TextContent(text)
			}
			out.Messages = append(out.Messages, msg)
		}
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, oai.Tool{Type: "function", Function: oai.Function{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}})
	}
	if tc := r.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "none":
			out.ToolChoice, _ = json.Marshal(tc.Type)
		case "any":
			out.ToolChoice, _ = json.Marshal("required")
		case "tool":
			out.ToolChoice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
		}
		if tc.DisableParallelToolUse {
			f := false
			out.ParallelToolCalls = &f
		}
	}
	return out
}

// FromOpenAIResponse converts an OpenAI-compatible upstream response.
func FromOpenAIResponse(in *oai.ChatResponse) (*llm.Response, error) {
	if len(in.Choices) == 0 || in.Choices[0].Message == nil {
		return nil, errors.New("upstream response has no choices")
	}
	c := in.Choices[0]
	out := &llm.Response{ID: in.ID, Type: "message", Role: "assistant", Model: in.Model, Usage: UsageFromOpenAI(in.Usage)}
	if c.Message.Content != nil && *c.Message.Content != "" {
		out.Content = append(out.Content, llm.Block{Type: "text", Text: *c.Message.Content})
	}
	for _, tc := range c.Message.ToolCalls {
		out.Content = append(out.Content, llm.Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments)})
	}
	if out.Content == nil {
		out.Content = []llm.Block{{Type: "text", Text: ""}}
	}
	finish := ""
	if c.FinishReason != nil {
		finish = *c.FinishReason
	}
	out.StopReason = StopFromOpenAI(finish)
	return out, nil
}

// toolInput returns arguments as a JSON object. Some local servers emit invalid
// JSON; the text is kept under "_raw" rather than dropped.
func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(args)) && strings.HasPrefix(args, "{") {
		return json.RawMessage(args)
	}
	b, _ := json.Marshal(map[string]string{"_raw": args})
	return b
}
