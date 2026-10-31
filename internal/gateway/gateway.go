// Package gateway is the request pipeline between the HTTP surfaces and the
// router: admission (model access, budget, rate limits) before a request, and
// accounting (cost, spend, token settlement, usage records) after it.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/budget"
	"github.com/nikolasss4/switchyard/internal/cache"
	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/limiter"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/pricing"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
	"github.com/nikolasss4/switchyard/internal/store"
	"github.com/nikolasss4/switchyard/internal/usage"
)

// Error is a gateway-level refusal with the status to return.
type Error struct {
	Status     int
	Type       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Message }

// Deps are the gateway's collaborators.
type Deps struct {
	Router   *router.Router
	Limiter  limiter.Limiter
	Spend    budget.Counter
	Prices   *pricing.Table
	Usage    usage.Sink
	Defaults config.Limits
	Log      *slog.Logger
	Caches   Caches
	Observer Observer
}

// Observer is told when a call starts and finishes (metrics, traces, capture).
type Observer interface {
	Start(ctx context.Context, c *Call) context.Context
	Finish(c *Call, o *Outcome, costUSD float64)
}

// Begin starts observing a call and returns the context to run it in.
func (g *Gateway) Begin(ctx context.Context, c *Call) context.Context {
	if g.Observer == nil {
		return ctx
	}
	return g.Observer.Start(ctx, c)
}

// Gateway runs requests through admission, the router and accounting.
type Gateway struct {
	Deps
	now  func() time.Time
	acct chan func()
	wg   sync.WaitGroup
}

// New returns a gateway. Accounting after a request (TPM settlement, spend,
// usage record, metrics) runs on a pool of workers, off the response path; when
// the queue is full it runs inline, which slows clients instead of losing it.
func New(d Deps) *Gateway {
	if d.Usage == nil {
		d.Usage = usage.Discard{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	g := &Gateway{Deps: d, now: time.Now, acct: make(chan func(), 16384)}
	for range 4 * runtime.GOMAXPROCS(0) {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			for f := range g.acct {
				f()
			}
		}()
	}
	return g
}

// Close waits for queued accounting to finish.
func (g *Gateway) Close() {
	close(g.acct)
	g.wg.Wait()
}

// Call is one request moving through the pipeline.
type Call struct {
	ID      string
	Surface string // openai | anthropic
	Key     *auth.Key
	Req     *llm.Request
	Header  http.Header
	Start   time.Time

	Route    *router.Route
	Budget   budget.State
	Span     any // the observer's span, if any
	reserved int // TPM tokens taken at admission
}

// NewCall starts a call.
func (g *Gateway) NewCall(surface string, key *auth.Key, req *llm.Request, h http.Header) *Call {
	return &Call{ID: newID(), Surface: surface, Key: key, Req: req, Header: h, Start: g.now()}
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// Outcome is what happened to a call, for accounting.
type Outcome struct {
	Provider      string
	Model         string
	Attempts      int
	FallbackDepth int
	Hedged        bool
	Cache         string // "" | exact | semantic
	CacheEligible bool   // the alias has a cache and the request could use it
	Usage         llm.Usage
	Response      *llm.Response // for content capture; nil for failed calls
	Status        int
	TTFT          time.Duration
	Err           error
}

// Admit resolves the route and applies model access, budget and rate limits.
func (g *Gateway) Admit(ctx context.Context, c *Call) error {
	route, err := g.Router.Resolve(c.Req.Model, c.Key.TenantID, c.Header)
	if err != nil {
		return err
	}
	c.Route = route
	if !c.Key.Allows(route.Alias) {
		return &Error{Status: http.StatusForbidden, Type: "permission_error", Message: fmt.Sprintf("this key may not use model %q", route.Alias)}
	}

	var buckets []limiter.Bucket
	tpm := limit(c.Key.TPM, g.Defaults.TPM)
	if g.Limiter != nil {
		if rpm := limit(c.Key.RPM, g.Defaults.RPM); rpm > 0 {
			buckets = append(buckets, limiter.Bucket{Key: c.Key.ID + ":rpm", PerMinute: rpm, Cost: 1})
		}
		if tpm > 0 {
			c.reserved = EstimateInputTokens(c.Req)
			buckets = append(buckets, limiter.Bucket{Key: c.Key.ID + ":tpm", PerMinute: tpm, Cost: c.reserved})
		}
	}

	// The budget read and the bucket take are independent round trips to
	// Redis, so they run concurrently.
	var spent float64
	var spendErr error
	checkBudget := c.Key.MonthlyBudgetUSD != nil && g.Spend != nil
	var budgetDone chan struct{}
	if checkBudget {
		budgetDone = make(chan struct{})
		go func() {
			defer close(budgetDone)
			spent, spendErr = g.Spend.Spent(ctx, c.Key.ID, g.now())
		}()
	}
	var d limiter.Decision
	var takeErr error
	if len(buckets) > 0 {
		d, takeErr = g.Limiter.Take(ctx, buckets)
	} else {
		d.Allowed = true
	}
	if checkBudget {
		<-budgetDone
		if spendErr != nil {
			// Fail open: a Redis outage should not take every tenant down.
			g.Log.Warn("budget check failed; allowing request", "key", c.Key.ID, "err", spendErr)
		} else {
			c.Budget = budget.Evaluate(spent, c.Key.MonthlyBudgetUSD, c.Key.SoftBudgetRatio)
			if c.Budget == budget.Hard {
				if takeErr == nil && d.Allowed {
					g.refund(buckets)
				}
				c.reserved = 0
				// 402, not 429: SDKs retry 429s, and waiting does not help here.
				return &Error{Status: http.StatusPaymentRequired, Type: "budget_exceeded",
					Message: fmt.Sprintf("monthly budget of $%.2f reached ($%.2f spent)", *c.Key.MonthlyBudgetUSD, spent)}
			}
		}
	}
	if takeErr != nil {
		g.Log.Warn("rate limit check failed; allowing request", "key", c.Key.ID, "err", takeErr)
		c.reserved = 0
		return nil
	}
	if !d.Allowed {
		c.reserved = 0
		if d.RetryAfter < 0 {
			return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error",
				Message: fmt.Sprintf("request needs about %d tokens but the key's limit is %d tokens per minute", EstimateInputTokens(c.Req), tpm)}
		}
		what := "requests"
		if d.Denied == c.Key.ID+":tpm" {
			what = "tokens"
		}
		return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", RetryAfter: d.RetryAfter,
			Message: fmt.Sprintf("rate limit exceeded (%s per minute)", what)}
	}
	return nil
}

