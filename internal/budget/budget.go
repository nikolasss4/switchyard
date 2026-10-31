// Package budget enforces monthly USD budgets per API key.
//
// Spend is kept in a shared counter (Redis in production) per key and calendar
// month (UTC), in micro-dollars. On a miss the counter is seeded from the
// usage records in Postgres. The check happens before a request and the spend is
// recorded after it, so concurrent in-flight requests can overshoot a hard
// limit by their own cost; the next request is then refused.
package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// State is where a key stands against its budget.
type State int

const (
	// OK is under the soft limit (or no budget).
	OK State = iota
	// Soft is at or over the soft limit: requests proceed with a warning.
	Soft
	// Hard is at or over the budget: requests are refused.
	Hard
)

func (s State) String() string { return [...]string{"ok", "soft", "hard"}[s] }

// Evaluate places spend against a monthly limit and soft ratio. A nil limit
// means no budget.
func Evaluate(spentUSD float64, limitUSD *float64, softRatio float64) State {
	if limitUSD == nil {
		return OK
	}
	switch {
	case spentUSD >= *limitUSD:
		return Hard
	case spentUSD >= *limitUSD*softRatio:
		return Soft
	}
	return OK
}

// Month returns the UTC calendar month containing t, as its start and a key suffix.
func Month(t time.Time) (time.Time, string) {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.Format("2006-01")
}

// Seeder loads a key's spend since a time from durable storage.
type Seeder func(ctx context.Context, keyID string, since time.Time) (float64, error)

// Counter is a monthly spend counter.
type Counter interface {
	Spent(ctx context.Context, keyID string, now time.Time) (float64, error)
	Add(ctx context.Context, keyID string, now time.Time, usd float64) error
}

func micros(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// Redis is a Counter shared by all gateway instances.
type Redis struct {
	client redis.UniversalClient
	prefix string
	seed   Seeder
}

// NewRedis returns a Redis spend counter.
func NewRedis(client redis.UniversalClient, prefix string, seed Seeder) *Redis {
	return &Redis{client: client, prefix: prefix, seed: seed}
}

func (r *Redis) key(keyID, month string) string { return r.prefix + keyID + ":" + month }

func (r *Redis) Spent(ctx context.Context, keyID string, now time.Time) (float64, error) {
	start, month := Month(now)
	k := r.key(keyID, month)
	v, err := r.client.Get(ctx, k).Result()
	if errors.Is(err, redis.Nil) {
		if err := r.seedKey(ctx, k, keyID, start); err != nil {
			return 0, err
		}
		v, err = r.client.Get(ctx, k).Result()
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("spend counter %s: %w", k, err)
	}
	return float64(n) / 1e6, nil
}

func (r *Redis) seedKey(ctx context.Context, k, keyID string, start time.Time) error {
	var usd float64
	if r.seed != nil {
		var err error
		if usd, err = r.seed(ctx, keyID, start); err != nil {
			return err
		}
	}
	// SETNX: another instance may have seeded (and added to) it meanwhile.
	return r.client.SetNX(ctx, k, micros(usd), 40*24*time.Hour).Err()
}

func (r *Redis) Add(ctx context.Context, keyID string, now time.Time, usd float64) error {
	start, month := Month(now)
	k := r.key(keyID, month)
	n, err := r.client.Exists(ctx, k).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		// Seed before adding, or a later seed would be skipped and the
		// month's earlier spend lost.
		if err := r.seedKey(ctx, k, keyID, start); err != nil {
			return err
		}
	}
	return r.client.IncrBy(ctx, k, micros(usd)).Err()
}

// Memory is an in-process Counter.
type Memory struct {
	mu    sync.Mutex
	spent map[string]int64
}

// NewMemory returns an in-process spend counter.
func NewMemory() *Memory { return &Memory{spent: map[string]int64{}} }

func (m *Memory) Spent(_ context.Context, keyID string, now time.Time) (float64, error) {
	_, month := Month(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	return float64(m.spent[keyID+":"+month]) / 1e6, nil
}

func (m *Memory) Add(_ context.Context, keyID string, now time.Time, usd float64) error {
	_, month := Month(now)
	m.mu.Lock()
	m.spent[keyID+":"+month] += micros(usd)
	m.mu.Unlock()
	return nil
}
