package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/sse"
	"github.com/nikolasss4/switchyard/internal/translate"
)

// OpenAIConfig configures an OpenAI-compatible upstream.
type OpenAIConfig struct {
	Name           string
	BaseURL        string // e.g. https://api.openai.com/v1 or http://localhost:8080/v1
	APIKey         string
	MaxTokensField string // max_tokens (default) or max_completion_tokens
	Headers        map[string]string
}

// OpenAI is an adapter for any server implementing /chat/completions.
type OpenAI struct {
	cfg    OpenAIConfig
	client *http.Client
}

// NewOpenAI returns an OpenAI-compatible adapter.
func NewOpenAI(cfg OpenAIConfig, client *http.Client) *OpenAI {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &OpenAI{cfg: cfg, client: client}
}

func (p *OpenAI) Name() string { return p.cfg.Name }
func (p *OpenAI) Kind() string { return "openai" }

func (p *OpenAI) post(ctx context.Context, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, transportError(p.cfg.Name, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, p.httpError(resp)
	}
	return resp, nil
}

func (p *OpenAI) httpError(resp *http.Response) *Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Provider: p.cfg.Name, Status: resp.StatusCode, Type: "api_error", RetryAfter: retryAfter(resp.Header)}
	var env oai.ErrorResponse
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		e.Message, e.Type = env.Error.Message, env.Error.Type
	} else {
		e.Message = strings.TrimSpace(string(body))
	}
	return e
}

func (p *OpenAI) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	r := *req
	r.Stream = false
	resp, err := p.post(ctx, "/chat/completions", translate.ToOpenAIRequest(&r, p.cfg.MaxTokensField))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out oai.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, transportError(p.cfg.Name, fmt.Errorf("decode response: %w", err))
	}
	return translate.FromOpenAIResponse(&out)
}

func (p *OpenAI) Stream(ctx context.Context, req *llm.Request) (Stream, error) {
	r := *req
	r.Stream = true
	resp, err := p.post(ctx, "/chat/completions", translate.ToOpenAIRequest(&r, p.cfg.MaxTokensField))
	if err != nil {
		return nil, err
	}
	return &openAIStream{name: p.cfg.Name, body: resp.Body, r: sse.NewReader(resp.Body), conv: translate.NewOpenAIStreamReader()}, nil
}

func (p *OpenAI) Embed(ctx context.Context, req *oai.EmbeddingRequest) (*oai.EmbeddingResponse, error) {
	resp, err := p.post(ctx, "/embeddings", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out oai.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, transportError(p.cfg.Name, fmt.Errorf("decode response: %w", err))
	}
	return &out, nil
}

type openAIStream struct {
	name    string
	body    io.ReadCloser
	r       *sse.Reader
	conv    *translate.OpenAIStreamReader
	pending []*llm.Event
	done    bool
}

func (s *openAIStream) Next() (*llm.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			return nil, io.EOF
		}
		raw, err := s.r.Next()
		if errors.Is(err, io.EOF) {
			// Some servers end without [DONE]; finish the message anyway.
			s.pending, s.done = s.conv.Finish(), true
			if len(s.pending) == 0 {
				return nil, &Error{Provider: s.name, Message: "upstream stream ended early"}
			}
			continue
		}
		if err != nil {
			return nil, transportError(s.name, err)
		}
		if string(raw.Data) == "[DONE]" {
			s.pending, s.done = s.conv.Finish(), true
			continue
		}
		var chunk oai.Chunk
		if err := json.Unmarshal(raw.Data, &chunk); err != nil {
			var env oai.ErrorResponse
			if json.Unmarshal(raw.Data, &env) == nil && env.Error.Message != "" {
				return nil, &Error{Provider: s.name, Status: 500, Type: env.Error.Type, Message: env.Error.Message}
			}
			return nil, &Error{Provider: s.name, Message: "bad stream chunk: " + err.Error()}
		}
		if chunk.Choices == nil && chunk.Usage == nil {
			var env oai.ErrorResponse
			if json.Unmarshal(raw.Data, &env) == nil && env.Error.Message != "" {
				return nil, &Error{Provider: s.name, Status: 500, Type: env.Error.Type, Message: env.Error.Message}
			}
		}
		s.pending = s.conv.Chunk(&chunk)
	}
}

func (s *openAIStream) Close() error { return s.body.Close() }
