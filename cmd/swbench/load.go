package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

const chatBody = `{"model":"%s","max_tokens":32,"messages":[{"role":"user","content":"Summarise why a gateway should never retry a stream after the first token."}]}`
const messagesBody = `{"model":"%s","max_tokens":32,"messages":[{"role":"user","content":"Summarise why a gateway should never retry a stream after the first token."}]}`

func (e *env) target(direct bool, surface string) vegeta.Target {
	h := http.Header{"Content-Type": {"application/json"}}
	switch {
	case direct && surface == "openai":
		return vegeta.Target{Method: "POST", URL: e.mockA + "/v1/chat/completions", Body: []byte(fmt.Sprintf(chatBody, "mock-fast")), Header: h}
	case direct:
		return vegeta.Target{Method: "POST", URL: e.mockA + "/v1/messages", Body: []byte(fmt.Sprintf(messagesBody, "mock-fast")), Header: h}
	case surface == "openai":
		h.Set("Authorization", "Bearer "+e.key)
		return vegeta.Target{Method: "POST", URL: e.gateway + "/v1/chat/completions", Body: []byte(fmt.Sprintf(chatBody, "bench-oai")), Header: h}
	default:
		h.Set("x-api-key", e.key)
		return vegeta.Target{Method: "POST", URL: e.gateway + "/v1/messages", Body: []byte(fmt.Sprintf(messagesBody, "bench-anth")), Header: h}
	}
}

