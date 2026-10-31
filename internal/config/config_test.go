package config

import (
	"net/http"
	"strings"
	"testing"
)

func TestDefaultsAndValidation(t *testing.T) {
	c, err := Parse([]byte(`
providers: [{name: p, type: openai, base_url: "http://localhost:9000/v1"}]
models: [{alias: fast, targets: [{provider: p, model: m}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Retry.MaxAttempts != 2 || c.Breaker.Window != 20 || c.Models[0].Cache.Semantic.Threshold != 0.95 {
		t.Fatalf("defaults: %+v", c)
	}

	_, err = Parse([]byte(`
providers:
  - {name: p, type: grpc, base_url: "file:///etc"}
  - {name: p, type: openai, base_url: "http://x"}
models:
  - {alias: a, targets: [{provider: missing, model: m}]}
  - {alias: a, targets: []}
rules: [{use: nowhere}]
`))
	for _, want := range []string{"type must be", "base_url must be", "names must be set and unique", "unknown provider", "aliases must be", "needs at least one", "not a model alias"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing error %q in %v", want, err)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	var r Rule
	r.When.Tenant, r.When.Headers = "acme", map[string]string{"X-Tier": "batch"}
	h := http.Header{}
	h.Set("x-tier", "batch")
	if !r.Matches("anything", "acme", h) || r.Matches("anything", "other", h) || r.Matches("anything", "acme", http.Header{}) {
		t.Fatal("rule matching is wrong")
	}
}
