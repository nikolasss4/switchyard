// Package provider defines the upstream provider interface and its adapters:
// Anthropic, any OpenAI-compatible server (OpenAI, vLLM, Ollama, mlx_lm.server)
// and, through either of those, the mock provider.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
)

// Provider is one upstream. Requests carry the upstream model name.
type Provider interface {
	Name() string
	Kind() string
	Complete(ctx context.Context, req *llm.Request) (*llm.Response, error)
	Stream(ctx context.Context, req *llm.Request) (Stream, error)
	Embed(ctx context.Context, req *oai.EmbeddingRequest) (*oai.EmbeddingResponse, error)
}

// Stream yields canonical events. Next returns io.EOF after message_stop.
type Stream interface {
	Next() (*llm.Event, error)
	Close() error
}

// Error is an upstream failure.
type Error struct {
	Provider   string
	Status     int // 0 for transport errors
	Type       string
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s: HTTP %d %s: %s", e.Provider, e.Status, e.Type, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether the same request may succeed if sent again (or to
// another provider): transport errors, timeouts, 408, 409, 429 and 5xx.
func (e *Error) Retryable() bool {
	switch {
	case e.Status == 0:
		return !errors.Is(e.Err, context.Canceled)
	case e.Status == 408, e.Status == 409, e.Status == 429, e.Status >= 500:
		return true
	}
	return false
}

// IsRetryable reports whether err is a retryable provider error.
func IsRetryable(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Retryable()
}

// CountsAsFailure reports whether err should count against a provider's
// health (circuit breaker). Client errors (4xx other than 408/429) and the
// caller's own cancellation do not.
func CountsAsFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var pe *Error
	if !errors.As(err, &pe) {
		return true
	}
	return pe.Retryable()
}

func transportError(provider string, err error) *Error {
	msg := err.Error()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		msg = "upstream timeout"
	}
	return &Error{Provider: provider, Message: msg, Err: err}
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// NewHTTPClient returns a client tuned for many concurrent requests to a few
// hosts. Per-request deadlines come from the request context, not the client.
func NewHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Keep the idle pool at least as large as the expected concurrency per
	// upstream: a smaller pool closes and reopens connections under load, and
	// the TIME_WAIT churn runs a host out of ephemeral ports.
	tr.MaxIdleConns = 8192
	tr.MaxIdleConnsPerHost = 4096
	tr.IdleConnTimeout = 90 * time.Second
	tr.ResponseHeaderTimeout = 0
	return &http.Client{Transport: tr}
}