// refund gives back what a refused request took from its buckets.
func (g *Gateway) refund(buckets []limiter.Bucket) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, b := range buckets {
		if err := g.Limiter.Adjust(ctx, b, b.Cost); err != nil {
			g.Log.Warn("refund failed", "bucket", b.Key, "err", err)
		}
	}
}

func limit(own *int, def int) int {
	if own != nil {
		return *own
	}
	return def
}

// EstimateInputTokens is a cheap pre-request estimate (about four characters
// per token) used to admit a request against its TPM bucket. The bucket is
// settled with the real count afterwards.
func EstimateInputTokens(r *llm.Request) int {
	n := len(r.SystemText())
	for _, m := range r.Messages {
		for _, b := range m.Content {
			n += len(b.Text) + len(b.Input) + len(b.Content) + len(b.Thinking)
			if b.Source != nil {
				n += 4 * 1600 // an image is on the order of 1.6K tokens
			}
		}
	}
	for _, t := range r.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return max(1, n/4)
}

// Complete runs a non-streaming call.
func (g *Gateway) Complete(ctx context.Context, c *Call) (*llm.Response, *Outcome, error) {
	hit, kind, p := g.lookup(ctx, c)
	if hit != nil {
		return hit, &Outcome{Status: http.StatusOK, Cache: kind, CacheEligible: true, Usage: hit.Usage, Response: hit}, nil
	}
	resp, res, err := g.Router.Complete(ctx, c.Route, c.Req)
	o := &Outcome{Status: http.StatusOK, Err: err, CacheEligible: p != nil}
	fillTarget(o, res)
	if err == nil {
		o.Usage, o.Response = resp.Usage, resp
		g.fill(p, resp)
	}
	return resp, o, err
}

