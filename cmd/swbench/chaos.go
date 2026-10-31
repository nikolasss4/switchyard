package main

import (
	"bytes"
	"log"
	"net/http"
	"sync"
	"time"
)

type chaosSecond struct {
	T          int     `json:"t"`
	Phase      string  `json:"phase"`
	Sent       int     `json:"sent"`
	OK         int     `json:"ok"`
	Failed     int     `json:"failed"`
	ServedByA  int     `json:"served_by_primary"`
	ServedByB  int     `json:"served_by_secondary"`
	P99        float64 `json:"p99_ms"`
	BreakerA   string  `json:"primary_breaker"`
	DirectSent int     `json:"direct_sent"`
	DirectOK   int     `json:"direct_ok"`
	lats       []time.Duration
}

type chaosPhase struct {
	Name     string         `json:"name"`
	From, To int            `json:"-"`
	FromS    int            `json:"from_s"`
	ToS      int            `json:"to_s"`
	Mock     map[string]any `json:"-"`
}

// chaos sends constant load at the "chaos" alias (primary mock A, fallback
// mock B) while A degrades: healthy, then 50% errors, then fully down, then
// healthy again. The same load goes straight to A as a baseline.
func (e *env) chaos() (any, error) {
	phases := []chaosPhase{
		{Name: "healthy", From: 0, To: 20},
		{Name: "primary 50% errors", From: 20, To: 45, Mock: map[string]any{"latency_ms": 20, "error_rate": 0.5, "error_status": 503}},
		{Name: "primary down", From: 45, To: 70, Mock: map[string]any{"latency_ms": 20, "down": true}},
		{Name: "primary recovered", From: 70, To: 100},
	}
	rate := 200
	if e.quick {
		phases = []chaosPhase{{Name: "healthy", From: 0, To: 4}, {Name: "primary 50% errors", From: 4, To: 8, Mock: map[string]any{"latency_ms": 20, "error_rate": 0.5, "error_status": 503}},
			{Name: "primary down", From: 8, To: 14, Mock: map[string]any{"latency_ms": 20, "down": true}}, {Name: "primary recovered", From: 14, To: 22}}
		rate = 100
	}
	for i := range phases {
		phases[i].FromS, phases[i].ToS = phases[i].From, phases[i].To
	}
	total := phases[len(phases)-1].To
	for _, m := range []string{e.mockA, e.mockB} {
		if err := e.mockConfig(m, map[string]any{"latency_ms": 20}); err != nil {
			return nil, err
		}
	}
	time.Sleep(6 * time.Second) // let a breaker left open by an earlier run close

	secs := make([]chaosSecond, total)
	var mu sync.Mutex
	client := newClient(4096)
	direct := newClient(4096)
	gwBody := []byte(`{"model":"chaos","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	directBody := []byte(`{"model":"mock-fast","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)

	// Breaker sampler.
	stop := make(chan struct{})
	start := time.Now()
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				var out struct {
					Providers []struct{ Name, State string }
				}
				if e.adminDo("GET", "/admin/api/providers", nil, &out) != nil {
					continue
				}
				sec := int(time.Since(start).Seconds())
				if sec >= total {
					continue
				}
				for _, p := range out.Providers {
					if p.Name == "mock-a" {
						mu.Lock()
						// Keep the worst state seen in the second.
						if rank(p.State) > rank(secs[sec].BreakerA) {
							secs[sec].BreakerA = p.State
						}
						mu.Unlock()
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	interval := time.Second / time.Duration(rate)
	phase := -1
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for now := range tick.C {
		el := now.Sub(start)
		sec := int(el.Seconds())
		if sec >= total {
			break
		}
		for i, p := range phases {
			if sec >= p.From && sec < p.To && i != phase {
				phase = i
				cfg := p.Mock
				if cfg == nil {
					cfg = map[string]any{"latency_ms": 20}
				}
				if err := e.mockConfig(e.mockA, cfg); err != nil {
					log.Printf("mock config: %v", err)
				}
				log.Printf("t=%ds %s", sec, p.Name)
			}
		}
		wg.Add(2)
		go func(sec int) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", e.gateway+"/v1/chat/completions", bytes.NewReader(gwBody))
			req.Header.Set("Authorization", "Bearer "+e.key)
			req.Header.Set("Content-Type", "application/json")
			t0 := time.Now()
			resp, err := client.Do(req)
			ok, by := false, ""
			if err == nil {
				ok, by = resp.StatusCode == 200, resp.Header.Get("X-Switchyard-Provider")
				_, _ = ioDiscard(resp)
			}
			mu.Lock()
			s := &secs[sec]
			s.Sent++
			if ok {
				s.OK++
				s.lats = append(s.lats, time.Since(t0))
				if by == "mock-a" {
					s.ServedByA++
				} else {
					s.ServedByB++
				}
			} else {
				s.Failed++
			}
			mu.Unlock()
		}(sec)
		go func(sec int) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", e.mockA+"/v1/chat/completions", bytes.NewReader(directBody))
			req.Header.Set("Content-Type", "application/json")
			resp, err := direct.Do(req)
			ok := false
			if err == nil {
				ok = resp.StatusCode == 200
				_, _ = ioDiscard(resp)
			}
			mu.Lock()
			secs[sec].DirectSent++
			if ok {
				secs[sec].DirectOK++
			}
			mu.Unlock()
		}(sec)
	}
	wg.Wait()
	close(stop)
	_ = e.mockConfig(e.mockA, map[string]any{"latency_ms": 20})

	type phaseSummary struct {
		chaosPhase
		Sent, OK, Failed, DirectSent, DirectOK int
		SuccessRatio                           float64 `json:"success_ratio"`
		DirectSuccessRatio                     float64 `json:"direct_success_ratio"`
		ServedBySecondary                      float64 `json:"served_by_secondary_ratio"`
		P99                                    float64 `json:"p99_ms"`
	}
	var sums []phaseSummary
	totalSent, totalOK := 0, 0
	for _, p := range phases {
		ps := phaseSummary{chaosPhase: p}
		var lats []time.Duration
		servedB := 0
		for s := p.From; s < p.To; s++ {
			secs[s].T, secs[s].Phase = s, p.Name
			secs[s].P99 = pct(secs[s].lats, .99)
			if secs[s].BreakerA == "" {
				secs[s].BreakerA = "closed"
			}
			ps.Sent += secs[s].Sent
			ps.OK += secs[s].OK
			ps.Failed += secs[s].Failed
			ps.DirectSent += secs[s].DirectSent
			ps.DirectOK += secs[s].DirectOK
			servedB += secs[s].ServedByB
			lats = append(lats, secs[s].lats...)
		}
		ps.SuccessRatio = float64(ps.OK) / float64(max(1, ps.Sent))
		ps.DirectSuccessRatio = float64(ps.DirectOK) / float64(max(1, ps.DirectSent))
		ps.ServedBySecondary = float64(servedB) / float64(max(1, ps.OK))
		ps.P99 = pct(lats, .99)
		totalSent += ps.Sent
		totalOK += ps.OK
		log.Printf("%-20s gateway %d/%d ok (%.2f%%), direct %.2f%%, %.0f%% via secondary, p99 %.1f ms",
			p.Name, ps.OK, ps.Sent, 100*ps.SuccessRatio, 100*ps.DirectSuccessRatio, 100*ps.ServedBySecondary, ps.P99)
		sums = append(sums, ps)
	}

	// Breaker timing: first open second after the primary went down or degraded,
	// first second after recovery where the primary served traffic again.
	firstOpen, backToPrimary := -1, -1
	for _, s := range secs {
		if firstOpen < 0 && s.T >= phases[1].From && s.BreakerA == "open" {
			firstOpen = s.T
		}
		if backToPrimary < 0 && s.T >= phases[3].From && s.ServedByA > s.ServedByB {
			backToPrimary = s.T
		}
	}
	return map[string]any{
		"rate_rps": rate, "phases": sums, "timeline": secs,
		"total_sent": totalSent, "total_ok": totalOK, "success_ratio": float64(totalOK) / float64(max(1, totalSent)),
		"breaker_first_open_s_after_degradation":            firstOpen - phases[1].From,
		"seconds_until_primary_serves_again_after_recovery": backToPrimary - phases[3].From,
		"breaker": "window 20, min 10 calls, opens at 50% failures, 5 s open, 3 half-open probes",
		"retry":   "2 attempts per target, 25-250 ms full-jitter backoff",
	}, nil
}

func rank(state string) int {
	switch state {
	case "open":
		return 3
	case "half_open":
		return 2
	case "closed":
		return 1
	}
	return 0
}
