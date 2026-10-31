package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeLookup struct {
	keys  map[string]*Key
	calls int
}

func (f *fakeLookup) GetKey(_ context.Context, id string) (*Key, error) {
	f.calls++
	if k, ok := f.keys[id]; ok {
		return k, nil
	}
	return nil, ErrNotFound
}

func TestAuthenticatorCachesAndRejects(t *testing.T) {
	g, _ := Generate()
	l := &fakeLookup{keys: map[string]*Key{g.ID: {ID: g.ID, SecretHash: g.Hash}}}
	a := NewAuthenticator(l, time.Minute)
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }

	for range 3 {
		if _, err := a.Authenticate(context.Background(), g.Plaintext); err != nil {
			t.Fatal(err)
		}
	}
	if l.calls != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", l.calls)
	}
	if _, err := a.Authenticate(context.Background(), g.Plaintext[:len(g.Plaintext)-1]+"Z"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong secret: %v", err)
	}
	other, _ := Generate()
	if _, err := a.Authenticate(context.Background(), other.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown key: %v", err)
	}

	revoked := now
	l.keys[g.ID].RevokedAt = &revoked
	now = now.Add(2 * time.Minute) // past the TTL
	if _, err := a.Authenticate(context.Background(), g.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked key: %v", err)
	}
}

func TestAuthenticatorCacheIsBounded(t *testing.T) {
	a := NewAuthenticator(&fakeLookup{keys: map[string]*Key{}}, time.Minute)
	for i := range maxCached + 10 {
		_, _ = a.Authenticate(context.Background(), "sy_"+fmt.Sprintf("%08d", i)+"_"+strings.Repeat("a", 32))
	}
	if n := len(a.cache); n > maxCached {
		t.Fatalf("cache grew to %d entries", n)
	}
}
