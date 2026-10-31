// Package oai holds OpenAI Chat Completions and Embeddings wire types.
package oai

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ChatRequest is a /v1/chat/completions request.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                StringList      `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	N                   *int            `json:"n,omitempty"`
	User                string          `json:"user,omitempty"`
}

// StreamOptions asks for a final usage chunk.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is one chat message. Content is a string, a list of parts, or null.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Part is one element of a list-valued message content.
type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is an http(s) or data: URL.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// Parts decodes Content into parts; a string becomes one text part.
func (m *Message) Parts() ([]Part, error) {
	raw := bytes.TrimSpace(m.Content)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []Part{{Type: "text", Text: s}}, nil
	}
	var parts []Part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("content must be a string or a list of parts")
	}
	return parts, nil
}

// TextContent returns a JSON string for Content.
func TextContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ToolCall is a function call made by the assistant.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"` // stream chunks only
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the function name and its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// Tool is a function tool definition.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function describes a callable function.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatResponse is a non-streamed completion.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion choice.
type Choice struct {
	Index        int      `json:"index"`
	Message      *RespMsg `json:"message,omitempty"`
	Delta        *RespMsg `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

// RespMsg is an assistant message or a stream delta.
type RespMsg struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	Refusal   *string    `json:"refusal,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Usage is OpenAI token usage. PromptTokens includes cached tokens.
type Usage struct {
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	TotalTokens         int           `json:"total_tokens"`
	PromptTokensDetails *TokenDetails `json:"prompt_tokens_details,omitempty"`
}

// TokenDetails breaks down prompt tokens.
type TokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// Chunk is a streamed completion chunk.
type Chunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// EmbeddingRequest is a /v1/embeddings request. Input is a string or list of strings.
type EmbeddingRequest struct {
	Model          string     `json:"model"`
	Input          StringList `json:"input"`
	EncodingFormat string     `json:"encoding_format,omitempty"`
	Dimensions     *int       `json:"dimensions,omitempty"`
	User           string     `json:"user,omitempty"`
}

// EmbeddingResponse is a /v1/embeddings response.
type EmbeddingResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  Usage       `json:"usage"`
}

// Embedding is one embedding vector.
type Embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// ErrorResponse is the OpenAI error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes an error.
type ErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

// StringList accepts a JSON string or a list of strings.
type StringList []string

// UnmarshalJSON accepts "x" or ["x", "y"].
func (s *StringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*s = nil
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("expected a string or a list of strings")
	}
	*s = many
	return nil
}
