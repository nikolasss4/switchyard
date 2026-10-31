package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nikolasss4/switchyard/internal/llm"
)

// Exact stores whole responses by fingerprint.
type Exact interface {
	Get(ctx context.Context, key string) (*llm.Response, bool, error)
	Set(ctx context.Context, key string, resp *llm.Response, ttl time.Duration) error
}

// RedisExact is an exact cache in Redis.
type RedisExact struct {
	client redis.UniversalClient
	prefix string
}

// NewRedisExact returns a Redis exact cache.
func NewRedisExact(c redis.UniversalClient, prefix string) *RedisExact {
	return &RedisExact{client: c, prefix: prefix}
}

func (c *RedisExact) Get(ctx context.Context, key string) (*llm.Response, bool, error) {
	b, err := c.client.Get(ctx, c.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var r llm.Response
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, false, err
	}
	return &r, true, nil
}

func (c *RedisExact) Set(ctx context.Context, key string, resp *llm.Response, ttl time.Duration) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, c.prefix+key, b, ttl).Err()
}

// MemoryExact is an in-process exact cache.
type MemoryExact struct {
	mu      sync.Mutex
	entries map[string]memEntry
	now     func() time.Time
}

type memEntry struct {
	data    []byte
	expires time.Time
}

// NewMemoryExact returns an in-process exact cache.
func NewMemoryExact() *MemoryExact {
	return &MemoryExact{entries: map[string]memEntry{}, now: time.Now}
}

func (c *MemoryExact) Get(_ context.Context, key string) (*llm.Response, bool, error) {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok || c.now().After(e.expires) {
		return nil, false, nil
	}
	var r llm.Response
	err := json.Unmarshal(e.data, &r)
	return &r, err == nil, err
}

func (c *MemoryExact) Set(_ context.Context, key string, resp *llm.Response, ttl time.Duration) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.entries[key] = memEntry{data: b, expires: c.now().Add(ttl)}
	c.mu.Unlock()
	return nil
}
