// Package admin is the small management API and page: tenants, keys, limits,
// usage, budgets and provider health. It is served on the operations listener
// and needs the admin token.
package admin

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/breaker"
	"github.com/nikolasss4/switchyard/internal/budget"
	"github.com/nikolasss4/switchyard/internal/store"
)

//go:embed web/index.html
var page []byte

// Store is the subset of the Postgres store the admin API uses.
type Store interface {
	UpsertTenant(ctx context.Context, t store.Tenant) error
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	CreateKey(ctx context.Context, in store.NewKey) (auth.Generated, error)
	ListKeys(ctx context.Context, tenantID string) ([]*auth.Key, error)
	RevokeKey(ctx context.Context, id string) error
	SetKeyLimits(ctx context.Context, id string, l store.KeyLimits) error
	UsageSince(ctx context.Context, since time.Time) ([]store.UsageRow, error)
}

// API serves /admin.
type API struct {
	Token    string
	Store    Store
	Spend    budget.Counter
	Breakers map[string]*breaker.Breaker
	Auth     *auth.Authenticator
	Log      *slog.Logger
}

// Register adds the admin routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		w.Header().Set("X-Frame-Options", "DENY")
		_, _ = w.Write(page)
	})
	h := func(f func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
		return a.authed(func(w http.ResponseWriter, r *http.Request) {
			if err := f(w, r); err != nil {
				status := http.StatusInternalServerError
				var he *httpError
				switch {
				case errors.As(err, &he):
					status = he.status
				case errors.Is(err, auth.ErrNotFound):
					status = http.StatusNotFound
				default:
					a.Log.Error("admin request failed", "path", r.URL.Path, "err", err)
				}
				writeJSON(w, status, map[string]string{"error": err.Error()})
			}
		})
	}
	mux.HandleFunc("GET /admin/api/tenants", h(a.listTenants))
	mux.HandleFunc("PUT /admin/api/tenants/{id}", h(a.putTenant))
	mux.HandleFunc("GET /admin/api/keys", h(a.listKeys))
	mux.HandleFunc("POST /admin/api/keys", h(a.createKey))
	mux.HandleFunc("POST /admin/api/keys/{id}/revoke", h(a.revokeKey))
	mux.HandleFunc("PUT /admin/api/keys/{id}/limits", h(a.setLimits))
	mux.HandleFunc("GET /admin/api/usage", h(a.usage))
	mux.HandleFunc("GET /admin/api/providers", h(a.providers))
}

func (a *API) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if a.Token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "admin token required"})
			return
		}
		next(w, r)
	}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func bad(msg string) error { return &httpError{http.StatusBadRequest, msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bad("invalid JSON: " + err.Error())
	}
	return nil
}

