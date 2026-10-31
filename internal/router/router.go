// Package router resolves a requested model to a chain of provider targets and
// executes requests along that chain with fallbacks.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/provider"
)

// Target is a resolved provider and upstream model.
type Target struct {
	Provider provider.Provider
	Model    string
}

// Route is the resolved chain for one request.
type Route struct {
	Alias   string
	Targets []Target
	Model   *config.Model
}

// Result describes how a request was served.
type Result struct {
	Target        *Target
	Attempts      int // upstream calls made, including failed ones
	FallbackDepth int // index of the target that served it
}

// ErrUnknownModel is returned for models that are neither an alias nor provider/model.
var ErrUnknownModel = errors.New("unknown model")

// Router resolves models and runs requests.
type Router struct {
	providers map[string]provider.Provider
	models    map[string]*config.Model
	rules     []config.Rule
}

// New returns a router over the configured aliases and rules.
func New(cfg *config.Config, providers map[string]provider.Provider) *Router {
	r := &Router{providers: providers, models: map[string]*config.Model{}, rules: cfg.Rules}
	for i := range cfg.Models {
		r.models[cfg.Models[i].Alias] = &cfg.Models[i]
	}
	return r
}

// Resolve maps a requested model to a route. Rules are applied first, in
// order; the first rule whose conditions all hold replaces the model. The
// result is an alias from the config, or "provider/model" to address one
// upstream model directly.
func (r *Router) Resolve(model, tenant string, h http.Header) (*Route, error) {
	for _, rule := range r.rules {
		if rule.Matches(model, tenant, h) {
			model = rule.Use
			break
		}
	}
	if m, ok := r.models[model]; ok {
		route := &Route{Alias: model, Model: m}
		for _, t := range m.Targets {
			route.Targets = append(route.Targets, Target{Provider: r.providers[t.Provider], Model: t.Model})
		}
		return route, nil
	}
	if name, upstream, ok := strings.Cut(model, "/"); ok {
		if p, found := r.providers[name]; found && upstream != "" {
			return &Route{Alias: model, Targets: []Target{{Provider: p, Model: upstream}}, Model: &config.Model{Alias: model, Timeout: 120e9}}, nil
		}
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownModel, model)
}

// Aliases lists the configured aliases.
func (r *Router) Aliases() []string {
	out := make([]string, 0, len(r.models))
	for a := range r.models {
		out = append(out, a)
	}
	return out
}

// fallsThrough reports whether a failed target should hand the request to the
// next one. A malformed request fails the same way everywhere, so 400, 413 and
// 422 are returned at once; a bad upstream key (401/403) or a model the
// upstream does not have (404) are that provider's problem.
func fallsThrough(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		return !errors.Is(err, context.Canceled)
	}
	switch pe.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return false
	}
	return true
}

// attempt is one upstream call with the route's per-attempt timeout. The
// returned cancel must be called once the result is no longer needed (for a
// stream, when it is closed).
func attempt[T any](ctx context.Context, route *Route, t Target, req *llm.Request,
	call func(context.Context, provider.Provider, *llm.Request) (T, error)) (T, context.CancelFunc, error) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if route.Model != nil && route.Model.Timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, route.Model.Timeout)
	}
	up := *req
	up.Model = t.Model
	v, err := call(actx, t.Provider, &up)
	if err != nil {
		cancel()
	}
	return v, cancel, err
}

// run walks the chain until a target succeeds.
func run[T any](ctx context.Context, route *Route, req *llm.Request,
	call func(context.Context, provider.Provider, *llm.Request) (T, error)) (T, context.CancelFunc, *Result, error) {
	res := &Result{}
	var zero T
	var lastErr error
	for depth, t := range route.Targets {
		res.Attempts++
		v, cancel, err := attempt(ctx, route, t, req, call)
		if err == nil {
			res.Target, res.FallbackDepth = &route.Targets[depth], depth
			return v, cancel, res, nil
		}
		lastErr = err
		if ctx.Err() != nil || !fallsThrough(err) {
			break
		}
	}
	if ctx.Err() != nil {
		return zero, nil, res, ctx.Err()
	}
	return zero, nil, res, lastErr
}

// Complete sends a non-streaming request along the route.
func (r *Router) Complete(ctx context.Context, route *Route, req *llm.Request) (*llm.Response, *Result, error) {
	resp, cancel, res, err := run(ctx, route, req, func(ctx context.Context, p provider.Provider, req *llm.Request) (*llm.Response, error) {
		return p.Complete(ctx, req)
	})
	if cancel != nil {
		cancel()
	}
	return resp, res, err
}

// Stream opens a stream along the route. Each attempt is read until its first
// content event before it counts as a success, so an upstream that accepts the
// connection and then fails (an overloaded error right after message_start,
// say) is still retried elsewhere. Nothing has reached the client at that point.
func (r *Router) Stream(ctx context.Context, route *Route, req *llm.Request) (provider.Stream, *Result, error) {
	s, cancel, res, err := run(ctx, route, req, func(ctx context.Context, p provider.Provider, req *llm.Request) (*peeked, error) {
		st, err := p.Stream(ctx, req)
		if err != nil {
			return nil, err
		}
		return peek(st)
	})
	if err != nil {
		return nil, res, err
	}
	s.cancel = cancel
	return s, res, nil
}

// peeked is a stream whose events up to the first content event are buffered.
type peeked struct {
	provider.Stream
	buf    []*llm.Event
	cancel context.CancelFunc
}

func peek(s provider.Stream) (*peeked, error) {
	p := &peeked{Stream: s}
	for {
		ev, err := s.Next()
		if err != nil {
			if errors.Is(err, io.EOF) && len(p.buf) > 0 {
				return p, nil
			}
			_ = s.Close()
			return nil, err
		}
		p.buf = append(p.buf, ev)
		if ev.IsContent() || ev.Type == llm.EventMessageStop {
			return p, nil
		}
	}
}

func (p *peeked) Next() (*llm.Event, error) {
	if len(p.buf) > 0 {
		ev := p.buf[0]
		p.buf = p.buf[1:]
		return ev, nil
	}
	return p.Stream.Next()
}

func (p *peeked) Close() error {
	err := p.Stream.Close()
	if p.cancel != nil {
		p.cancel()
	}
	return err
}
