//go:build integration

package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/nikolasss4/switchyard/internal/testenv"
)

// Two gateway instances (two clients, two Limiter values) share one bucket.
func TestRedisSharedAcrossInstances(t *testing.T) {
	opt, err := redis.ParseURL(testenv.Redis(t))
	if err != nil {
		t.Fatal(err)
	}
	a := NewRedis(redis.NewClient(opt), "it:"+t.Name()+":")
	b := NewRedis(redis.NewClient(opt), "it:"+t.Name()+":")
	ctx := context.Background()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Add(1)
		go func(l *Redis) {
			defer wg.Done()
			d, err := l.Take(ctx, []Bucket{{Key: "key1:rpm", PerMinute: 100, Cost: 1}, {Key: "key1:tpm", PerMinute: 100000, Cost: 10}})
			if err != nil {
				t.Error(err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}([]*Redis{a, b}[i%2])
	}
	wg.Wait()
	if n := allowed.Load(); n < 100 || n > 103 {
		t.Fatalf("allowed %d of 300 across two instances, want ~100", n)
	}
}