type latency struct {
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

type attackResult struct {
	Rate       float64  `json:"rate"`
	Requests   uint64   `json:"requests"`
	Success    float64  `json:"success_ratio"`
	Throughput float64  `json:"throughput"`
	Latency    latency  `json:"latency"`
	Errors     []string `json:"errors,omitempty"`
}

// attack runs a constant-rate load (open model: requests are sent on schedule
// whether or not earlier ones finished).
func (e *env) attack(t vegeta.Target, rate int, d time.Duration) attackResult {
	a := vegeta.NewAttacker(vegeta.KeepAlive(true), vegeta.Connections(10000), vegeta.MaxConnections(10000),
		vegeta.Workers(64), vegeta.Timeout(30*time.Second))
	var m vegeta.Metrics
	for res := range a.Attack(vegeta.NewStaticTargeter(t), vegeta.ConstantPacer{Freq: rate, Per: time.Second}, d, "") {
		m.Add(res)
	}
	m.Close()
	l := m.Latencies
	return attackResult{Rate: m.Rate, Requests: m.Requests, Success: m.Success, Throughput: m.Throughput, Errors: m.Errors,
		Latency: latency{P50: ms(l.P50), P90: ms(l.P90), P95: ms(l.P95), P99: ms(l.P99), Max: ms(l.Max), Mean: ms(l.Mean)}}
}

type overheadRow struct {
	Surface    string       `json:"surface"`
	Rate       int          `json:"target_rps"`
	Direct     attackResult `json:"direct"`
	Gateway    attackResult `json:"gateway"`
	AddedP50   float64      `json:"added_p50_ms"`
	AddedP95   float64      `json:"added_p95_ms"`
	AddedP99   float64      `json:"added_p99_ms"`
	GatewayCPU float64      `json:"gateway_cpu_cores"`
	GatewayRSS float64      `json:"gateway_rss_mb"`
}

// overhead compares the mock called directly with the same call through the
// gateway at increasing constant request rates. The mock answers in 20 ms.
func (e *env) overhead() (any, error) {
	if err := e.mockConfig(e.mockA, map[string]any{"latency_ms": 20}); err != nil {
		return nil, err
	}
	rates := []int{250, 500, 1000, 2000, 4000}
	d := 20 * time.Second
	if e.quick {
		rates, d = []int{100, 500}, 3*time.Second
	}
	var rows []overheadRow
	for _, surface := range []string{"openai", "anthropic"} {
		e.attack(e.target(false, surface), 200, 3*time.Second) // warm up connections and caches
		for _, r := range rates {
			direct := e.attack(e.target(true, surface), r, d)
			before, t0 := e.gatewayStats(), time.Now()
			gw := e.attack(e.target(false, surface), r, d)
			after := e.gatewayStats()
			row := overheadRow{Surface: surface, Rate: r, Direct: direct, Gateway: gw,
				AddedP50: gw.Latency.P50 - direct.Latency.P50, AddedP95: gw.Latency.P95 - direct.Latency.P95, AddedP99: gw.Latency.P99 - direct.Latency.P99,
				GatewayCPU: (after.CPUSeconds - before.CPUSeconds) / time.Since(t0).Seconds(), GatewayRSS: after.RSSBytes / 1e6}
			log.Printf("%s %5d rps: direct p50 %.2f p99 %.2f | gateway p50 %.2f p99 %.2f ok %.4f | +p50 %.2f +p99 %.2f ms, %.2f cores",
				surface, r, direct.Latency.P50, direct.Latency.P99, gw.Latency.P50, gw.Latency.P99, gw.Success, row.AddedP50, row.AddedP99, row.GatewayCPU)
			rows = append(rows, row)
		}
	}
	return map[string]any{"mock_latency_ms": 20, "duration_s": d.Seconds(), "rows": rows}, nil
}

type throughputRow struct {
	Target      string  `json:"target"`
	Concurrency int     `json:"concurrency"`
	RPS         float64 `json:"rps"`
	ErrorRatio  float64 `json:"error_ratio"`
	P50         float64 `json:"p50_ms"`
	P99         float64 `json:"p99_ms"`
	GatewayCPU  float64 `json:"gateway_cpu_cores,omitempty"`
	GatewayRSS  float64 `json:"gateway_rss_mb,omitempty"`
}

// closedLoop runs n workers that each send the next request as soon as the
// previous one returns, for d.
func (e *env) closedLoop(t vegeta.Target, n int, d time.Duration) (float64, float64, []time.Duration) {
	client := newClient(n)
	var ok, failed atomic.Int64
	var mu sync.Mutex
	var lats []time.Duration
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 4096)
			for time.Now().Before(stop) {
				req, _ := http.NewRequest(t.Method, t.URL, bytes.NewReader(t.Body))
				req.Header = t.Header.Clone()
				start := time.Now()
				resp, err := client.Do(req)
				if err == nil {
					_, _ = ioDiscard(resp)
				}
				if err != nil || resp.StatusCode != 200 {
					failed.Add(1)
					continue
				}
				ok.Add(1)
				local = append(local, time.Since(start))
			}
			mu.Lock()
			lats = append(lats, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	total := float64(ok.Load() + failed.Load())
	errRatio := 0.0
	if total > 0 {
		errRatio = float64(failed.Load()) / total
	}
	return float64(ok.Load()) / d.Seconds(), errRatio, lats
}

func pct(lats []time.Duration, p float64) float64 {
	if len(lats) == 0 {
		return 0
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	i := int(float64(len(lats)-1) * p)
	return ms(lats[i])
}

// throughput finds how many requests per second the gateway serves with the
// mock answering instantly, at increasing client concurrency.
func (e *env) throughput() (any, error) {
	if err := e.mockConfig(e.mockA, map[string]any{"latency_ms": 0}); err != nil {
		return nil, err
	}
	levels := []int{16, 64, 256, 1024}
	d := 15 * time.Second
	if e.quick {
		levels, d = []int{16, 64}, 3*time.Second
	}
	var rows []throughputRow
	for _, c := range levels {
		rps, errs, lats := e.closedLoop(e.target(true, "openai"), c, d)
		rows = append(rows, throughputRow{Target: "mock direct", Concurrency: c, RPS: rps, ErrorRatio: errs, P50: pct(lats, .5), P99: pct(lats, .99)})
		before, t0 := e.gatewayStats(), time.Now()
		rps, errs, lats = e.closedLoop(e.target(false, "openai"), c, d)
		after := e.gatewayStats()
		row := throughputRow{Target: "gateway", Concurrency: c, RPS: rps, ErrorRatio: errs, P50: pct(lats, .5), P99: pct(lats, .99),
			GatewayCPU: (after.CPUSeconds - before.CPUSeconds) / time.Since(t0).Seconds(), GatewayRSS: after.RSSBytes / 1e6}
		rows = append(rows, row)
		log.Printf("concurrency %4d: mock %.0f rps | gateway %.0f rps, errors %.4f, p99 %.1f ms, %.2f cores, %.0f MB",
			c, rows[len(rows)-2].RPS, row.RPS, row.ErrorRatio, row.P99, row.GatewayCPU, row.GatewayRSS)
	}
	return map[string]any{"duration_s": d.Seconds(), "rows": rows}, nil
}
