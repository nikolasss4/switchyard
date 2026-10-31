// Package config loads the gateway's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole gateway configuration.
type Config struct {
	Listen        string     `yaml:"listen"`
	MetricsListen string     `yaml:"metrics_listen"` // /metrics and the admin API; keep it off the public network
	Providers     []Provider `yaml:"providers"`
	Models        []Model    `yaml:"models"`
	Rules         []Rule     `yaml:"rules"`
	Retry         Retry      `yaml:"retry"`
	Breaker       Breaker    `yaml:"breaker"`
	Limits        Limits     `yaml:"limits"`
	Pricing       string     `yaml:"pricing"`
	Embedder      *Target    `yaml:"embedder"` // used by the semantic cache
	// AllowDirect lets clients name "provider/model" instead of an alias.
	// Off by default: it bypasses the aliases an operator chose to expose.
	AllowDirect bool `yaml:"allow_direct"`
}

// Provider is one upstream.
type Provider struct {
	Name           string            `yaml:"name"`
	Type           string            `yaml:"type"` // anthropic | openai
	BaseURL        string            `yaml:"base_url"`
	APIKeyEnv      string            `yaml:"api_key_env"`
	MaxTokensField string            `yaml:"max_tokens_field"`
	Headers        map[string]string `yaml:"headers"`
}

// Model is a model alias and the ordered fallback chain behind it.
type Model struct {
	Alias      string        `yaml:"alias"`
	Targets    []Target      `yaml:"targets"`
	Timeout    time.Duration `yaml:"timeout"`     // per attempt
	HedgeAfter time.Duration `yaml:"hedge_after"` // 0 = no hedging
	Cache      Cache         `yaml:"cache"`
}

// Target is a provider and the model name to send it.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Cache configures response caching for one alias.
type Cache struct {
	Exact    bool          `yaml:"exact"`
	TTL      time.Duration `yaml:"ttl"`
	Semantic struct {
		Enabled   bool    `yaml:"enabled"`
		Threshold float64 `yaml:"threshold"`
	} `yaml:"semantic"`
}

// Rule rewrites the requested model when all its conditions match.
type Rule struct {
	When struct {
		Tenant  string            `yaml:"tenant"`
		Model   string            `yaml:"model"`
		Headers map[string]string `yaml:"headers"`
	} `yaml:"when"`
	Use string `yaml:"use"`
}

// Retry is the retry policy for one target before falling back.
type Retry struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
}

// Breaker configures the per-provider circuit breaker.
type Breaker struct {
	Window         int           `yaml:"window"`       // outcomes kept
	MinRequests    int           `yaml:"min_requests"` // before the rate is trusted
	FailureRate    float64       `yaml:"failure_rate"` // opens at or above this
	OpenFor        time.Duration `yaml:"open_for"`     // before a half-open probe
	HalfOpenProbes int           `yaml:"half_open_probes"`
}

// Limits are the defaults for keys that set no limits of their own.
type Limits struct {
	RPM int `yaml:"rpm"`
	TPM int `yaml:"tpm"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses and validates YAML.
func Parse(b []byte) (*Config, error) {
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MetricsListen == "" {
		c.MetricsListen = ":9091"
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 2
	}
	if c.Retry.BaseDelay == 0 {
		c.Retry.BaseDelay = 100 * time.Millisecond
	}
	if c.Retry.MaxDelay == 0 {
		c.Retry.MaxDelay = 2 * time.Second
	}
	b := &c.Breaker
	if b.Window == 0 {
		b.Window = 20
	}
	if b.MinRequests == 0 {
		b.MinRequests = 10
	}
	if b.FailureRate == 0 {
		b.FailureRate = 0.5
	}
	if b.OpenFor == 0 {
		b.OpenFor = 10 * time.Second
	}
	if b.HalfOpenProbes == 0 {
		b.HalfOpenProbes = 3
	}
	for i := range c.Models {
		m := &c.Models[i]
		if m.Timeout == 0 {
			m.Timeout = 120 * time.Second
		}
		if m.Cache.TTL == 0 {
			m.Cache.TTL = 10 * time.Minute
		}
		if m.Cache.Semantic.Threshold == 0 {
			m.Cache.Semantic.Threshold = 0.95
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || names[p.Name] {
			errs = append(errs, fmt.Errorf("provider %q: names must be set and unique", p.Name))
		}
		names[p.Name] = true
		if p.Type != "anthropic" && p.Type != "openai" {
			errs = append(errs, fmt.Errorf("provider %q: type must be anthropic or openai", p.Name))
		}
		// Upstream URLs come only from this file, never from requests, so a
		// client cannot point the gateway at an internal address.
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider %q: base_url must be an http(s) URL", p.Name))
		}
	}
	aliases := map[string]bool{}
	for _, m := range c.Models {
		if m.Alias == "" || aliases[m.Alias] {
			errs = append(errs, fmt.Errorf("model %q: aliases must be set and unique", m.Alias))
		}
		aliases[m.Alias] = true
		if len(m.Targets) == 0 {
			errs = append(errs, fmt.Errorf("model %q: needs at least one target", m.Alias))
		}
		for _, t := range m.Targets {
			if !names[t.Provider] {
				errs = append(errs, fmt.Errorf("model %q: unknown provider %q", m.Alias, t.Provider))
			}
		}
		if s := m.Cache.Semantic; s.Enabled && (s.Threshold <= 0 || s.Threshold > 1) {
			errs = append(errs, fmt.Errorf("model %q: semantic threshold must be in (0, 1]", m.Alias))
		}
	}
	for i, r := range c.Rules {
		if !aliases[r.Use] {
			errs = append(errs, fmt.Errorf("rules[%d]: use %q is not a model alias", i, r.Use))
		}
	}
	if c.Embedder != nil && !names[c.Embedder.Provider] {
		errs = append(errs, fmt.Errorf("embedder: unknown provider %q", c.Embedder.Provider))
	}
	return errors.Join(errs...)
}
