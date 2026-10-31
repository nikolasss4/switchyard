package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/gateway"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/router"
)

func observer(t *testing.T) (*Observer, *tracetest.InMemoryExporter, *strings.Builder) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	var buf strings.Builder
	return &Observer{M: NewMetrics(), Tracer: tp.Tracer("test"), Capture: slog.New(slog.NewJSONHandler(&buf, nil))}, exp, &buf
}

func call(capture bool) *gateway.Call {
	req := &llm.Request{Model: "fast", MaxTokens: 50, Messages: []llm.Message{{Role: "user", Content: []llm.Block{{Type: "text", Text: "my email is a@b.co, secret plans"}}}}}
	return &gateway.Call{ID: "req_1", Surface: "openai", Req: req, Start: time.Now().Add(-time.Second),
		Key:   &auth.Key{ID: "k1", TenantID: "acme", CapturePrompts: capture},
		Route: &router.Route{Alias: "fast", Model: &config.Model{Alias: "fast"}}}
}

func outcome() *gateway.Outcome {
	return &gateway.Outcome{Status: 200, Provider: "anthropic", Model: "claude-haiku-5-5", Attempts: 2, FallbackDepth: 1, CacheEligible: true,
		Usage:    llm.Usage{InputTokens: 10, OutputTokens: 20, CacheReadInputTokens: 5},
		Response: &llm.Response{StopReason: "end_turn", Content: []llm.Block{{Type: "text", Text: "the answer"}}}}
}

func attrs(kv []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, a := range kv {
		m[string(a.Key)] = a.Value.Emit()
	}
	return m
}

func TestSpanFollowsGenAIConventionsWithoutContent(t *testing.T) {
	o, exp, logs := observer(t)
	c := call(false)
	o.Start(context.Background(), c)
	o.Finish(c, outcome(), 0.001)
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "chat fast" {
		t.Fatalf("spans = %v", spans)
	}
	a := attrs(spans[0].Attributes)
	for k, want := range map[string]string{
		"gen_ai.operation.name": "chat", "gen_ai.request.model": "fast", "gen_ai.response.model": "claude-haiku-5-5",
		"gen_ai.usage.input_tokens": "15", "gen_ai.usage.output_tokens": "20", "gen_ai.response.finish_reasons": `["end_turn"]`,
		"switchyard.fallback_depth": "1", "switchyard.tenant": "acme",
	} {
		if a[k] != want {
			t.Errorf("%s = %q, want %q", k, a[k], want)
		}
	}
	if _, ok := a["gen_ai.input.messages"]; ok || logs.Len() != 0 {
		t.Fatal("content must not be recorded unless the tenant opted in")
	}
	if got := testutil.ToFloat64(o.M.Fallbacks.WithLabelValues("fast", "anthropic", "1")); got != 1 {
		t.Fatalf("fallbacks = %v", got)
	}
	if got := testutil.ToFloat64(o.M.Cache.WithLabelValues("fast", "miss")); got != 1 {
		t.Fatalf("cache misses = %v", got)
	}
	if got := testutil.ToFloat64(o.M.Tokens.WithLabelValues("fast", "anthropic", "claude-haiku-5-5", "cache_read")); got != 5 {
		t.Fatalf("cache_read tokens = %v", got)
	}
	if got := testutil.ToFloat64(o.M.InFlight); got != 0 {
		t.Fatalf("in flight = %v", got)
	}
}

func TestCaptureIsRedacted(t *testing.T) {
	o, exp, logs := observer(t)
	c := call(true)
	o.Start(context.Background(), c)
	o.Finish(c, outcome(), 0)
	in := attrs(exp.GetSpans()[0].Attributes)["gen_ai.input.messages"]
	if !strings.Contains(in, "secret plans") || strings.Contains(in, "a@b.co") || !strings.Contains(logs.String(), "the answer") {
		t.Fatalf("span input %q, log %q", in, logs.String())
	}
}
