package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikolasss4/switchyard/internal/auth"
)

// Store wraps a Postgres pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects and runs migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }

// Tenant is a customer that owns keys.
type Tenant struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	CapturePrompts bool      `json:"capture_prompts"`
	CreatedAt      time.Time `json:"created_at"`
}

// UpsertTenant creates a tenant or updates its name and capture setting.
func (s *Store) UpsertTenant(ctx context.Context, t Tenant) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO tenants (id, name, capture_prompts) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, capture_prompts = EXCLUDED.capture_prompts`,
		t.ID, t.Name, t.CapturePrompts)
	return err
}

// ListTenants returns all tenants.
func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, name, capture_prompts, created_at FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		err := r.Scan(&t.ID, &t.Name, &t.CapturePrompts, &t.CreatedAt)
		return t, err
	})
}

// NewKey is the input to CreateKey.
type NewKey struct {
	TenantID         string
	Name             string
	RPM, TPM         *int
	MonthlyBudgetUSD *float64
	SoftBudgetRatio  float64
	AllowedModels    []string
}

// CreateKey issues a key. The plaintext is returned once and never stored.
func (s *Store) CreateKey(ctx context.Context, in NewKey) (auth.Generated, error) {
	g, err := auth.Generate()
	if err != nil {
		return auth.Generated{}, err
	}
	soft := in.SoftBudgetRatio
	if soft == 0 {
		soft = 0.8
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO api_keys
		(id, tenant_id, name, secret_hash, rpm, tpm, monthly_budget_usd, soft_budget_ratio, allowed_models)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		g.ID, in.TenantID, in.Name, g.Hash, in.RPM, in.TPM, in.MonthlyBudgetUSD, soft, in.AllowedModels)
	return g, err
}

const keyColumns = `k.id, k.tenant_id, k.name, k.secret_hash, k.rpm, k.tpm, k.monthly_budget_usd::float8,
	k.soft_budget_ratio::float8, k.allowed_models, t.capture_prompts, k.created_at, k.revoked_at`

func scanKey(r pgx.Row) (*auth.Key, error) {
	var k auth.Key
	err := r.Scan(&k.ID, &k.TenantID, &k.Name, &k.SecretHash, &k.RPM, &k.TPM, &k.MonthlyBudgetUSD,
		&k.SoftBudgetRatio, &k.AllowedModels, &k.CapturePrompts, &k.CreatedAt, &k.RevokedAt)
	return &k, err
}

// GetKey loads a key by id (implements auth.Lookup).
func (s *Store) GetKey(ctx context.Context, id string) (*auth.Key, error) {
	k, err := scanKey(s.Pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys k JOIN tenants t ON t.id = k.tenant_id WHERE k.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrNotFound
	}
	return k, err
}

// ListKeys returns keys, newest first; tenantID "" lists all tenants.
func (s *Store) ListKeys(ctx context.Context, tenantID string) ([]*auth.Key, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+keyColumns+` FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE $1 = '' OR k.tenant_id = $1 ORDER BY k.created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*auth.Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks a key revoked.
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return auth.ErrNotFound
	}
	return err
}

// KeyLimits are the mutable limits of a key; nil fields are cleared.
type KeyLimits struct {
	RPM, TPM         *int
	MonthlyBudgetUSD *float64
	SoftBudgetRatio  float64
}

// SetKeyLimits replaces a key's limits and budget.
func (s *Store) SetKeyLimits(ctx context.Context, id string, l KeyLimits) error {
	if l.SoftBudgetRatio == 0 {
		l.SoftBudgetRatio = 0.8
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE api_keys SET rpm = $2, tpm = $3, monthly_budget_usd = $4, soft_budget_ratio = $5 WHERE id = $1`,
		id, l.RPM, l.TPM, l.MonthlyBudgetUSD, l.SoftBudgetRatio)
	if err == nil && tag.RowsAffected() == 0 {
		return auth.ErrNotFound
	}
	return err
}
