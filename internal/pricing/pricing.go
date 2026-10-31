// Package pricing computes request cost from a versioned price table and the
// usage the provider reported.
package pricing

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nikolasss4/switchyard/internal/llm"
)

// Price is USD per million tokens.
type Price struct {
	Input       float64      `yaml:"input"`
	Output      float64      `yaml:"output"`
	CacheRead   *float64     `yaml:"cache_read"`  // nil = input price
	CacheWrite  *float64     `yaml:"cache_write"` // nil = input price
	LongContext *LongContext `yaml:"long_context"`
}

// LongContext replaces the input and output prices when a request's total
// input exceeds AboveInputTokens.
type LongContext struct {
	AboveInputTokens int     `yaml:"above_input_tokens"`
	Input            float64 `yaml:"input"`
	Output           float64 `yaml:"output"`
}

// Table is a versioned set of prices. Keys are model names, or a prefix
// ending in "/*" that matches every model under it.
type Table struct {
	Version string           `yaml:"version"`
	Models  map[string]Price `yaml:"models"`
}

// Load reads a price table.
func Load(path string) (*Table, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses a price table.
func Parse(b []byte) (*Table, error) {
	t := &Table{}
	if err := yaml.Unmarshal(b, t); err != nil {
		return nil, err
	}
	if t.Version == "" {
		return nil, fmt.Errorf("price table needs a version")
	}
	return t, nil
}

// Lookup finds a model's price: exact match first, then the longest "/*" prefix.
func (t *Table) Lookup(model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	if p, ok := t.Models[model]; ok {
		return p, true
	}
	best, found := "", false
	var bestPrice Price
	for k, p := range t.Models {
		prefix, ok := strings.CutSuffix(k, "*")
		if ok && strings.HasPrefix(model, prefix) && len(prefix) > len(best) {
			best, bestPrice, found = prefix, p, true
		}
	}
	return bestPrice, found
}

// Cost returns the USD cost of usage on model, and whether the model was priced.
// Unpriced models cost zero and should be surfaced to the operator.
func (t *Table) Cost(model string, u llm.Usage) (float64, bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	return p.Cost(u), true
}

// CostFor prices a request served by provider: "provider/model" is tried
// first (so "local/*" covers every model of a provider named local), then model.
func (t *Table) CostFor(provider, model string, u llm.Usage) (float64, bool) {
	if c, ok := t.Cost(provider+"/"+model, u); ok {
		return c, true
	}
	return t.Cost(model, u)
}

// Cost computes the cost of usage at this price.
func (p Price) Cost(u llm.Usage) float64 {
	in, out := p.Input, p.Output
	cacheRead, cacheWrite := in, in
	if p.CacheRead != nil {
		cacheRead = *p.CacheRead
	}
	if p.CacheWrite != nil {
		cacheWrite = *p.CacheWrite
	}
	totalIn := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if lc := p.LongContext; lc != nil && totalIn > lc.AboveInputTokens {
		// Cache multipliers keep their ratio to the input price.
		ratio := lc.Input / in
		if in == 0 {
			ratio = 1
		}
		in, out, cacheRead, cacheWrite = lc.Input, lc.Output, cacheRead*ratio, cacheWrite*ratio
	}
	return (float64(u.InputTokens)*in + float64(u.OutputTokens)*out +
		float64(u.CacheReadInputTokens)*cacheRead + float64(u.CacheCreationInputTokens)*cacheWrite) / 1e6
}
