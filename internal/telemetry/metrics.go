// Package telemetry holds the gateway's Prometheus metrics, OpenTelemetry
// tracing (GenAI semantic conventions) and opt-in prompt capture.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics are the gateway's Prometheus collectors.
type Metrics struct {
	Registry *prometheus.Registry

	Requests         *prometheus.CounterVec
	Duration         *prometheus.HistogramVec
	TTFT             *prometheus.HistogramVec
	UpstreamDuration *prometheus.HistogramVec
	Tokens           *prometheus.CounterVec
	Cost             *prometheus.CounterVec
	Cache            *prometheus.CounterVec
	Fallbacks        *prometheus.CounterVec
	Hedges           *prometheus.CounterVec
	Refused          *prometheus.CounterVec
	BreakerState     *prometheus.GaugeVec
	BreakerChanges   *prometheus.CounterVec
	InFlight         prometheus.Gauge
	Streams          prometheus.Gauge
	UsageDropped     prometheus.Counter
}

// Latency buckets from 1 ms to 2 min: the gateway's own overhead lives at the
// bottom, model latency at the top.
var latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 40, 80, 120}

// NewMetrics registers every collector on a fresh registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_requests_total",
			Help: "Requests by surface, alias, serving provider and model, HTTP status class and cache result."},
			[]string{"surface", "alias", "provider", "model", "code", "cache"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "switchyard_request_duration_seconds",
			Help: "End-to-end request duration as seen by the gateway.", Buckets: latencyBuckets},
			[]string{"alias", "provider", "model", "stream"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "switchyard_time_to_first_token_seconds",
			Help: "Time from request arrival to the first content event of a stream.", Buckets: latencyBuckets},
			[]string{"alias", "provider", "model"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "switchyard_upstream_attempt_duration_seconds",
			Help: "Duration of each upstream attempt (to the first token for streams).", Buckets: latencyBuckets},
			[]string{"provider", "model", "outcome"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_tokens_total",
			Help: "Tokens reported by providers."}, []string{"alias", "provider", "model", "type"}),
		Cost: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_cost_usd_total",
			Help: "Cost in USD from the price table."}, []string{"alias", "provider", "model", "tenant"}),
		Cache: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_cache_requests_total",
			Help: "Cache lookups on aliases with a cache: hit_exact, hit_semantic or miss."}, []string{"alias", "result"}),
		Fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_fallbacks_total",
			Help: "Requests served by a target other than the first, by depth."}, []string{"alias", "provider", "depth"}),
		Hedges: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_hedged_requests_total",
			Help: "Requests where a hedge lane was started."}, []string{"alias"}),
		Refused: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_refused_total",
			Help: "Requests refused before reaching a provider."}, []string{"reason"}),
		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "switchyard_breaker_state",
			Help: "Circuit breaker state per provider: 0 closed, 1 open, 2 half-open."}, []string{"provider"}),
		BreakerChanges: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_breaker_transitions_total",
			Help: "Circuit breaker transitions."}, []string{"provider", "to"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "switchyard_inflight_requests", Help: "Requests in progress."}),
		Streams:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "switchyard_active_streams", Help: "Open client streams."}),
		UsageDropped: prometheus.NewCounter(prometheus.CounterOpts{Name: "switchyard_usage_records_dropped_total",
			Help: "Usage records dropped because the write buffer was full."}),
	}
	m.Registry.MustRegister(m.Requests, m.Duration, m.TTFT, m.UpstreamDuration, m.Tokens, m.Cost, m.Cache, m.Fallbacks,
		m.Hedges, m.Refused, m.BreakerState, m.BreakerChanges, m.InFlight, m.Streams, m.UsageDropped,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}
