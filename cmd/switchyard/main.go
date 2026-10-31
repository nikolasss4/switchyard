// Command switchyard runs the LLM gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/budget"
	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/gateway"
	"github.com/nikolasss4/switchyard/internal/limiter"
	"github.com/nikolasss4/switchyard/internal/pricing"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
	"github.com/nikolasss4/switchyard/internal/server"
	"github.com/nikolasss4/switchyard/internal/store"
	"github.com/nikolasss4/switchyard/internal/usage"
)

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
	} else {
		log.Warn("REDIS_URL not set: rate limits and budgets are per instance")
		lim, spend = limiter.NewMemory(), budget.NewMemory()
	}

	writer := usage.NewWriter(db.InsertUsage, log, 50_000, 500, time.Second)
	defer writer.Close()

	providers := provider.FromConfig(cfg, provider.NewHTTPClient())
	gw := gateway.New(gateway.Deps{
		Router: router.New(cfg, providers), Limiter: lim, Spend: spend, Prices: prices,
		Usage: writer, Defaults: cfg.Limits, Log: log,
	})
	srv := server.New(gw, auth.NewAuthenticator(db, 30*time.Second), log)

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "price_version", prices.Version)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdown)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
