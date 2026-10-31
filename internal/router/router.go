// Package router resolves a requested model to a chain of provider targets and
// executes requests along that chain with fallbacks.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nikolasss4/switchyard/internal/breaker"
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
	Hedged        bool
}

// ErrUnknownModel is returned for models that are neither an alias nor provider/model.
var ErrUnknownModel = errors.New("unknown model")

// ErrCircuitOpen is wrapped by the error for a target skipped because its
// provider's breaker is open.
var ErrCircuitOpen = errors.New("circuit open")

// Router resolves models and runs requests.
type Router struct {
	providers map[string]provider.Provider
	models    map[string]*config.Model
	rules     []config.Rule
	retry     config.Retry
	breakers  map[string]*breaker.Breaker
	rnd       func() float64

	// OnAttempt, if set, wraps every upstream attempt (tracing, metrics).
	OnAttempt func(ctx context.Context, t Target) (context.Context, func(error))
}

// New returns a router over the configured aliases and rules, with one
// circuit breaker per provider.
func New(cfg *config.Config, providers map[string]provider.Provider) *Router {
	r := &Router{providers: providers, models: map[string]*config.Model{}, rules: cfg.Rules,
		retry: cfg.Retry, breakers: map[string]*breaker.Breaker{}, rnd: rand.Float64}
	if r.retry.MaxAttempts < 1 {
		r.retry.MaxAttempts = 1
	}
	for i := range cfg.Models {
		r.models[cfg.Models[i].Alias] = &cfg.Models[i]
	}
	b := cfg.Breaker
	for name := range providers {
		r.breakers[name] = breaker.New(breaker.Config{Window: b.Window, MinRequests: b.MinRequests,
			FailureRate: b.FailureRate, OpenFor: b.OpenFor, Probes: b.HalfOpenProbes})
	}
	return r
}

// Breakers returns the per-provider breakers, keyed by provider name.
func (r *Router) Breakers() map[string]*breaker.Breaker { return r.breakers }

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
		if actx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = &provider.Error{Provider: t.Provider.Name(), Message: "attempt timed out", Err: err}
		}
	}
	return v, cancel, err
}

// backoff is the delay before retry number i (1-based): full jitter over an
// exponential ceiling, or the upstream's Retry-After if that is longer. It
// returns -1 when Retry-After exceeds MaxDelay; falling back beats waiting.
func (r *Router) backoff(i int, err error) time.Duration {
	ceiling := r.retry.BaseDelay << (i - 1)
	if ceiling > r.retry.MaxDelay || ceiling <= 0 {
		ceiling = r.retry.MaxDelay
	}
	d := time.Duration(r.rnd() * float64(ceiling))
	var pe *provider.Error
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		if pe.RetryAfter > r.retry.MaxDelay {
			return -1
		}
		d = max(d, pe.RetryAfter)
	}
	return d
}

