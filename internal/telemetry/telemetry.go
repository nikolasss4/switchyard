package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/nikolasss4/switchyard/internal/breaker"
	"github.com/nikolasss4/switchyard/internal/gateway"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
)

// SetupTracing installs an OTLP/HTTP trace exporter when
// OTEL_EXPORTER_OTLP_ENDPOINT (or the traces-specific variable) is set, and a
// no-op tracer otherwise. The returned function flushes and stops it.
func SetupTracing(ctx context.Context, version string) (trace.Tracer, func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return noop.NewTracerProvider().Tracer("switchyard"), func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	res, _ := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", "switchyard"), attribute.String("service.version", version)))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Tracer("switchyard"), tp.Shutdown, nil
}

// Observer implements gateway.Observer and the router's attempt hook.
type Observer struct {
	M       *Metrics
	Tracer  trace.Tracer
	Capture *slog.Logger // receives redacted prompts for tenants that opted in
}

type spanKey struct{}

// genAIProvider maps an adapter kind to gen_ai.provider.name.
func genAIProvider(kind string) string {
	if kind == "anthropic" {
		return "anthropic"
	}
	return "openai" // any OpenAI-compatible server
}

// Start opens the gateway span for a call, continuing an incoming trace.
func (o *Observer) Start(ctx context.Context, c *gateway.Call) context.Context {
	o.M.InFlight.Inc()
	if c.Req.Stream {
		o.M.Streams.Inc()
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(c.Header))
	op := "chat"
	if c.Surface == "embeddings" {
		op = "embeddings"
	}
	ctx, span := o.Tracer.Start(ctx, op+" "+c.Req.Model, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", op),
		attribute.String("gen_ai.request.model", c.Req.Model),
		attribute.Int("gen_ai.request.max_tokens", c.Req.MaxTokens),
		attribute.Bool("switchyard.stream", c.Req.Stream),
		attribute.String("switchyard.surface", c.Surface),
		attribute.String("switchyard.request_id", c.ID),
	))
	if c.Key != nil {
		span.SetAttributes(attribute.String("switchyard.tenant", c.Key.TenantID), attribute.String("switchyard.key_id", c.Key.ID))
	}
	c.Span = span
	return context.WithValue(ctx, spanKey{}, span)
}

// Finish records metrics, ends the span and captures content when enabled.
func (o *Observer) Finish(c *gateway.Call, out *gateway.Outcome, cost float64) {
	o.M.InFlight.Dec()
	if c.Req.Stream {
		o.M.Streams.Dec()
	}
	alias := ""
	if c.Route != nil {
		alias = c.Route.Alias
	}
	code := strconv.Itoa(out.Status/100) + "xx"
	o.M.Requests.WithLabelValues(c.Surface, alias, out.Provider, out.Model, code, out.Cache).Inc()

	var ge *gateway.Error
	if errors.As(out.Err, &ge) {
		o.M.Refused.WithLabelValues(ge.Type).Inc()
	}
	if c.Route != nil {
		o.M.Duration.WithLabelValues(alias, out.Provider, out.Model, strconv.FormatBool(c.Req.Stream)).Observe(time.Since(c.Start).Seconds())
		if out.TTFT > 0 {
			o.M.TTFT.WithLabelValues(alias, out.Provider, out.Model).Observe(out.TTFT.Seconds())
		}
		switch {
		case out.Cache == "exact":
			o.M.Cache.WithLabelValues(alias, "hit_exact").Inc()
		case out.Cache == "semantic":
			o.M.Cache.WithLabelValues(alias, "hit_semantic").Inc()
		case out.CacheEligible:
			o.M.Cache.WithLabelValues(alias, "miss").Inc()
		}
		if out.FallbackDepth > 0 {
			o.M.Fallbacks.WithLabelValues(alias, out.Provider, strconv.Itoa(out.FallbackDepth)).Inc()
		}
		if out.Hedged {
			o.M.Hedges.WithLabelValues(alias).Inc()
		}
	}
	if out.Cache == "" && out.Provider != "" {
		u := out.Usage
		for typ, n := range map[string]int{"input": u.InputTokens, "output": u.OutputTokens, "cache_read": u.CacheReadInputTokens, "cache_write": u.CacheCreationInputTokens} {
			if n > 0 {
				o.M.Tokens.WithLabelValues(alias, out.Provider, out.Model, typ).Add(float64(n))
			}
		}
		if cost > 0 && c.Key != nil {
			o.M.Cost.WithLabelValues(alias, out.Provider, out.Model, c.Key.TenantID).Add(cost)
		}
	}

	capture := c.Key != nil && c.Key.CapturePrompts && o.Capture != nil
	if span, ok := c.Span.(trace.Span); ok {
		span.SetAttributes(
			attribute.String("gen_ai.response.model", out.Model),
			attribute.Int("gen_ai.usage.input_tokens", out.Usage.InputTokens+out.Usage.CacheReadInputTokens+out.Usage.CacheCreationInputTokens),
			attribute.Int("gen_ai.usage.output_tokens", out.Usage.OutputTokens),
			attribute.String("switchyard.alias", alias),
			attribute.String("switchyard.provider", out.Provider),
			attribute.String("switchyard.cache", out.Cache),
			attribute.Int("switchyard.attempts", out.Attempts),
			attribute.Int("switchyard.fallback_depth", out.FallbackDepth),
			attribute.Float64("switchyard.cost_usd", cost),
			attribute.Int("http.response.status_code", out.Status),
		)
		if out.Response != nil && out.Response.StopReason != "" {
			span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", []string{out.Response.StopReason}))
		}
		if out.Err != nil {
			span.SetStatus(codes.Error, out.Err.Error())
			span.SetAttributes(attribute.String("error.type", errorType(out.Err)))
		}
		if capture {
			in, outMsgs := contentJSON(c.Req, out.Response)
			span.SetAttributes(attribute.String("gen_ai.input.messages", in), attribute.String("gen_ai.output.messages", outMsgs))
		}
		span.End()
	}
	if capture {
		in, outMsgs := contentJSON(c.Req, out.Response)
		o.Capture.Info("capture", "request_id", c.ID, "tenant", c.Key.TenantID, "input", in, "output", outMsgs)
	}
}

