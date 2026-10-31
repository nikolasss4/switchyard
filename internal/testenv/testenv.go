//go:build integration

// Package testenv provides Postgres and Redis for integration tests: the
// instances named by TEST_POSTGRES_URL / TEST_REDIS_URL if set, otherwise
// containers started with testcontainers (needs Docker).
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	pgOnce, redisOnce sync.Once
	pgURL, redisURL   string
	pgErr, redisErr   error
)

// PostgresServer returns a server URL (pgvector image when containerised).
func PostgresServer(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		if u := os.Getenv("TEST_POSTGRES_URL"); u != "" {
			pgURL = u
			return
		}
		ctx := context.Background()
		c, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
			tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("postgres"),
			testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)))
		if err != nil {
			pgErr = err
			return
		}
		pgURL, pgErr = c.ConnectionString(ctx, "sslmode=disable")
	})
	if pgErr != nil {
		t.Fatalf("postgres: %v", pgErr)
	}
	return pgURL
}

// Postgres creates a fresh database for the test and returns its URL.
func Postgres(t *testing.T) string {
	t.Helper()
	server := PostgresServer(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), server)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, _ := url.Parse(server)
	u.Path = "/" + name
	return u.String()
}

// Redis returns a Redis URL. Tests share one server; use unique key prefixes.
func Redis(t *testing.T) string {
	t.Helper()
	redisOnce.Do(func() {
		if u := os.Getenv("TEST_REDIS_URL"); u != "" {
			redisURL = u
			return
		}
		ctx := context.Background()
		c, err := tcredis.Run(ctx, "redis:7-alpine")
		if err != nil {
			redisErr = err
			return
		}
		redisURL, redisErr = c.ConnectionString(ctx)
	})
	if redisErr != nil {
		t.Fatalf("redis: %v", redisErr)
	}
	return redisURL
}