// lane serves a request from one target, retrying retryable failures with
// backoff. It never retries once the call succeeded; for streams that means
// once the first content event was read (see peek).
func lane[T any](ctx context.Context, r *Router, route *Route, depth int, req *llm.Request, attempts *atomic.Int64,
	call func(context.Context, provider.Provider, *llm.Request) (T, error)) (T, context.CancelFunc, error) {
	t := route.Targets[depth]
	br := r.breakers[t.Provider.Name()]
	var zero T
	var lastErr error
	for i := 1; i <= r.retry.MaxAttempts; i++ {
		if br != nil && !br.Allow() {
			if lastErr == nil {
				lastErr = &provider.Error{Provider: t.Provider.Name(), Status: http.StatusServiceUnavailable,
					Type: "overloaded_error", Message: "circuit open", Err: ErrCircuitOpen}
			}
			return zero, nil, lastErr
		}
		attempts.Add(1)
		actx, end := ctx, func(error) {}
		if r.OnAttempt != nil {
			actx, end = r.OnAttempt(ctx, t)
		}
		v, cancel, err := attempt(actx, route, t, req, call)
		end(err)
		if br != nil && ctx.Err() == nil {
			br.Record(provider.CountsAsFailure(err))
		}
		if err == nil {
			return v, cancel, nil
		}
		lastErr = err
		if ctx.Err() != nil || !provider.IsRetryable(err) || i == r.retry.MaxAttempts {
			break
		}
		d := r.backoff(i, err)
		if d < 0 || !sleep(ctx, d) {
			break
		}
	}
	return zero, nil, lastErr
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type laneResult[T any] struct {
	v      T
	cancel context.CancelFunc // releases the attempt (its timeout and the lane's context)
	err    error
	depth  int
	lane   int
}

// run walks the chain until a target succeeds. With hedging on, a second lane
// starts on the next target (or the same one, for a single-target chain) if the
// first has not succeeded after HedgeAfter; the first success wins and the other
// lane is cancelled, its result released.
func run[T any](ctx context.Context, r *Router, route *Route, req *llm.Request,
	call func(context.Context, provider.Provider, *llm.Request) (T, error), release func(T)) (T, context.CancelFunc, *Result, error) {
	res := &Result{}
	var attempts atomic.Int64
	var zero T
	n := len(route.Targets)
	results := make(chan laneResult[T], n+1)
	var laneCancels []context.CancelFunc
	pending, next := 0, 0
	launch := func(depth int) {
		lctx, lcancel := context.WithCancel(ctx)
		id := len(laneCancels)
		laneCancels = append(laneCancels, lcancel)
		pending++
		go func() {
			v, c, err := lane(lctx, r, route, depth, req, &attempts, call)
			if err != nil {
				lcancel()
			} else {
				inner := c
				c = func() { inner(); lcancel() }
			}
			results <- laneResult[T]{v: v, cancel: c, err: err, depth: depth, lane: id}
		}()
	}
	// stop cancels every lane but keep and releases whatever they still return.
	stop := func(keep int) {
		for i, c := range laneCancels {
			if i != keep {
				c()
			}
		}
		if left := pending; left > 0 {
			go func() {
				for range left {
					if lr := <-results; lr.err == nil {
						release(lr.v)
						lr.cancel()
					}
				}
			}()
		}
	}
	finish := func(lr *laneResult[T], err error) (T, context.CancelFunc, *Result, error) {
		res.Attempts = int(attempts.Load())
		if lr != nil && err == nil {
			res.Target, res.FallbackDepth = &route.Targets[lr.depth], lr.depth
			return lr.v, lr.cancel, res, nil
		}
		return zero, nil, res, err
	}

	launch(0)
	next = 1
	var hedge <-chan time.Time
	if route.Model != nil && route.Model.HedgeAfter > 0 {
		t := time.NewTimer(route.Model.HedgeAfter)
		defer t.Stop()
		hedge = t.C
	}
	var lastErr error
	for {
		select {
		case lr := <-results:
			pending--
			if lr.err == nil {
				stop(lr.lane)
				return finish(&lr, nil)
			}
			lastErr = lr.err
			if ctx.Err() != nil {
				stop(-1)
				return finish(nil, ctx.Err())
			}
			if !fallsThrough(lr.err) {
				stop(-1)
				return finish(nil, lastErr)
			}
			if pending > 0 {
				continue // the other hedge lane is still running
			}
			if next < n {
				launch(next)
				next++
				continue
			}
			return finish(nil, lastErr)
		case <-hedge:
			hedge = nil
			if pending > 0 && (next < n || n == 1) {
				res.Hedged = true
				if next < n {
					launch(next)
					next++
				} else {
					launch(0)
				}
			}
		case <-ctx.Done():
			stop(-1)
			return finish(nil, ctx.Err())
		}
	}
}

// Complete sends a non-streaming request along the route.
func (r *Router) Complete(ctx context.Context, route *Route, req *llm.Request) (*llm.Response, *Result, error) {
	resp, cancel, res, err := run(ctx, r, route, req, func(ctx context.Context, p provider.Provider, req *llm.Request) (*llm.Response, error) {
		return p.Complete(ctx, req)
	}, func(*llm.Response) {})
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
	s, cancel, res, err := run(ctx, r, route, req, func(ctx context.Context, p provider.Provider, req *llm.Request) (*peeked, error) {
		st, err := p.Stream(ctx, req)
		if err != nil {
			return nil, err
		}
		return peek(st)
	}, func(p *peeked) { _ = p.Stream.Close() })
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
