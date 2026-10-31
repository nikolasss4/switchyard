// Command switchyard runs the LLM gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/nikolasss4/switchyard/internal/admin"
	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/budget"
	"github.com/nikolasss4/switchyard/internal/cache"
	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/gateway"
	"github.com/nikolasss4/switchyard/internal/limiter"
	"github.com/nikolasss4/switchyard/internal/pricing"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
	"github.com/nikolasss4/switchyard/internal/server"
	"github.com/nikolasss4/switchyard/internal/store"
	"github.com/nikolasss4/switchyard/internal/telemetry"
	"github.com/nikolasss4/switchyard/internal/usage"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", envOr("SWITCHYARD_CONFIG", "switchyard.yaml"), "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	prices, err := pricing.Load(cfg.Pricing)
	if err != nil {
		return err
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()

	var lim limiter.Limiter
	var spend budget.Counter
	var exact cache.Exact
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			return err
		}
		rdb := redis.NewClient(opt)
		defer rdb.Close()
		if err := rdb.Ping(ctx).Err(); err != nil {
			return err
		}
		lim = limiter.NewRedis(rdb, "sy:rl:")
		spend = budget.NewRedis(rdb, "sy:spend:", db.SpendSince)
		exact = cache.NewRedisExact(rdb, "sy:cache:")
	} else {
		log.Warn("REDIS_URL not set: rate limits, budgets and the exact cache are per instance")
		lim, spend, exact = limiter.NewMemory(), budget.NewMemory(), cache.NewMemoryExact()
	}

	tracer, stopTracing, err := telemetry.SetupTracing(ctx, version)
	if err != nil {
		return err
	}
	defer stopTracing(context.Background()) //nolint:errcheck
	obs := &telemetry.Observer{M: telemetry.NewMetrics(), Tracer: tracer, Capture: log.With("stream", "capture")}

	writer := usage.NewWriter(db.InsertUsage, log, 50_000, 500, time.Second)
	writer.OnDrop = obs.M.UsageDropped.Inc
	defer writer.Close()

	client := provider.NewHTTPClient()
	client.Transport = telemetry.Transport{Base: client.Transport}
	providers := provider.FromConfig(cfg, client)
	caches := gateway.Caches{Exact: exact}
	if semanticEnabled(cfg) {
		if cfg.Embedder == nil {
			return errors.New("a model enables the semantic cache but no embedder is configured")
		}
		vec, err := cache.NewPgvector(ctx, db.Pool)
		if err != nil {
			return err
		}
		caches.Vectors = vec
		caches.Embedder = &router.Target{Provider: providers[cfg.Embedder.Provider], Model: cfg.Embedder.Model}
	}
	rt := router.New(cfg, providers)
	rt.OnAttempt = obs.Attempt
	obs.WatchBreakers(rt.Breakers())
	gw := gateway.New(gateway.Deps{
		Router: rt, Limiter: lim, Spend: spend, Prices: prices,
		Usage: writer, Defaults: cfg.Limits, Log: log, Caches: caches, Observer: obs,
	})
	authn := auth.NewAuthenticator(db, 30*time.Second)
	srv := server.New(gw, authn, log)

	if demo := os.Getenv("SWITCHYARD_DEMO_KEY"); demo != "" {
		if err := db.SeedKey(ctx, "demo", "demo key", demo); err != nil {
			return fmt.Errorf("seeding the demo key: %w", err)
		}
		log.Warn("seeded the demo key from SWITCHYARD_DEMO_KEY; do not do this in production")
	}

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	opsMux := http.NewServeMux()
	opsMux.Handle("GET /metrics", promhttp.HandlerFor(obs.M.Registry, promhttp.HandlerOpts{}))
	opsMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	adminToken := os.Getenv("SWITCHYARD_ADMIN_TOKEN")
	if adminToken == "" {
		log.Warn("SWITCHYARD_ADMIN_TOKEN not set: the admin API refuses every request")
	}
	(&admin.API{Token: adminToken, Store: db, Spend: spend, Breakers: rt.Breakers(), Auth: authn, Log: log}).Register(opsMux)
	opsSrv := &http.Server{Addr: cfg.MetricsListen, Handler: opsMux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "metrics", cfg.MetricsListen, "price_version", prices.Version, "version", version)
		errc <- httpSrv.ListenAndServe()
	}()
	go func() { errc <- opsSrv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = opsSrv.Shutdown(shutdown)
	return httpSrv.Shutdown(shutdown)
}

func semanticEnabled(cfg *config.Config) bool {
	for _, m := range cfg.Models {
		if m.Cache.Semantic.Enabled {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
