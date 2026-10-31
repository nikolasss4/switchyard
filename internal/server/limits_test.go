package server

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/auth"
)

const hello = `{"model":"via-openai","max_tokens":10,"messages":[{"role":"user","content":"hello there"}]}`

func waitRecords(t *testing.T, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(h.usage.Records()) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(h.usage.Records()); got != n {
		t.Fatalf("usage records = %d, want %d", got, n)
	}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	for _, key := range []string{"", "sk-not-ours", "sy_abcdefgh_" + "x0000000000000000000000000000000"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
			resp := h.postKey(t, key, path, hello)
			if resp.StatusCode != 401 {
				t.Errorf("%s key %q: status %d", path, key, resp.StatusCode)
			}
		}
	}
}

func TestUsageAndCostRecorded(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, "/v1/chat/completions", hello)
	if resp.StatusCode != 200 || resp.Header.Get("X-Switchyard-Provider") != "mock-oai" || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	waitRecords(t, h, 1)
	r := h.usage.Records()[0]
	// mock-fast is priced at $0.50 in / $1.50 out per million tokens.
	want := (2*0.50 + 10*1.50) / 1e6
	if r.InputTokens != 2 || r.OutputTokens != 10 || math.Abs(r.CostUSD-want) > 1e-12 || r.Alias != "via-openai" || r.TenantID != "t1" || r.Status != 200 {
		t.Fatalf("record = %+v", r)
	}
	spent, _ := h.spend.Spent(t.Context(), r.KeyID, time.Now())
	if math.Abs(spent-want) > 1e-6 {
		t.Fatalf("spend = %v", spent)
	}
}

func TestStreamUsageRecorded(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, "/v1/messages", `{"model":"via-anthropic","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	collect(t, resp.Body)
	waitRecords(t, h, 1)
	r := h.usage.Records()[0]
	if !r.Stream || r.OutputTokens != 32 || r.InputTokens != 1 || r.TTFTMS == nil || r.CostUSD <= 0 || r.Status != 200 {
		t.Fatalf("record = %+v", r)
	}
}

func TestRateLimitRPM(t *testing.T) {
	h := newHarness(t)
	rpm := 2
	key, _ := h.keys.NewStaticKey(auth.Key{TenantID: "t1", RPM: &rpm})
	for i := range 2 {
		if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp := h.postKey(t, key, "/v1/messages", hello)
	var env struct {
		Error struct{ Type string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" || env.Error.Type != "rate_limit_error" {
		t.Fatalf("status %d retry-after %q type %q", resp.StatusCode, resp.Header.Get("Retry-After"), env.Error.Type)
	}
}

func TestTPMSettledWithActualUsage(t *testing.T) {
	h := newHarness(t)
	tpm := 30
	key, k := h.keys.NewStaticKey(auth.Key{TenantID: "t1", TPM: &tpm})
	// Admission reserves ~3 tokens; the call really uses 2 in + 10 out = 12.
	if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitRecords(t, h, 1)
	if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitRecords(t, h, 2)
	// 30 - 24 = 6 left: enough to admit a third request's estimate...
	if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitRecords(t, h, 3)
	// ...which pushes the bucket into debt, so the fourth is refused.
	if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 429 {
		t.Fatalf("fourth request: %d (key %s)", resp.StatusCode, k.ID)
	}
}

func TestBudgetSoftAndHard(t *testing.T) {
	h := newHarness(t)
	limit := 1.0
	key, k := h.keys.NewStaticKey(auth.Key{TenantID: "t1", MonthlyBudgetUSD: &limit, SoftBudgetRatio: 0.5})
	_ = h.spend.Add(t.Context(), k.ID, time.Now(), 0.6)
	resp := h.postKey(t, key, "/v1/chat/completions", hello)
	if resp.StatusCode != 200 || resp.Header.Get("X-Switchyard-Budget") != "soft-limit-reached" {
		t.Fatalf("soft: %d %q", resp.StatusCode, resp.Header.Get("X-Switchyard-Budget"))
	}
	_ = h.spend.Add(t.Context(), k.ID, time.Now(), 0.4)
	resp = h.postKey(t, key, "/v1/messages", hello)
	var env struct {
		Error struct{ Type string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if resp.StatusCode != 402 || env.Error.Type != "budget_exceeded" {
		t.Fatalf("hard: %d %q", resp.StatusCode, env.Error.Type)
	}
}

func TestModelAllowlist(t *testing.T) {
	h := newHarness(t)
	key, _ := h.keys.NewStaticKey(auth.Key{TenantID: "t1", AllowedModels: []string{"via-anthropic"}})
	if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 403 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBudgetRefusalDoesNotSpendRateTokens(t *testing.T) {
	h := newHarness(t)
	limit, rpm := 0.0, 1
	key, _ := h.keys.NewStaticKey(auth.Key{TenantID: "t1", MonthlyBudgetUSD: &limit, RPM: &rpm})
	for range 3 {
		if resp := h.postKey(t, key, "/v1/chat/completions", hello); resp.StatusCode != 402 {
			t.Fatalf("status %d, want 402 every time (not 429)", resp.StatusCode)
		}
	}
}
