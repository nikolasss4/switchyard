package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func ioDiscard(resp *http.Response) (int64, error) {
	defer resp.Body.Close()
	return io.Copy(io.Discard, resp.Body)
}

// streamOnce sends a streaming request and returns the time to the first
// content event and whether the stream finished cleanly.
func (e *env) streamOnce(client *http.Client, direct bool, surface string, outputTokens int) (time.Duration, bool) {
	var url, body string
	h := http.Header{"Content-Type": {"application/json"}}
	if surface == "openai" {
		url, body = e.mockA+"/v1/chat/completions", fmt.Sprintf(`{"model":"%s","stream":true,"max_tokens":%d,"messages":[{"role":"user","content":"stream please"}]}`, "mock-fast", outputTokens)
		if !direct {
			url, body = e.gateway+"/v1/chat/completions", strings.Replace(body, "mock-fast", "bench-oai", 1)
			h.Set("Authorization", "Bearer "+e.key)
		}
	} else {
		url, body = e.mockA+"/v1/messages", fmt.Sprintf(`{"model":"%s","stream":true,"max_tokens":%d,"messages":[{"role":"user","content":"stream please"}]}`, "mock-fast", outputTokens)
		if !direct {
			url, body = e.gateway+"/v1/messages", strings.Replace(body, "mock-fast", "bench-anth", 1)
			h.Set("x-api-key", e.key)
		}
	}
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	req.Header = h
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ttft time.Duration
	done := false
	for sc.Scan() {
		line := sc.Text()
		if ttft == 0 && (strings.Contains(line, `"content_block_delta"`) || (strings.Contains(line, `"delta":{"content":"`) && !strings.Contains(line, `"content":""`))) {
			ttft = time.Since(start)
		}
		if line == "data: [DONE]" || strings.Contains(line, `"type":"message_stop"`) {
			done = true
		}
	}
	return ttft, done && ttft > 0
}

type ttftRow struct {
	Surface  string  `json:"surface"`
	Direct   latency `json:"direct_ttft"`
	Gateway  latency `json:"gateway_ttft"`
	AddedP50 float64 `json:"added_p50_ms"`
	AddedP99 float64 `json:"added_p99_ms"`
	Samples  int     `json:"samples"`
}

type concurrencyRow struct {
	Streams        int     `json:"streams"`
	Completed      int64   `json:"completed"`
	Failed         int64   `json:"failed"`
	TTFTP50        float64 `json:"ttft_p50_ms"`
	TTFTP99        float64 `json:"ttft_p99_ms"`
	PeakRSSMB      float64 `json:"gateway_peak_rss_mb"`
	PeakGoroutines float64 `json:"gateway_peak_goroutines"`
	GatewayCPU     float64 `json:"gateway_cpu_cores"`
	StreamSeconds  float64 `json:"stream_seconds"`
	RSSPerStreamKB float64 `json:"rss_per_stream_kb"`
}

func lat(ds []time.Duration) latency {
	return latency{P50: pct(ds, .5), P90: pct(ds, .9), P95: pct(ds, .95), P99: pct(ds, .99), Max: pct(ds, 1)}
}

// stream measures the time to first token the gateway adds, and how many
// concurrent streams it holds.
func (e *env) stream() (any, error) {
	// TTFT: the mock waits 100 ms, then streams 50 tokens at 200 tokens/s.
	if err := e.mockConfig(e.mockA, map[string]any{"latency_ms": 100, "tokens_per_second": 200, "output_tokens": 50}); err != nil {
		return nil, err
	}
	n := 400
	if e.quick {
		n = 40
	}
	client := newClient(256)
	var rows []ttftRow
	for _, surface := range []string{"openai", "anthropic"} {
		var direct, gw []time.Duration
		for i := range n {
			// Interleave so drift on the machine hits both sides equally; 20 in flight.
			var wg sync.WaitGroup
			var mu sync.Mutex
			if i%20 != 0 {
				continue
			}
			for range 20 {
				wg.Add(2)
				go func() {
					defer wg.Done()
					if d, ok := e.streamOnce(client, true, surface, 50); ok {
						mu.Lock()
						direct = append(direct, d)
						mu.Unlock()
					}
				}()
				go func() {
					defer wg.Done()
					if d, ok := e.streamOnce(client, false, surface, 50); ok {
						mu.Lock()
						gw = append(gw, d)
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
		}
		row := ttftRow{Surface: surface, Direct: lat(direct), Gateway: lat(gw), Samples: len(gw)}
		row.AddedP50, row.AddedP99 = row.Gateway.P50-row.Direct.P50, row.Gateway.P99-row.Direct.P99
		log.Printf("ttft %s: direct p50 %.2f p99 %.2f | gateway p50 %.2f p99 %.2f (%d samples)", surface, row.Direct.P50, row.Direct.P99, row.Gateway.P50, row.Gateway.P99, row.Samples)
		rows = append(rows, row)
	}

	// Concurrency: each stream lasts about 10 s (100 tokens at 10 tokens/s).
	if err := e.mockConfig(e.mockA, map[string]any{"latency_ms": 0, "tokens_per_second": 10, "output_tokens": 100}); err != nil {
		return nil, err
	}
	levels := []int{1000, 5000, 10000}
	if e.quick {
		levels = []int{200}
	}
	var conc []concurrencyRow
	for _, streams := range levels {
		conc = append(conc, e.concurrentStreams(streams))
		time.Sleep(3 * time.Second) // let connections drain
	}
	return map[string]any{"ttft": rows, "ttft_mock": "100 ms to first byte, 200 tokens/s",
		"concurrency": conc, "concurrency_mock": "100 tokens at 10 tokens/s (about 10 s per stream)"}, nil
}

func (e *env) concurrentStreams(n int) concurrencyRow {
	client := newClient(n + 16)
	base := e.gatewayStats()
	var completed, failed atomic.Int64
	var mu sync.Mutex
	var ttfts []time.Duration
	var peak processStats
	stopSampling := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-t.C:
				s := e.gatewayStats()
				if s.RSSBytes > peak.RSSBytes {
					peak.RSSBytes = s.RSSBytes
				}
				if s.Goroutines > peak.Goroutines {
					peak.Goroutines = s.Goroutines
				}
			}
		}
	}()
	start := time.Now()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 2 * time.Second / time.Duration(n)) // spread opens over 2 s
			d, ok := e.streamOnce(client, false, "openai", 100)
			if ok {
				completed.Add(1)
				mu.Lock()
				ttfts = append(ttfts, d)
				mu.Unlock()
			} else {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stopSampling)
	end := e.gatewayStats()
	row := concurrencyRow{Streams: n, Completed: completed.Load(), Failed: failed.Load(), TTFTP50: pct(ttfts, .5), TTFTP99: pct(ttfts, .99),
		PeakRSSMB: peak.RSSBytes / 1e6, PeakGoroutines: peak.Goroutines, StreamSeconds: elapsed.Seconds(),
		GatewayCPU: (end.CPUSeconds - base.CPUSeconds) / elapsed.Seconds()}
	if n > 0 {
		row.RSSPerStreamKB = (peak.RSSBytes - base.RSSBytes) / float64(n) / 1e3
	}
	log.Printf("%d concurrent streams: %d ok, %d failed, ttft p99 %.1f ms, peak RSS %.0f MB, %.0f goroutines, %.2f cores",
		n, row.Completed, row.Failed, row.TTFTP99, row.PeakRSSMB, row.PeakGoroutines, row.GatewayCPU)
	return row
}
