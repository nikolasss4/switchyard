package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikolasss4/switchyard/internal/llm"
)

// Vectors stores embeddings with their responses. The prompt text itself is
// not stored.
type Vectors interface {
	// Nearest returns the closest entry in scope and its cosine similarity.
	Nearest(ctx context.Context, scope string, v []float32) (*llm.Response, float64, bool, error)
	Put(ctx context.Context, scope string, v []float32, resp *llm.Response, ttl time.Duration) error
}

// Pgvector keeps entries in a pgvector table. Lookups filter by scope and then
// rank by cosine distance; scopes are narrow (one tenant, alias and context),
// so this is a small exact scan rather than an ANN index.
type Pgvector struct {
	pool *pgxpool.Pool
}

// NewPgvector creates the extension and table if needed.
func NewPgvector(ctx context.Context, pool *pgxpool.Pool) (*Pgvector, error) {
	_, err := pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS semantic_cache (
			id         bigserial PRIMARY KEY,
			scope      text NOT NULL,
			embedding  vector NOT NULL,
			response   jsonb NOT NULL,
			expires_at timestamptz NOT NULL
		);
		CREATE INDEX IF NOT EXISTS semantic_cache_scope ON semantic_cache (scope, expires_at);`)
	if err != nil {
		return nil, fmt.Errorf("semantic cache needs the pgvector extension: %w", err)
	}
	return &Pgvector{pool: pool}, nil
}

func vecLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func (p *Pgvector) Nearest(ctx context.Context, scope string, v []float32) (*llm.Response, float64, bool, error) {
	var raw []byte
	var dist float64
	err := p.pool.QueryRow(ctx, `SELECT response, embedding <=> $2::vector FROM semantic_cache
		WHERE scope = $1 AND expires_at > now() AND vector_dims(embedding) = $3
		ORDER BY embedding <=> $2::vector LIMIT 1`, scope, vecLiteral(v), len(v)).Scan(&raw, &dist)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	var r llm.Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, 0, false, err
	}
	return &r, 1 - dist, true, nil
}

func (p *Pgvector) Put(ctx context.Context, scope string, v []float32, resp *llm.Response, ttl time.Duration) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO semantic_cache (scope, embedding, response, expires_at)
		VALUES ($1, $2::vector, $3, now() + $4::interval)`, scope, vecLiteral(v), b, fmt.Sprintf("%d milliseconds", ttl.Milliseconds()))
	return err
}

// MemoryVectors is an in-process Vectors for tests and single-instance runs.
type MemoryVectors struct {
	mu      sync.Mutex
	entries map[string][]vecEntry
	now     func() time.Time
}

type vecEntry struct {
	v       []float32
	resp    []byte
	expires time.Time
}

// NewMemoryVectors returns an in-process vector store.
func NewMemoryVectors() *MemoryVectors {
	return &MemoryVectors{entries: map[string][]vecEntry{}, now: time.Now}
}

func (m *MemoryVectors) Nearest(_ context.Context, scope string, v []float32) (*llm.Response, float64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	best, bestSim := -1, -2.0
	now := m.now()
	for i, e := range m.entries[scope] {
		if now.After(e.expires) || len(e.v) != len(v) {
			continue
		}
		if s := Cosine(e.v, v); s > bestSim {
			best, bestSim = i, s
		}
	}
	if best < 0 {
		return nil, 0, false, nil
	}
	var r llm.Response
	err := json.Unmarshal(m.entries[scope][best].resp, &r)
	return &r, bestSim, err == nil, err
}

func (m *MemoryVectors) Put(_ context.Context, scope string, v []float32, resp *llm.Response, ttl time.Duration) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.entries[scope] = append(m.entries[scope], vecEntry{v: v, resp: b, expires: m.now().Add(ttl)})
	m.mu.Unlock()
	return nil
}

// Cosine is the cosine similarity of two vectors.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