func (a *API) listTenants(w http.ResponseWriter, r *http.Request) error {
	ts, err := a.Store.ListTenants(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": ts})
	return nil
}

func (a *API) putTenant(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name           string `json:"name"`
		CapturePrompts bool   `json:"capture_prompts"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t := store.Tenant{ID: r.PathValue("id"), Name: in.Name, CapturePrompts: in.CapturePrompts}
	if t.Name == "" {
		t.Name = t.ID
	}
	if err := a.Store.UpsertTenant(r.Context(), t); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

// keyView is a key as the API shows it: never the hash.
type keyView struct {
	ID               string     `json:"id"`
	TenantID         string     `json:"tenant_id"`
	Name             string     `json:"name"`
	RPM              *int       `json:"rpm"`
	TPM              *int       `json:"tpm"`
	MonthlyBudgetUSD *float64   `json:"monthly_budget_usd"`
	SoftBudgetRatio  float64    `json:"soft_budget_ratio"`
	AllowedModels    []string   `json:"allowed_models"`
	CreatedAt        time.Time  `json:"created_at"`
	RevokedAt        *time.Time `json:"revoked_at"`
	SpentThisMonth   *float64   `json:"spent_this_month_usd,omitempty"`
	BudgetState      string     `json:"budget_state,omitempty"`
}

func view(k *auth.Key) keyView {
	return keyView{ID: k.ID, TenantID: k.TenantID, Name: k.Name, RPM: k.RPM, TPM: k.TPM, MonthlyBudgetUSD: k.MonthlyBudgetUSD,
		SoftBudgetRatio: k.SoftBudgetRatio, AllowedModels: k.AllowedModels, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt}
}

func (a *API) listKeys(w http.ResponseWriter, r *http.Request) error {
	keys, err := a.Store.ListKeys(r.Context(), r.URL.Query().Get("tenant"))
	if err != nil {
		return err
	}
	out := make([]keyView, 0, len(keys))
	now := time.Now()
	for _, k := range keys {
		v := view(k)
		if a.Spend != nil && k.RevokedAt == nil {
			if spent, err := a.Spend.Spent(r.Context(), k.ID, now); err == nil {
				v.SpentThisMonth = &spent
				v.BudgetState = budget.Evaluate(spent, k.MonthlyBudgetUSD, k.SoftBudgetRatio).String()
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
	return nil
}

type limitsIn struct {
	RPM              *int     `json:"rpm"`
	TPM              *int     `json:"tpm"`
	MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
	SoftBudgetRatio  float64  `json:"soft_budget_ratio"`
}

func (l limitsIn) validate() error {
	if (l.RPM != nil && *l.RPM < 0) || (l.TPM != nil && *l.TPM < 0) || (l.MonthlyBudgetUSD != nil && *l.MonthlyBudgetUSD < 0) {
		return bad("limits must not be negative")
	}
	if l.SoftBudgetRatio < 0 || l.SoftBudgetRatio > 1 {
		return bad("soft_budget_ratio must be in (0, 1]")
	}
	return nil
}

func (a *API) createKey(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		TenantID      string   `json:"tenant_id"`
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		limitsIn
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.TenantID == "" || in.Name == "" {
		return bad("tenant_id and name are required")
	}
	if err := in.validate(); err != nil {
		return err
	}
	g, err := a.Store.CreateKey(r.Context(), store.NewKey{TenantID: in.TenantID, Name: in.Name, RPM: in.RPM, TPM: in.TPM,
		MonthlyBudgetUSD: in.MonthlyBudgetUSD, SoftBudgetRatio: in.SoftBudgetRatio, AllowedModels: in.AllowedModels})
	if err != nil {
		if strings.Contains(err.Error(), "foreign key") {
			return bad("unknown tenant " + in.TenantID)
		}
		return err
	}
	a.Log.Info("key created", "key", g.ID, "tenant", in.TenantID)
	writeJSON(w, http.StatusCreated, map[string]string{"id": g.ID, "key": g.Plaintext,
		"note": "store this key now; it is not shown again"})
	return nil
}

func (a *API) revokeKey(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if err := a.Store.RevokeKey(r.Context(), id); err != nil {
		return err
	}
	if a.Auth != nil {
		a.Auth.Invalidate(id)
	}
	a.Log.Info("key revoked", "key", id)
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "revoked"})
	return nil
}

func (a *API) setLimits(w http.ResponseWriter, r *http.Request) error {
	var in limitsIn
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := a.Store.SetKeyLimits(r.Context(), id, store.KeyLimits(in)); err != nil {
		return err
	}
	if a.Auth != nil {
		a.Auth.Invalidate(id)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
	return nil
}

func (a *API) usage(w http.ResponseWriter, r *http.Request) error {
	window := 24 * time.Hour
	if s := r.URL.Query().Get("since"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return bad("since must be a duration such as 1h or 168h")
		}
		window = d
	}
	rows, err := a.Store.UsageSince(r.Context(), time.Now().Add(-window))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"since": window.String(), "rows": rows})
	return nil
}

func (a *API) providers(w http.ResponseWriter, _ *http.Request) error {
	type row struct {
		Name string `json:"name"`
		breaker.Snapshot
	}
	out := make([]row, 0, len(a.Breakers))
	for name, b := range a.Breakers {
		out = append(out, row{Name: name, Snapshot: b.Snapshot()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
	return nil
}
