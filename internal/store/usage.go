package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// UsageRecord is one request's accounting row.
type UsageRecord struct {
	TS               time.Time
	RequestID        string
	KeyID, TenantID  string
	Surface, Alias   string
	Provider, Model  string
	Stream           bool
	Status           int
	Cache            string
	Attempts         int
	FallbackDepth    int
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	PriceVersion     string
	LatencyMS        int
	TTFTMS           *int
}

var usageColumns = []string{"ts", "request_id", "key_id", "tenant_id", "surface", "alias", "provider", "model", "stream", "status",
	"cache", "attempts", "fallback_depth", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
	"cost_usd", "price_version", "latency_ms", "ttft_ms"}

// InsertUsage writes records with COPY.
func (s *Store) InsertUsage(ctx context.Context, recs []UsageRecord) error {
	_, err := s.Pool.CopyFrom(ctx, pgx.Identifier{"usage_records"}, usageColumns,
		pgx.CopyFromSlice(len(recs), func(i int) ([]any, error) {
			r := recs[i]
			return []any{r.TS, r.RequestID, r.KeyID, r.TenantID, r.Surface, r.Alias, r.Provider, r.Model, r.Stream, r.Status,
				r.Cache, r.Attempts, r.FallbackDepth, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
				r.CostUSD, r.PriceVersion, r.LatencyMS, r.TTFTMS}, nil
		}))
	return err
}

// SpendSince returns a key's total cost since t.
func (s *Store) SpendSince(ctx context.Context, keyID string, since time.Time) (float64, error) {
	var usd float64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0)::float8 FROM usage_records WHERE key_id = $1 AND ts >= $2`, keyID, since).Scan(&usd)
	return usd, err
}

// UsageRow is an aggregate for the admin API.
type UsageRow struct {
	KeyID        string  `json:"key_id"`
	TenantID     string  `json:"tenant_id"`
	Alias        string  `json:"alias"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	CacheHits    int64   `json:"cache_hits"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// UsageSince aggregates usage per key, alias, provider and model since t.
func (s *Store) UsageSince(ctx context.Context, since time.Time) ([]UsageRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT key_id, tenant_id, alias, provider, model, count(*),
		count(*) FILTER (WHERE status >= 400), count(*) FILTER (WHERE cache <> ''),
		COALESCE(SUM(input_tokens + cache_read_tokens + cache_write_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cost_usd), 0)::float8
		FROM usage_records WHERE ts >= $1 GROUP BY 1, 2, 3, 4, 5 ORDER BY 11 DESC, 6 DESC`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UsageRow, error) {
		var u UsageRow
		err := r.Scan(&u.KeyID, &u.TenantID, &u.Alias, &u.Provider, &u.Model, &u.Requests, &u.Errors, &u.CacheHits,
			&u.InputTokens, &u.OutputTokens, &u.CostUSD)
		return u, err
	})
}
