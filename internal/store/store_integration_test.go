//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/testenv"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), testenv.Postgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := open(t)
	if err := Migrate(context.Background(), s.Pool); err != nil {
		t.Fatal(err)
	}
}

func TestKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.UpsertTenant(ctx, Tenant{ID: "acme", Name: "Acme", CapturePrompts: true}); err != nil {
		t.Fatal(err)
	}
	rpm, budget := 60, 25.5
	g, err := s.CreateKey(ctx, NewKey{TenantID: "acme", Name: "ci", RPM: &rpm, MonthlyBudgetUSD: &budget, AllowedModels: []string{"fast"}})
	if err != nil {
		t.Fatal(err)
	}

	// The plaintext is not stored anywhere in the row.
	var stored []byte
	_ = s.Pool.QueryRow(ctx, "SELECT secret_hash FROM api_keys WHERE id = $1", g.ID).Scan(&stored)
	_, secret, _ := auth.Parse(g.Plaintext)
	if string(stored) == secret || len(stored) != 32 {
		t.Fatalf("stored hash looks wrong: %x", stored)
	}

	a := auth.NewAuthenticator(s, time.Minute)
	k, err := a.Authenticate(ctx, g.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if *k.RPM != 60 || k.TPM != nil || *k.MonthlyBudgetUSD != 25.5 || !k.CapturePrompts || !k.Allows("fast") || k.Allows("smart") {
		t.Fatalf("key = %+v", k)
	}

	if err := s.SetKeyLimits(ctx, g.ID, KeyLimits{TPM: &rpm}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	a.Invalidate(g.ID)
	if _, err := a.Authenticate(ctx, g.Plaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("revoked key authenticated: %v", err)
	}
	if err := s.RevokeKey(ctx, g.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	keys, _ := s.ListKeys(ctx, "acme")
	if len(keys) != 1 || keys[0].RPM != nil || *keys[0].TPM != 60 || keys[0].RevokedAt == nil {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestUsageAndSpend(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_ = s.UpsertTenant(ctx, Tenant{ID: "t", Name: "T"})
	now := time.Now()
	ttft := 120
	recs := []UsageRecord{
		{TS: now.Add(-40 * 24 * time.Hour), RequestID: "old", KeyID: "k1", TenantID: "t", Surface: "openai", Alias: "fast", Status: 200, CostUSD: 5},
		{TS: now, RequestID: "a", KeyID: "k1", TenantID: "t", Surface: "openai", Alias: "fast", Provider: "p", Model: "m", Status: 200, InputTokens: 10, OutputTokens: 5, CostUSD: 0.25, TTFTMS: &ttft},
		{TS: now, RequestID: "b", KeyID: "k1", TenantID: "t", Surface: "anthropic", Alias: "fast", Provider: "p", Model: "m", Status: 502, CostUSD: 0},
		{TS: now, RequestID: "c", KeyID: "k1", TenantID: "t", Surface: "openai", Alias: "fast", Provider: "p", Model: "m", Status: 200, Cache: "exact", CostUSD: 0.5},
	}
	if err := s.InsertUsage(ctx, recs); err != nil {
		t.Fatal(err)
	}
	spend, err := s.SpendSince(ctx, "k1", now.Add(-24*time.Hour))
	if err != nil || spend != 0.75 {
		t.Fatalf("spend = %v %v", spend, err)
	}
	rows, err := s.UsageSince(ctx, now.Add(-time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Requests != 3 || rows[0].Errors != 1 || rows[0].CacheHits != 1 || rows[0].InputTokens != 10 {
		t.Fatalf("rows = %+v %v", rows, err)
	}
}
