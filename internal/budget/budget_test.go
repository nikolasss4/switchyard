package budget

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func ptr(f float64) *float64 { return &f }

func TestEvaluate(t *testing.T) {
	cases := []struct {
		spent float64
		limit *float64
		soft  float64
		want  State
	}{
		{1e9, nil, 0.8, OK},
		{0, ptr(10), 0.8, OK},
		{7.99, ptr(10), 0.8, OK},
		{8, ptr(10), 0.8, Soft},
		{9.99, ptr(10), 0.8, Soft},
		{10, ptr(10), 0.8, Hard},
		{0, ptr(0), 0.8, Hard}, // a zero budget blocks everything
		{5, ptr(10), 1, OK},    // soft ratio 1 disables the warning
	}
	for _, c := range cases {
		if got := Evaluate(c.spent, c.limit, c.soft); got != c.want {
			t.Errorf("Evaluate(%v, %v, %v) = %v, want %v", c.spent, c.limit, c.soft, got, c.want)
		}
	}
}

func TestMonthBoundaryIsUTC(t *testing.T) {
	kyiv := time.FixedZone("UTC+2", 2*3600)
	start, m := Month(time.Date(2026, 11, 1, 1, 0, 0, 0, kyiv)) // 23:00 UTC on Oct 31
	if m != "2026-10" || !start.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %s", start, m)
	}
}

func TestRedisCounterSeedsOnceAndAccumulates(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	seeds := 0
	c := NewRedis(client, "spend:", func(_ context.Context, keyID string, since time.Time) (float64, error) {
		seeds++
		if keyID != "k" || since.Day() != 1 {
			t.Errorf("seed(%s, %v)", keyID, since)
		}
		return 2.5, nil
	})
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if err := c.Add(ctx, "k", now, 0.000001); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = c.Add(ctx, "k", now, 0.1)
	}
	got, err := c.Spent(ctx, "k", now)
	if err != nil || got != 2.800001 || seeds != 1 {
		t.Fatalf("spent %v err %v seeds %d", got, err, seeds)
	}
	// A new month starts from its own seed.
	next, _ := c.Spent(ctx, "k", now.Add(24*time.Hour))
	if next != 2.5 || seeds != 2 {
		t.Fatalf("november %v seeds %d", next, seeds)
	}
}

func TestMemoryCounter(t *testing.T) {
	m := NewMemory()
	now := time.Now()
	_ = m.Add(context.Background(), "k", now, 1.25)
	_ = m.Add(context.Background(), "k", now, 0.75)
	if got, _ := m.Spent(context.Background(), "k", now); got != 2 {
		t.Fatal(got)
	}
}
