package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/breaker"
	"github.com/nikolasss4/switchyard/internal/budget"
	"github.com/nikolasss4/switchyard/internal/store"
)

type fakeStore struct {
	keys    auth.Static
	tenants map[string]store.Tenant
}

func (f *fakeStore) UpsertTenant(_ context.Context, t store.Tenant) error {
	f.tenants[t.ID] = t
	return nil
}
func (f *fakeStore) ListTenants(context.Context) ([]store.Tenant, error) {
	var out []store.Tenant
	for _, t := range f.tenants {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeStore) CreateKey(_ context.Context, in store.NewKey) (auth.Generated, error) {
	g, _ := auth.Generate()
	f.keys[g.ID] = &auth.Key{ID: g.ID, TenantID: in.TenantID, Name: in.Name, SecretHash: g.Hash, RPM: in.RPM, MonthlyBudgetUSD: in.MonthlyBudgetUSD, SoftBudgetRatio: 0.8}
	return g, nil
}
func (f *fakeStore) ListKeys(context.Context, string) ([]*auth.Key, error) {
	var out []*auth.Key
	for _, k := range f.keys {
		out = append(out, k)
	}
	return out, nil
}
func (f *fakeStore) RevokeKey(_ context.Context, id string) error {
	k, ok := f.keys[id]
	if !ok {
		return auth.ErrNotFound
	}
	now := time.Now()
	k.RevokedAt = &now
	return nil
}
func (f *fakeStore) SetKeyLimits(context.Context, string, store.KeyLimits) error { return nil }
func (f *fakeStore) UsageSince(context.Context, time.Time) ([]store.UsageRow, error) {
	return []store.UsageRow{{KeyID: "k", Requests: 3}}, nil
}

func setup(t *testing.T) (*httptest.Server, *fakeStore, *budget.Memory) {
	t.Helper()
	fs := &fakeStore{keys: auth.Static{}, tenants: map[string]store.Tenant{}}
	spend := budget.NewMemory()
	api := &API{Token: "admin-secret", Store: fs, Spend: spend, Log: slog.New(slog.DiscardHandler),
		Breakers: map[string]*breaker.Breaker{"p1": breaker.New(breaker.Config{})}}
	mux := http.NewServeMux()
	api.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, fs, spend
}

func do(t *testing.T, ts *httptest.Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestAdminNeedsToken(t *testing.T) {
	ts, _, _ := setup(t)
	for _, tok := range []string{"", "wrong"} {
		if code, _ := do(t, ts, tok, "GET", "/admin/api/keys", ""); code != 401 {
			t.Fatalf("token %q: %d", tok, code)
		}
	}
	if code, _ := do(t, ts, "", "GET", "/admin", ""); code != 200 {
		t.Fatal("the page itself loads without a token (it asks for one)")
	}
}

func TestKeyLifecycleNeverShowsHash(t *testing.T) {
	ts, fs, spend := setup(t)
	code, created := do(t, ts, "admin-secret", "POST", "/admin/api/keys", `{"tenant_id":"acme","name":"ci","rpm":60,"monthly_budget_usd":10}`)
	if code != 201 || !strings.HasPrefix(created["key"].(string), "sy_") {
		t.Fatalf("%d %v", code, created)
	}
	id := created["id"].(string)
	_ = spend.Add(context.Background(), id, time.Now(), 9)
	_, list := do(t, ts, "admin-secret", "GET", "/admin/api/keys", "")
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "hash") || strings.Contains(string(b), created["key"].(string)) {
		t.Fatalf("list leaks secrets: %s", b)
	}
	k := list["keys"].([]any)[0].(map[string]any)
	if k["budget_state"] != "soft" || k["spent_this_month_usd"].(float64) != 9 {
		t.Fatalf("budget view: %v", k)
	}
	if code, _ := do(t, ts, "admin-secret", "POST", "/admin/api/keys/"+id+"/revoke", ""); code != 200 || fs.keys[id].RevokedAt == nil {
		t.Fatal("revoke failed")
	}
	if code, _ := do(t, ts, "admin-secret", "POST", "/admin/api/keys/nope/revoke", ""); code != 404 {
		t.Fatalf("unknown key: %d", code)
	}
}

func TestValidation(t *testing.T) {
	ts, _, _ := setup(t)
	for _, body := range []string{`{"name":"x"}`, `{"tenant_id":"a","name":"x","rpm":-1}`, `{"tenant_id":"a","name":"x","bogus":1}`, `not json`} {
		if code, _ := do(t, ts, "admin-secret", "POST", "/admin/api/keys", body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _ := do(t, ts, "admin-secret", "GET", "/admin/api/usage?since=forever", ""); code != 400 {
		t.Error("bad since accepted")
	}
}

func TestProviders(t *testing.T) {
	ts, _, _ := setup(t)
	_, out := do(t, ts, "admin-secret", "GET", "/admin/api/providers", "")
	p := out["providers"].([]any)[0].(map[string]any)
	if p["name"] != "p1" || p["state"] != "closed" {
		t.Fatal(out)
	}
}
