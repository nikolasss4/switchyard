package limiter

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// takeScript checks every bucket, then takes from all of them or none. It uses
// the Redis server clock so instances with skewed clocks agree.
//
// KEYS: bucket keys. ARGV: per bucket, capacity then cost.
// Returns {1, 0} when allowed, {0, wait_ms} when denied, {0, -1, i} when a
// cost can never fit; the third element is the 1-based denying bucket.
var takeScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local n = #KEYS
local levels = {}
local wait, denied = 0, 0
for i = 1, n do
  local cap = tonumber(ARGV[i * 2 - 1])
  local cost = tonumber(ARGV[i * 2])
  if cost > cap then return {0, -1, i} end
  local rate = cap / 60000
  local v = redis.call('HMGET', KEYS[i], 'tokens', 'ts')
  local tokens, ts = tonumber(v[1]), tonumber(v[2])
  if tokens == nil then tokens, ts = cap, now end
  tokens = math.min(cap, tokens + (now - ts) * rate)
  levels[i] = tokens
  if tokens < cost then
    local need = math.ceil((cost - tokens) / rate)
    if need > wait then wait, denied = need, i end
  end
end
if wait > 0 then return {0, wait, denied} end
for i = 1, n do
  local cap = tonumber(ARGV[i * 2 - 1])
  redis.call('HSET', KEYS[i], 'tokens', tostring(levels[i] - tonumber(ARGV[i * 2])), 'ts', now)
  redis.call('PEXPIRE', KEYS[i], 120000)
end
return {1, 0, 0}
`)

// adjustScript refills a bucket, then adds a delta clamped to [-cap, cap].
// KEYS[1]: bucket. ARGV: capacity, delta.
var adjustScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local cap, delta = tonumber(ARGV[1]), tonumber(ARGV[2])
local v = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens, ts = tonumber(v[1]), tonumber(v[2])
if tokens == nil then tokens, ts = cap, now end
tokens = math.min(cap, tokens + (now - ts) * cap / 60000)
tokens = math.max(-cap, math.min(cap, tokens + delta))
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
redis.call('PEXPIRE', KEYS[1], 120000)
return 1
`)

// Redis is a limiter shared by all gateway instances.
type Redis struct {
	client redis.UniversalClient
	prefix string
}

// NewRedis returns a Redis limiter; keys are namespaced with prefix.
func NewRedis(client redis.UniversalClient, prefix string) *Redis {
	return &Redis{client: client, prefix: prefix}
}

func (r *Redis) Take(ctx context.Context, buckets []Bucket) (Decision, error) {
	keys := make([]string, len(buckets))
	args := make([]any, 0, 2*len(buckets))
	for i, b := range buckets {
		// The hash tag keeps one key's buckets in the same Redis Cluster slot.
		keys[i] = r.prefix + "{" + b.Key + "}"
		args = append(args, strconv.Itoa(b.PerMinute), strconv.Itoa(b.Cost))
	}
	res, err := takeScript.Run(ctx, r.client, keys, args...).Int64Slice()
	if err != nil {
		return Decision{}, err
	}
	if res[0] == 1 {
		return Decision{Allowed: true}, nil
	}
	d := Decision{RetryAfter: time.Duration(res[1]) * time.Millisecond}
	if res[1] < 0 {
		d.RetryAfter = -1
	}
	if i := int(res[2]); i >= 1 && i <= len(buckets) {
		d.Denied = buckets[i-1].Key
	}
	return d, nil
}

func (r *Redis) Adjust(ctx context.Context, b Bucket, delta int) error {
	return adjustScript.Run(ctx, r.client, []string{r.prefix + "{" + b.Key + "}"}, strconv.Itoa(b.PerMinute), strconv.Itoa(delta)).Err()
}