// Stream opens a streaming call. The returned stream finishes the call when it
// ends, fails or is closed. A cache hit is replayed as a stream.
func (g *Gateway) Stream(ctx context.Context, c *Call) (provider.Stream, *Outcome, error) {
	hit, kind, p := g.lookup(ctx, c)
	if hit != nil {
		o := &Outcome{Status: http.StatusOK, Cache: kind, CacheEligible: true, Response: hit}
		return &metered{Stream: cache.NewReplay(hit), g: g, c: c, o: o}, o, nil
	}
	s, res, err := g.Router.Stream(ctx, c.Route, c.Req)
	o := &Outcome{Status: http.StatusOK, Err: err, CacheEligible: p != nil}
	fillTarget(o, res)
	if err != nil {
		return nil, o, err
	}
	m := &metered{Stream: s, g: g, c: c, o: o}
	if p != nil || (c.Key != nil && c.Key.CapturePrompts) {
		m.asm, m.pending = cache.NewAssembler(), p
	}
	return m, o, nil
}

func fillTarget(o *Outcome, res *router.Result) {
	if res == nil {
		return
	}
	if t := res.Target; t != nil && t.Provider != nil {
		o.Provider, o.Model = t.Provider.Name(), t.Model
	}
	o.Attempts, o.FallbackDepth, o.Hedged = res.Attempts, res.FallbackDepth, res.Hedged
}

// Finish settles accounting for a call, asynchronously. Call it once per call.
func (g *Gateway) Finish(c *Call, o *Outcome) {
	end := g.now()
	f := func() { g.finish(c, o, end) }
	select {
	case g.acct <- f:
	default:
		f()
	}
}

