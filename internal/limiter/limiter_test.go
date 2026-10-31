package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMemoryRefillsContinuously(t *testing.T) {
	m := NewMemory()
	now := time.Unix(0, 0)
	m.now = func() time.Time { return now }
	ctx := context.Background()
	rpm := []Bucket{{Key: "k:rpm", PerMinute: 60, Cost: 1}}

	for i := range 60 {
		if d, _ := m.Take(ctx, rpm); !d.Allowed {
			t.Fatalf("request %d denied while the bucket was full", i)
		}
	}
	d, _ := m.Take(ctx, rpm)
	if d.Allowed || d.RetryAfter != time.Second || d.Denied != "k:rpm" {
		t.Fatalf("61st: %+v", d)
	}
	now = now.Add(time.Second) // one token per second at 60 rpm
	if d, _ := m.Take(ctx, rpm); !d.Allowed {
		t.Fatalf("after 1s: %+v", d)
	}
}

func TestMemoryAllOrNothing(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	both := []Bucket{{Key: "rpm", PerMinute: 100, Cost: 1}, {Key: "tpm", PerMinute: 1000, Cost: 800}}
	if d, _ := m.Take(ctx, both); !d.Allowed {
		t.Fatal(d)
	}
	d, _ := m.Take(ctx, both)
	if d.Allowed || d.Denied != "tpm" {
		t.Fatalf("second take: %+v", d)
	}
	// The rpm bucket must not have been charged by the denied take.
	if got := m.buckets["rpm"].tokens; got < 98.9 || got > 99.1 {
		t.Fatalf("rpm tokens = %v, want 99", got)
	}
}

func TestMemoryOversizedCost(t *testing.T) {
	d, _ := NewMemory().Take(context.Background(), []Bucket{{Key: "tpm", PerMinute: 100, Cost: 101}})
	if d.Allowed || d.RetryAfter >= 0 {
		t.Fatalf("%+v", d)
	}
}

func TestMemoryAdjustDebt(t *testing.T) {
	m := NewMemory()
	now := time.Unix(0, 0)
	m.now = func() time.Time { return now }
	ctx := context.Background()
	b := Bucket{Key: "tpm", PerMinute: 600, Cost: 100}
	_, _ = m.Take(ctx, []Bucket{b}) // reserve 100
	_ = m.Adjust(ctx, b, -900)      // actual use was 1000: settle
	if got := m.buckets["tpm"].tokens; got != -400 {
		t.Fatalf("tokens = %v, want -400", got)
	}
	_ = m.Adjust(ctx, b, -10000)
	if got := m.buckets["tpm"].tokens; got != -600 {
		t.Fatalf("debt must clamp at -capacity, got %v", got)
	}
}

func miniRedis(t *testing.T) *Redis {
	t.Helper()
	mr := miniredis.RunT(t)
	return NewRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "test:")
}

func TestRedisScriptSemantics(t *testing.T) {
	r := miniRedis(t)
	ctx := context.Background()
	both := []Bucket{{Key: "k:rpm", PerMinute: 10, Cost: 1}, {Key: "k:tpm", PerMinute: 100, Cost: 60}}
	if d, err := r.Take(ctx, both); err != nil || !d.Allowed {
		t.Fatalf("first: %+v %v", d, err)
	}
	d, err := r.Take(ctx, both)
	if err != nil || d.Allowed || d.Denied != "k:tpm" || d.RetryAfter <= 0 {
		t.Fatalf("second: %+v %v", d, err)
	}
	if d, _ := r.Take(ctx, []Bucket{{Key: "k:tpm", PerMinute: 100, Cost: 101}}); d.Allowed || d.RetryAfter >= 0 {
		t.Fatalf("oversized: %+v", d)
	}
	if err := r.Adjust(ctx, both[1], 50); err != nil {
		t.Fatal(err)
	}
	if d, _ := r.Take(ctx, both); !d.Allowed {
		t.Fatalf("after refund: %+v", d)
	}
}

func TestRedisConcurrentTakesNeverOverspend(t *testing.T) {
	r := miniRedis(t)
	ctx := context.Background()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := r.Take(ctx, []Bucket{{Key: "shared", PerMinute: 50, Cost: 1}}); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// 200 concurrent takes on a 50/min bucket: 50 plus at most a token or two of refill.
	if n := allowed.Load(); n < 50 || n > 52 {
		t.Fatalf("allowed %d of 200", n)
	}
}
