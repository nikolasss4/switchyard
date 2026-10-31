// Package limiter implements per-key token buckets for requests per minute and
// tokens per minute. The Redis implementation runs the check-and-take as one
// Lua script, so every gateway instance shares the same buckets and a request
// either takes from all of its buckets or from none.
package limiter

import (
	"context"
	"time"
)

// Bucket is one bucket to take from. A bucket holds up to PerMinute tokens and
// refills continuously at PerMinute per minute.
type Bucket struct {
	Key       string
	PerMinute int
	Cost      int
}

// Decision is the outcome of a Take.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until every bucket could cover its cost. It is
	// negative when a cost exceeds a bucket's capacity and can never be served.
	RetryAfter time.Duration
	// Denied is the key of the bucket that caused the denial.
	Denied string
}

// Limiter takes from buckets atomically.
type Limiter interface {
	Take(ctx context.Context, buckets []Bucket) (Decision, error)
	// Adjust adds delta tokens to a bucket (negative to debit). It settles a
	// reservation once the real token count is known; a bucket may go into
	// debt down to -PerMinute, which delays later requests.
	Adjust(ctx context.Context, b Bucket, delta int) error
}
