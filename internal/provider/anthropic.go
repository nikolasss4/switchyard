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
)

// AnthropicVersion is the API version the adapter sends. It rejects
// temperature, top_p and top_k, so the adapter drops them.
const AnthropicVersion = "2026-10-21"

// AnthropicConfig configures an Anthropic Messages API upstream.
type AnthropicConfig struct {
	Name    string
	BaseURL string // https://api.anthropic.com
	APIKey  string
	Headers map[string]string
}

// Anthropic is an adapter for the Anthropic Messages API (and the mock's
// /v1/messages endpoint).
type Anthropic struct {
	cfg    AnthropicConfig
	client *http.Client
}

// NewAnthropic returns an Anthropic adapter.
func NewAnthropic(cfg AnthropicConfig, client *http.Client) *Anthropic {
	cfg.BaseURL = strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1")
	return &Anthropic{cfg: cfg, client: client}
}

func (p *Anthropic) Name() string { return p.cfg.Name }
func (p *Anthropic) Kind() string { return "anthropic" }

// body builds the upstream request body. A request that arrived on the
// Anthropic surface is forwarded as sent, with the model and stream flag
// rewritten and sampling parameters removed.
func body(req *llm.Request, stream bool) ([]byte, error) {
	if len(req.Raw) > 0 {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(req.Raw, &m); err != nil {
			return nil, err
		}
		m["model"], _ = json.Marshal(req.Model)
		if stream {
			m["stream"] = json.RawMessage("true")
		} else {
			delete(m, "stream")
		}
		delete(m, "temperature")
		delete(m, "top_p")
		delete(m, "top_k")
		return json.Marshal(m)
	}
	r := *req
	r.Stream, r.Temperature, r.TopP = stream, nil, nil
	return json.Marshal(r)
}

func (p *Anthropic) post(ctx context.Context, req *llm.Request, stream bool) (*http.Response, error) {
	b, err := body(req, stream)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("anthropic-version", AnthropicVersion)
	if p.cfg.APIKey != "" {
		hr.Header.Set("x-api-key", p.cfg.APIKey)
	}
	if len(req.Betas) > 0 {
		hr.Header.Set("anthropic-beta", strings.Join(req.Betas, ","))
	}
	for k, v := range p.cfg.Headers {
		hr.Header.Set(k, v)
	}
	resp, err := p.client.Do(hr)
	if err != nil {
		return nil, transportError(p.cfg.Name, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, p.httpError(resp)
	}
	return resp, nil
}

func (p *Anthropic) httpError(resp *http.Response) *Error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Provider: p.cfg.Name, Status: resp.StatusCode, Type: "api_error", RetryAfter: retryAfter(resp.Header)}
	var env struct {
		Error llm.APIError `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error.Message != "" {
		e.Type, e.Message = env.Error.Type, env.Error.Message
	} else {
		e.Message = strings.TrimSpace(string(b))
	}
	return e
}

func (p *Anthropic) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	resp, err := p.post(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out llm.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, transportError(p.cfg.Name, fmt.Errorf("decode response: %w", err))
	}
	return &out, nil
}

func (p *Anthropic) Stream(ctx context.Context, req *llm.Request) (Stream, error) {
	resp, err := p.post(ctx, req, true)
	if err != nil {
		return nil, err
	}
	return &anthropicStream{name: p.cfg.Name, body: resp.Body, r: sse.NewReader(resp.Body)}, nil
}

func (p *Anthropic) Embed(context.Context, *oai.EmbeddingRequest) (*oai.EmbeddingResponse, error) {
	return nil, &Error{Provider: p.cfg.Name, Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "the Anthropic API has no embeddings endpoint"}
}

type anthropicStream struct {
	name    string
	body    io.ReadCloser
	r       *sse.Reader
	stopped bool
}

// errorStatus maps an in-stream error type to the HTTP status it would have had.
func errorStatus(typ string) int {
	switch typ {
	case "overloaded_error":
		return 529
	case "rate_limit_error":
		return 429
	case "invalid_request_error":
		return 400
	case "authentication_error":
		return 401
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	case "request_too_large":
		return 413
	}
	return 500
}

func (s *anthropicStream) Next() (*llm.Event, error) {
	for {
		if s.stopped {
			return nil, io.EOF
		}
		raw, err := s.r.Next()
		if errors.Is(err, io.EOF) {
			return nil, &Error{Provider: s.name, Message: "upstream stream ended before message_stop"}
		}
		if err != nil {
			return nil, transportError(s.name, err)
		}
		var ev llm.Event
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			return nil, &Error{Provider: s.name, Message: "bad stream event: " + err.Error()}
		}
		switch ev.Type {
		case llm.EventPing:
			continue
		case llm.EventError:
			e := &Error{Provider: s.name, Status: 500, Type: "api_error", Message: "upstream stream error"}
			if ev.Error != nil {
				e.Type, e.Message, e.Status = ev.Error.Type, ev.Error.Message, errorStatus(ev.Error.Type)
			}
			return nil, e
		case llm.EventMessageStop:
			s.stopped = true
		}
		ev.Raw = raw.Data
		return &ev, nil
	}
}

func (s *anthropicStream) Close() error { return s.body.Close() }