func (g *Gateway) finish(c *Call, o *Outcome, end time.Time) {
	if o.Err != nil && o.Status == http.StatusOK {
		o.Status = StatusOf(o.Err)
	}
	cost, priced := 0.0, true
	if o.Cache == "" && o.Provider != "" {
		cost, priced = g.Prices.CostFor(o.Provider, o.Model, o.Usage)
	}
	if !priced {
		g.Log.Warn("no price for model; cost recorded as 0", "provider", o.Provider, "model", o.Model)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if c.Key != nil {
		if tpm := limit(c.Key.TPM, g.Defaults.TPM); tpm > 0 && g.Limiter != nil && c.reserved > 0 {
			used := 0
			if o.Cache == "" {
				u := o.Usage
				used = u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			}
			if delta := c.reserved - used; delta != 0 {
				if err := g.Limiter.Adjust(ctx, limiter.Bucket{Key: c.Key.ID + ":tpm", PerMinute: tpm}, delta); err != nil {
					g.Log.Warn("tpm settle failed", "key", c.Key.ID, "err", err)
				}
			}
		}
		if cost > 0 && g.Spend != nil {
			if err := g.Spend.Add(ctx, c.Key.ID, g.now(), cost); err != nil {
				g.Log.Error("spend record failed", "key", c.Key.ID, "err", err)
			}
		}
	}

	rec := store.UsageRecord{
		TS: c.Start, RequestID: c.ID, Surface: c.Surface, Stream: c.Req.Stream, Status: o.Status,
		Provider: o.Provider, Model: o.Model, Cache: o.Cache, Attempts: o.Attempts, FallbackDepth: o.FallbackDepth,
		InputTokens: o.Usage.InputTokens, OutputTokens: o.Usage.OutputTokens,
		CacheReadTokens: o.Usage.CacheReadInputTokens, CacheWriteTokens: o.Usage.CacheCreationInputTokens,
		CostUSD: cost, PriceVersion: g.Prices.Version, LatencyMS: int(end.Sub(c.Start).Milliseconds()),
	}
	if c.Key != nil {
		rec.KeyID, rec.TenantID = c.Key.ID, c.Key.TenantID
	}
	if c.Route != nil {
		rec.Alias = c.Route.Alias
	}
	if o.TTFT > 0 {
		ms := int(o.TTFT.Milliseconds())
		rec.TTFTMS = &ms
	}
	g.Usage.Record(rec)
	if g.Observer != nil {
		g.Observer.Finish(c, o, cost)
	}

	// Prompts and completions are never logged here.
	attrs := []any{"request_id", c.ID, "tenant", rec.TenantID, "key", rec.KeyID, "surface", c.Surface, "alias", rec.Alias,
		"provider", o.Provider, "model", o.Model, "stream", c.Req.Stream, "status", o.Status, "attempts", o.Attempts,
		"fallback_depth", o.FallbackDepth, "cache", o.Cache, "input_tokens", o.Usage.InputTokens,
		"output_tokens", o.Usage.OutputTokens, "cost_usd", cost, "latency_ms", rec.LatencyMS}
	if rec.TTFTMS != nil {
		attrs = append(attrs, "ttft_ms", *rec.TTFTMS)
	}
	if o.Err != nil {
		attrs = append(attrs, "err", o.Err.Error())
		g.Log.Warn("request failed", attrs...)
		return
	}
	g.Log.Info("request", attrs...)
}

// StatusOf is the HTTP status an error maps to.
func StatusOf(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Status
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Status >= 400 && pe.Status < 500 && !pe.Retryable() {
			return pe.Status
		}
		return http.StatusBadGateway
	}
	if errors.Is(err, context.Canceled) {
		return 499 // client closed the request
	}
	if errors.Is(err, router.ErrUnknownModel) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// metered wraps a stream to collect usage and time to first token, and finishes
// the call exactly once.
type metered struct {
	provider.Stream
	g       *Gateway
	c       *Call
	o       *Outcome
	once    sync.Once
	gotStop bool
	asm     *cache.Assembler
	pending *pending
}

func (m *metered) Next() (*llm.Event, error) {
	ev, err := m.Stream.Next()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			m.o.Err = err
		}
		m.finish()
		return nil, err
	}
	if m.asm != nil {
		m.asm.Add(ev)
	}
	if m.o.TTFT == 0 && ev.IsContent() {
		m.o.TTFT = m.g.now().Sub(m.c.Start)
	}
	switch ev.Type {
	case llm.EventMessageStart:
		if ev.Message != nil {
			m.o.Usage = ev.Message.Usage
		}
	case llm.EventMessageDelta:
		if u := ev.Usage; u != nil {
			m.o.Usage.OutputTokens = u.OutputTokens
			if u.InputTokens > 0 {
				m.o.Usage.InputTokens = u.InputTokens
			}
			if u.CacheReadInputTokens > 0 {
				m.o.Usage.CacheReadInputTokens = u.CacheReadInputTokens
			}
			if u.CacheCreationInputTokens > 0 {
				m.o.Usage.CacheCreationInputTokens = u.CacheCreationInputTokens
			}
		}
	case llm.EventMessageStop:
		m.gotStop = true
		if m.asm != nil {
			if resp, ok := m.asm.Response(); ok {
				m.o.Response = resp
				m.g.fill(m.pending, resp)
			}
		}
	}
	return ev, nil
}

// Close ends the stream; closing before message_stop means the client left.
func (m *metered) Close() error {
	err := m.Stream.Close()
	if !m.gotStop && m.o.Err == nil {
		m.o.Err, m.o.Status = context.Canceled, 499
	}
	m.finish()
	return err
}

func (m *metered) finish() { m.once.Do(func() { m.g.Finish(m.c, m.o) }) }

// Embed runs an embeddings call on the route's first target.
func (g *Gateway) Embed(ctx context.Context, c *Call, in *oai.EmbeddingRequest) (*oai.EmbeddingResponse, *Outcome, error) {
	t := c.Route.Targets[0]
	up := *in
	up.Model = t.Model
	o := &Outcome{Status: http.StatusOK, Provider: t.Provider.Name(), Model: t.Model, Attempts: 1}
	resp, err := t.Provider.Embed(ctx, &up)
	o.Err = err
	if err == nil {
		o.Usage.InputTokens = resp.Usage.PromptTokens
	}
	return resp, o, err
}
