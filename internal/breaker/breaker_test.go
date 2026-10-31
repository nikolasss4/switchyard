package breaker

import (
	"sync"
	"testing"
	"time"
)

func newTest() (*Breaker, *time.Time, *[]string) {
	now := time.Unix(0, 0)
	b := New(Config{Window: 10, MinRequests: 4, FailureRate: 0.5, OpenFor: 5 * time.Second, Probes: 2})
	b.SetClock(func() time.Time { return now })
	var log []string
	b.OnChange = func(from, to State) { log = append(log, from.String()+">"+to.String()) }
	return b, &now, &log
}

func call(b *Breaker, fail bool) bool {
	if !b.Allow() {
		return false
	}
	b.Record(fail)
	return true
}

func TestStaysClosedBelowMinRequests(t *testing.T) {
	b, _, _ := newTest()
	for range 3 {
		call(b, true)
	}
	if b.State() != Closed {
		t.Fatal("3 failures < min_requests must not trip")
	}
	call(b, true)
	if b.State() != Open {
		t.Fatal("4 of 4 failing must trip")
	}
}

func TestTripsAtFailureRate(t *testing.T) {
	b, _, _ := newTest()
	for _, f := range []bool{false, false, false, true, false, true, false} { // 2/7
		call(b, f)
	}
	if b.State() != Closed {
		t.Fatal("2/7 is under 50%")
	}
	call(b, true)
	call(b, true) // 4/9 still under
	if b.State() != Closed {
		t.Fatal("4/9 is under 50%")
	}
	call(b, true) // 5/10
	if b.State() != Open {
		t.Fatal("5/10 must trip")
	}
}

func TestWindowSlides(t *testing.T) {
	b, _, _ := newTest()
	for range 10 {
		call(b, false)
	}
	// Old successes fall out of the 10-call window as failures come in.
	for i := range 5 {
		call(b, true)
		if i < 4 && b.State() != Closed {
			t.Fatalf("tripped early after %d failures", i+1)
		}
	}
	if b.State() != Open {
		t.Fatal("5 failures in the last 10 must trip")
	}
}

func TestOpenHalfOpenClosed(t *testing.T) {
	b, now, log := newTest()
	for range 4 {
		call(b, true)
	}
	if b.Allow() {
		t.Fatal("open breaker allowed a call")
	}
	*now = now.Add(5 * time.Second)
	// Half-open: exactly two concurrent probes.
	if !b.Allow() || !b.Allow() || b.Allow() {
		t.Fatal("half-open must admit exactly Probes calls")
	}
	b.Record(false)
	if b.State() != HalfOpen {
		t.Fatal("one probe success of two must stay half-open")
	}
	b.Record(false)
	if b.State() != Closed || b.Snapshot().Calls != 0 {
		t.Fatalf("two probe successes must close with a fresh window: %+v", b.Snapshot())
	}
	want := []string{"closed>open", "open>half_open", "half_open>closed"}
	if len(*log) != 3 || (*log)[0] != want[0] || (*log)[1] != want[1] || (*log)[2] != want[2] {
		t.Fatalf("transitions = %v", *log)
	}
}

func TestProbeFailureReopens(t *testing.T) {
	b, now, _ := newTest()
	for range 4 {
		call(b, true)
	}
	*now = now.Add(5 * time.Second)
	call(b, true)
	if b.State() != Open || b.Allow() {
		t.Fatal("a failed probe must re-open")
	}
	*now = now.Add(4 * time.Second)
	if b.Allow() {
		t.Fatal("re-opened breaker waits a full OpenFor again")
	}
	*now = now.Add(time.Second)
	if !b.Allow() {
		t.Fatal("should be half-open again")
	}
}

func TestConcurrentUse(t *testing.T) {
	b := New(Config{Window: 50, MinRequests: 10, FailureRate: 0.5, OpenFor: time.Millisecond, Probes: 3})
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 500 {
				call(b, (i+j)%3 == 0)
				_ = b.Snapshot()
			}
		}()
	}
	wg.Wait()
}
