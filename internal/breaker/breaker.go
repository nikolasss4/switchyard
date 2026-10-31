// Package breaker is a per-provider circuit breaker.
//
// Closed: outcomes go into a sliding window of the last N calls; once the
// window holds MinRequests calls and the failure rate reaches FailureRate, the
// breaker opens. Open: calls are refused until OpenFor has passed, then it is
// half-open. Half-open: up to Probes calls go through; one failure re-opens it,
// Probes successes close it with a fresh window.
package breaker

import (
	"sync"
	"time"
)

// State is a breaker state.
type State int

// Breaker states.
const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string { return [...]string{"closed", "open", "half_open"}[s] }

// Config tunes a breaker.
type Config struct {
	Window      int
	MinRequests int
	FailureRate float64
	OpenFor     time.Duration
	Probes      int
}

// Breaker is safe for concurrent use.
type Breaker struct {
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	state    State
	window   []bool // true = failure; ring buffer
	pos, n   int
	fails    int
	openedAt time.Time
	inFlight int // half-open probes not yet recorded
	probeOK  int

	// OnChange, if set, is called (outside the lock) on every transition.
	OnChange func(from, to State)
}

// New returns a closed breaker.
func New(cfg Config) *Breaker {
	if cfg.Window <= 0 {
		cfg.Window = 20
	}
	if cfg.Probes <= 0 {
		cfg.Probes = 1
	}
	return &Breaker{cfg: cfg, now: time.Now, window: make([]bool, cfg.Window)}
}

// SetClock replaces the clock, for tests.
func (b *Breaker) SetClock(now func() time.Time) { b.now = now }

// Allow reports whether a call may go through. A caller that is allowed must
// Record the outcome.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	var from, to State
	changed := false
	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.cfg.OpenFor {
			b.mu.Unlock()
			return false
		}
		from, to, changed = Open, HalfOpen, true
		b.state, b.inFlight, b.probeOK = HalfOpen, 0, 0
		fallthrough
	case HalfOpen:
		if b.inFlight >= b.cfg.Probes {
			b.mu.Unlock()
			b.notify(changed, from, to)
			return false
		}
		b.inFlight++
	}
	b.mu.Unlock()
	b.notify(changed, from, to)
	return true
}

// Record reports a call's outcome.
func (b *Breaker) Record(failure bool) {
	b.mu.Lock()
	from := b.state
	switch b.state {
	case Closed:
		if b.n == len(b.window) {
			if b.window[b.pos] {
				b.fails--
			}
		} else {
			b.n++
		}
		b.window[b.pos] = failure
		if failure {
			b.fails++
		}
		b.pos = (b.pos + 1) % len(b.window)
		if b.n >= b.cfg.MinRequests && float64(b.fails)/float64(b.n) >= b.cfg.FailureRate {
			b.trip()
		}
	case HalfOpen:
		if b.inFlight > 0 {
			b.inFlight--
		}
		if failure {
			b.trip()
		} else if b.probeOK++; b.probeOK >= b.cfg.Probes {
			b.state = Closed
			b.reset()
		}
	case Open:
		// A call admitted before the breaker opened; its outcome is moot.
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from != to, from, to)
}

func (b *Breaker) trip() {
	b.state, b.openedAt = Open, b.now()
	b.reset()
}

func (b *Breaker) reset() {
	clear(b.window)
	b.pos, b.n, b.fails, b.inFlight, b.probeOK = 0, 0, 0, 0, 0
}

func (b *Breaker) notify(changed bool, from, to State) {
	if changed && b.OnChange != nil {
		b.OnChange(from, to)
	}
}

// Snapshot is the breaker's current state and window.
type Snapshot struct {
	State       string  `json:"state"`
	Calls       int     `json:"window_calls"`
	FailureRate float64 `json:"window_failure_rate"`
}

// Snapshot returns the current state. An open breaker whose OpenFor has
// passed still reports open until the next Allow.
func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{State: b.state.String(), Calls: b.n}
	if b.n > 0 {
		s.FailureRate = float64(b.fails) / float64(b.n)
	}
	return s
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
