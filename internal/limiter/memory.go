package limiter

import (
	"context"
	"math"
	"sync"
	"time"
)

// Memory is an in-process limiter for single-instance deployments and tests.
type Memory struct {
	mu      sync.Mutex
	buckets map[string]*memBucket
	now     func() time.Time
}

type memBucket struct {
	tokens float64
	last   time.Time
}

// NewMemory returns an in-process limiter.
func NewMemory() *Memory {
	return &Memory{buckets: map[string]*memBucket{}, now: time.Now}
}

func (m *Memory) refill(key string, perMinute int, now time.Time) *memBucket {
	b, ok := m.buckets[key]
	capacity := float64(perMinute)
	if !ok {
		b = &memBucket{tokens: capacity, last: now}
		m.buckets[key] = b
		return b
	}
	rate := capacity / float64(time.Minute.Milliseconds())
	elapsed := float64(now.Sub(b.last).Milliseconds())
	b.tokens = math.Min(capacity, b.tokens+elapsed*rate)
	b.last = now
	return b
}

func (m *Memory) Take(_ context.Context, buckets []Bucket) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var wait float64
	denied := ""
	for _, bk := range buckets {
		if bk.Cost > bk.PerMinute {
			return Decision{RetryAfter: -1, Denied: bk.Key}, nil
		}
		b := m.refill(bk.Key, bk.PerMinute, now)
		if b.tokens < float64(bk.Cost) {
			rate := float64(bk.PerMinute) / float64(time.Minute.Milliseconds())
			if need := (float64(bk.Cost) - b.tokens) / rate; need > wait {
				wait, denied = need, bk.Key
			}
		}
	}
	if wait > 0 {
		return Decision{RetryAfter: time.Duration(math.Ceil(wait)) * time.Millisecond, Denied: denied}, nil
	}
	for _, bk := range buckets {
		m.buckets[bk.Key].tokens -= float64(bk.Cost)
	}
	return Decision{Allowed: true}, nil
}

func (m *Memory) Adjust(_ context.Context, bk Bucket, delta int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.refill(bk.Key, bk.PerMinute, m.now())
	b.tokens = math.Max(-float64(bk.PerMinute), math.Min(float64(bk.PerMinute), b.tokens+float64(delta)))
	return nil
}
