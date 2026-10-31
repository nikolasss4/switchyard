package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/breaker"
	"github.com/nikolasss4/switchyard/internal/provider"
)

const reliable = `
retry: {max_attempts: 3, base_delay: 1ms, max_delay: 50ms}
breaker: {window: 10, min_requests: 2, failure_rate: 0.5, open_for: 1h, half_open_probes: 1}
providers:
  - {name: a, type: openai, base_url: http://a}
  - {name: b, type: openai, base_url: http://b}
models:
  - alias: chain
    targets: [{provider: a, model: a-1}, {provider: b, model: b-1}]
  - alias: hedged
    hedge_after: 20ms
    targets: [{provider: a, model: a-1}, {provider: b, model: b-1}]
  - alias: solo
    hedge_after: 20ms
    targets: [{provider: a, model: a-1}]
  - alias: tight
    timeout: 30ms
    targets: [{provider: a, model: a-1}, {provider: b, model: b-1}]
`

func complete(t *testing.T, r *Router, alias string) (string, *Result, error) {
	t.Helper()
	route, err := r.Resolve(alias, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, res, err := r.Complete(context.Background(), route, req)
	if err != nil {
		return "", res, err
	}
	return resp.ID, res, nil
}

func TestRetriesSameTargetThenSucceeds(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(503), ok()}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	id, res, err := complete(t, r, "chain")
	if err != nil || id != "a" || res.Attempts != 2 || res.FallbackDepth != 0 || b.calls.Load() != 0 {
		t.Fatalf("id %s res %+v err %v", id, res, err)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(422)}}
	r := setup(t, reliable, a, &fake{name: "b", script: []behaviour{ok()}})
	_, res, err := complete(t, r, "chain")
	if err == nil || a.calls.Load() != 1 || res.Attempts != 1 {
		t.Fatalf("422 must fail at once: err %v calls %d", err, a.calls.Load())
	}
}

func TestLongRetryAfterFallsBackInsteadOfWaiting(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{{err: &provider.Error{Provider: "a", Status: 429, RetryAfter: 30 * time.Second}}}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	start := time.Now()
	id, _, err := complete(t, r, "chain")
	if err != nil || id != "b" || a.calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("id %s err %v a calls %d took %v", id, err, a.calls.Load(), time.Since(start))
	}
}

func TestBackoffIsJitteredAndCapped(t *testing.T) {
	r := setup(t, reliable, &fake{name: "a"}, &fake{name: "b"})
	r.rnd = func() float64 { return 1 }
	err := errors.New("x")
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 32, 50, 50} {
		if got := r.backoff(i+1, err); got != want*time.Millisecond {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, want*time.Millisecond)
		}
	}
	r.rnd = func() float64 { return 0.25 }
	if got := r.backoff(3, err); got != time.Millisecond {
		t.Errorf("jittered backoff = %v", got)
	}
	ra := &provider.Error{RetryAfter: 40 * time.Millisecond}
	if got := r.backoff(1, ra); got != 40*time.Millisecond {
		t.Errorf("Retry-After must win when longer: %v", got)
	}
}

func TestBreakerSkipsOpenProvider(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{fail(500)}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	if _, _, err := complete(t, r, "chain"); err != nil { // a fails 3x (trips after 2), b serves
		t.Fatal(err)
	}
	if st := r.Breakers()["a"].State(); st != breaker.Open {
		t.Fatalf("a breaker = %v", st)
	}
	before := a.calls.Load()
	id, res, err := complete(t, r, "chain")
	if err != nil || id != "b" || a.calls.Load() != before || res.Attempts != 1 || res.FallbackDepth != 1 {
		t.Fatalf("open breaker must skip a without calling it: id %s res %+v calls %d->%d", id, res, before, a.calls.Load())
	}

	// With every breaker open the router fails fast with ErrCircuitOpen.
	b.script = []behaviour{fail(500)}
	for range 3 {
		_, _, _ = complete(t, r, "chain")
	}
	_, _, err = complete(t, r, "chain")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("all open: %v", err)
	}
}

func TestHedgeWinsOverSlowPrimary(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{slow(2 * time.Second)}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	start := time.Now()
	id, res, err := complete(t, r, "hedged")
	if err != nil || id != "b" || !res.Hedged || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("id %s res %+v err %v took %v", id, res, err, time.Since(start))
	}
	// The cancelled loser must not count against a's breaker.
	time.Sleep(20 * time.Millisecond)
	if snap := r.Breakers()["a"].Snapshot(); snap.Calls != 0 {
		t.Fatalf("cancelled hedge was recorded: %+v", snap)
	}
}

func TestHedgeNotUsedWhenPrimaryIsFast(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{ok()}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	id, res, _ := complete(t, r, "hedged")
	if id != "a" || res.Hedged || b.calls.Load() != 0 {
		t.Fatalf("id %s hedged %v b calls %d", id, res.Hedged, b.calls.Load())
	}
}

func TestHedgeSingleTargetDuplicatesCall(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{slow(2 * time.Second), ok()}}
	r := setup(t, reliable, a, &fake{name: "b"})
	start := time.Now()
	id, res, err := complete(t, r, "solo")
	if err != nil || id != "a" || !res.Hedged || a.calls.Load() != 2 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("id %s res %+v err %v calls %d", id, res, err, a.calls.Load())
	}
}

func TestPerAttemptTimeoutFallsBack(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{slow(time.Second)}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	r.retry.MaxAttempts = 1
	id, res, err := complete(t, r, "tight")
	if err != nil || id != "b" || res.FallbackDepth != 1 {
		t.Fatalf("id %s res %+v err %v", id, res, err)
	}
}

func TestStreamHedgeClosesLoser(t *testing.T) {
	a := &fake{name: "a", script: []behaviour{slow(150 * time.Millisecond)}}
	b := &fake{name: "b", script: []behaviour{ok()}}
	r := setup(t, reliable, a, b)
	route, _ := r.Resolve("hedged", "", nil)
	s, res, err := r.Stream(context.Background(), route, req)
	if err != nil || res.Target.Provider.Name() != "b" {
		t.Fatalf("res %+v err %v", res, err)
	}
	s.Close()
}
