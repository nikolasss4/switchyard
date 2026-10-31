package pricing

import (
	"math"
	"os"
	"testing"

	"github.com/nikolasss4/switchyard/internal/llm"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func shipped(t *testing.T) *Table {
	t.Helper()
	b, err := os.ReadFile("../../pricing/prices.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tab, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func TestCostWithCache(t *testing.T) {
	tab := shipped(t)
	// 1000 fresh input, 2000 cache read, 500 cache write, 300 output on Sonnet 5.5.
	got, ok := tab.Cost("claude-sonnet-5-5", llm.Usage{InputTokens: 1000, CacheReadInputTokens: 2000, CacheCreationInputTokens: 500, OutputTokens: 300})
	want := (1000*2.00 + 2000*0.20 + 500*2.50 + 300*10.00) / 1e6
	if !ok || !near(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestLongContextTier(t *testing.T) {
	tab := shipped(t)
	short, _ := tab.Cost("claude-haiku-5-5", llm.Usage{InputTokens: 100000, OutputTokens: 1000})
	long, _ := tab.Cost("claude-haiku-5-5", llm.Usage{InputTokens: 100001, OutputTokens: 1000})
	if !near(short, (100000*0.10+1000*0.50)/1e6) || !near(long, (100001*0.50+1000*2.50)/1e6) {
		t.Fatalf("short %v long %v", short, long)
	}
	// Cached tokens count toward the threshold and scale with the input price.
	mixed, _ := tab.Cost("claude-haiku-5-5", llm.Usage{InputTokens: 1, CacheReadInputTokens: 100000})
	if !near(mixed, (1*0.50+100000*0.05)/1e6) {
		t.Fatalf("mixed %v", mixed)
	}
}

func TestPrefixAndUnknown(t *testing.T) {
	tab := shipped(t)
	if c, ok := tab.CostFor("local", "mlx-community/Qwen2.5-3B-Instruct-4bit", llm.Usage{InputTokens: 1e6}); !ok || c != 0 {
		t.Fatalf("local: %v %v", c, ok)
	}
	if c, ok := tab.CostFor("anthropic", "claude-opus-5-5", llm.Usage{OutputTokens: 1e6}); !ok || c != 20 {
		t.Fatalf("provider fallback to model: %v %v", c, ok)
	}
	if _, ok := tab.Cost("gpt-unknown", llm.Usage{InputTokens: 1}); ok {
		t.Fatal("unknown model must be reported as unpriced")
	}
	if tab.Version == "" {
		t.Fatal("version missing")
	}
}
