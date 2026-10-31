//go:build integration

package cache

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/testenv"
)

func TestPgvectorNearest(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testenv.Postgres(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	p, err := NewPgvector(ctx, pool)
	if err != nil {
		// Containers use the pgvector image; only an external server may lack it.
		if os.Getenv("TEST_POSTGRES_URL") != "" && strings.Contains(err.Error(), "extension") {
			t.Skip("pgvector is not installed on this server:", err)
		}
		t.Fatal(err)
	}
	_ = p.Put(ctx, "s1", []float32{1, 0, 0}, &llm.Response{ID: "x"}, time.Minute)
	_ = p.Put(ctx, "s1", []float32{0, 1, 0}, &llm.Response{ID: "y"}, time.Minute)
	_ = p.Put(ctx, "s2", []float32{1, 0, 0}, &llm.Response{ID: "other"}, time.Minute)
	_ = p.Put(ctx, "s1", []float32{1, 0}, &llm.Response{ID: "wrong-dims"}, time.Minute)
	_ = p.Put(ctx, "s1", []float32{0.99, 0.01, 0}, &llm.Response{ID: "expired"}, -time.Second)

	r, sim, ok, err := p.Nearest(ctx, "s1", []float32{0.9, 0.1, 0})
	if err != nil || !ok || r.ID != "x" || sim < 0.99 {
		t.Fatalf("%+v %v %v %v", r, sim, ok, err)
	}
	if _, _, ok, _ := p.Nearest(ctx, "nope", []float32{1, 0, 0}); ok {
		t.Fatal("empty scope hit")
	}
}