func errorType(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Status > 0 {
			return strconv.Itoa(pe.Status)
		}
		return "transport"
	}
	var ge *gateway.Error
	if errors.As(err, &ge) {
		return ge.Type
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "_OTHER"
}

// contentJSON renders the conversation in the GenAI message format, redacted.
func contentJSON(req *llm.Request, resp *llm.Response) (string, string) {
	type part struct {
		Type    string `json:"type"`
		Content string `json:"content,omitempty"`
		Name    string `json:"name,omitempty"`
	}
	type msg struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}
	conv := func(role string, blocks []llm.Block) msg {
		m := msg{Role: role}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				m.Parts = append(m.Parts, part{Type: "text", Content: Redact(b.Text)})
			case "tool_use":
				m.Parts = append(m.Parts, part{Type: "tool_call", Name: b.Name, Content: Redact(string(b.Input))})
			case "tool_result":
				m.Parts = append(m.Parts, part{Type: "tool_call_response", Content: Redact(b.ToolResultText())})
			default:
				m.Parts = append(m.Parts, part{Type: b.Type})
			}
		}
		return m
	}
	var in []msg
	if len(req.System) > 0 {
		in = append(in, conv("system", req.System))
	}
	for _, m := range req.Messages {
		in = append(in, conv(m.Role, m.Content))
	}
	ib, _ := json.Marshal(in)
	ob := []byte("[]")
	if resp != nil {
		ob, _ = json.Marshal([]msg{conv("assistant", resp.Content)})
	}
	return string(ib), string(ob)
}

// Attempt is the router's per-attempt hook: a client span per upstream call
// and the upstream latency histogram.
func (o *Observer) Attempt(ctx context.Context, t router.Target) (context.Context, func(error)) {
	start := time.Now()
	ctx, span := o.Tracer.Start(ctx, "chat "+t.Model, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", genAIProvider(t.Provider.Kind())),
		attribute.String("gen_ai.request.model", t.Model),
		attribute.String("switchyard.provider", t.Provider.Name()),
	))
	return ctx, func(err error) {
		outcome := "ok"
		if err != nil {
			outcome = "error"
			if errors.Is(err, context.Canceled) {
				outcome = "cancelled"
			}
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String("error.type", errorType(err)))
		}
		o.M.UpstreamDuration.WithLabelValues(t.Provider.Name(), t.Model, outcome).Observe(time.Since(start).Seconds())
		span.End()
	}
}

// WatchBreakers exports breaker state and transitions.
func (o *Observer) WatchBreakers(bs map[string]*breaker.Breaker) {
	for name, b := range bs {
		o.M.BreakerState.WithLabelValues(name).Set(0)
		b.OnChange = func(_, to breaker.State) {
			o.M.BreakerState.WithLabelValues(name).Set(float64(to))
			o.M.BreakerChanges.WithLabelValues(name, to.String()).Inc()
		}
	}
}

// Transport injects the trace context into upstream requests.
type Transport struct{ Base http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(r.Header))
	return t.Base.RoundTrip(r)
}
