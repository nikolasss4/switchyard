// Command mockllm runs the mock LLM provider used by the tests, benchmarks and
// the docker compose demo.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/nikolasss4/switchyard/internal/mock"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	name := flag.String("name", "mock", "model id reported by /v1/models")
	cfg := mock.DefaultConfig()
	flag.IntVar(&cfg.LatencyMS, "latency-ms", cfg.LatencyMS, "delay before the first byte")
	flag.IntVar(&cfg.JitterMS, "jitter-ms", cfg.JitterMS, "uniform extra delay")
	flag.Float64Var(&cfg.ErrorRate, "error-rate", cfg.ErrorRate, "fraction of requests that fail")
	flag.IntVar(&cfg.RPM, "rpm", cfg.RPM, "requests per minute before 429; 0 = unlimited")
	flag.Float64Var(&cfg.TokensPerSecond, "tokens-per-second", cfg.TokensPerSecond, "stream speed; 0 = unthrottled")
	flag.IntVar(&cfg.OutputTokens, "output-tokens", cfg.OutputTokens, "tokens per reply")
	flag.Parse()

	slog.Info("mock provider listening", "addr", *addr, "name", *name)
	srv := &http.Server{Addr: *addr, Handler: mock.New(*name, cfg), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("mock stopped", "err", err)
		os.Exit(1)
	}
}
