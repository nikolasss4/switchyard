package auth

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// Key is a stored API key with its limits and tenant settings.
type Key struct {
	ID               string
	TenantID         string
	Name             string
	SecretHash       []byte
	RPM, TPM         *int     // nil = gateway default
	MonthlyBudgetUSD *float64 // nil = no budget
	SoftBudgetRatio  float64
	AllowedModels    []string // nil = any
	CapturePrompts   bool     // tenant opted in to prompt capture
	CreatedAt        time.Time
	RevokedAt        *time.Time
}

// Allows reports whether the key may use model.
func (k *Key) Allows(model string) bool {
	return k.AllowedModels == nil || slices.Contains(k.AllowedModels, model)
}

// ErrNotFound is returned by a Lookup for unknown ids.
var ErrNotFound = errors.New("key not found")

// ErrUnauthorized is returned for missing, malformed, wrong or revoked keys.
var ErrUnauthorized = errors.New("invalid API key")

// Lookup loads a key by id.
type Lookup interface {
	GetKey(ctx context.Context, id string) (*Key, error)
}

// Authenticator verifies keys, caching lookups for a short TTL so the hot path
// does not hit Postgres. A revocation therefore takes up to TTL to apply.
type Authenticator struct {
	lookup Lookup
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	key     *Key // nil = known missing
	expires time.Time
}

// NewAuthenticator returns an authenticator with the given cache TTL.
func NewAuthenticator(l Lookup, ttl time.Duration) *Authenticator {
	return &Authenticator{lookup: l, ttl: ttl, now: time.Now, cache: map[string]cached{}}
}

// Authenticate returns the key for a key string.
func (a *Authenticator) Authenticate(ctx context.Context, keyString string) (*Key, error) {
	id, secret, err := Parse(keyString)
	if err != nil {
		return nil, ErrUnauthorized
	}
	key, err := a.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if key == nil || key.RevokedAt != nil || !Verify(secret, key.SecretHash) {
		return nil, ErrUnauthorized
	}
	return key, nil
}

func (a *Authenticator) get(ctx context.Context, id string) (*Key, error) {
	now := a.now()
	a.mu.Lock()
	c, ok := a.cache[id]
	a.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.key, nil
	}
	key, err := a.lookup.GetKey(ctx, id)
	if errors.Is(err, ErrNotFound) {
		key, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cache[id] = cached{key: key, expires: now.Add(a.ttl)}
	a.mu.Unlock()
	return key, nil
}

// Invalidate drops a cached key, e.g. after a revoke on this instance.
func (a *Authenticator) Invalidate(id string) {
	a.mu.Lock()
	delete(a.cache, id)
	a.mu.Unlock()
}
