// Command switchyardctl manages tenants, keys and limits through the gateway's
// admin API.
//
//	export SWITCHYARD_ADMIN_URL=http://localhost:9091 SWITCHYARD_ADMIN_TOKEN=...
//	switchyardctl tenants put acme --name "Acme Inc"
//	switchyardctl keys create --tenant acme --name ci --rpm 600 --tpm 200000 --budget 50
//	switchyardctl keys list
//	switchyardctl keys limits <id> --rpm 60
//	switchyardctl keys revoke <id>
//	switchyardctl usage --since 168h
//	switchyardctl providers
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type client struct {
	base, token string
	http        *http.Client
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	// The base URL is the operator's own SWITCHYARD_ADMIN_URL.
	req, err := http.NewRequest(method, c.base+path, rd) //nolint:noctx,gosec // short-lived CLI
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req) //nolint:gosec // see above
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	return json.Unmarshal(b, out)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, usage)
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

const usage = `usage: switchyardctl <tenants|keys|usage|providers> ...
  tenants list
  tenants put <id> [--name NAME] [--capture]
  keys list [--tenant ID]
  keys create --tenant ID --name NAME [--rpm N] [--tpm N] [--budget USD] [--soft RATIO] [--models a,b]
  keys limits <id> [--rpm N] [--tpm N] [--budget USD] [--soft RATIO]
  keys revoke <id>
  usage [--since 24h]
  providers
Environment: SWITCHYARD_ADMIN_URL (default http://localhost:9091), SWITCHYARD_ADMIN_TOKEN.
Add --json before the command for raw JSON output.`

var errUsage = errors.New("usage")

func run(args []string) error {
	asJSON := false
	if len(args) > 0 && args[0] == "--json" {
		asJSON, args = true, args[1:]
	}
	if len(args) == 0 {
		return errUsage
	}
	base := os.Getenv("SWITCHYARD_ADMIN_URL")
	if base == "" {
		base = "http://localhost:9091"
	}
	c := &client{base: strings.TrimRight(base, "/"), token: os.Getenv("SWITCHYARD_ADMIN_TOKEN"), http: &http.Client{Timeout: 30 * time.Second}}
	if c.token == "" {
		return errors.New("SWITCHYARD_ADMIN_TOKEN is not set")
	}
	print := func(v any, table func(w *tabwriter.Writer)) error {
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		table(w)
		return w.Flush()
	}

	cmd, sub := args[0], ""
	if len(args) > 1 {
		sub = args[1]
	}
	switch {
	case cmd == "tenants" && sub == "list":
		var out struct {
			Tenants []struct {
				ID, Name       string
				CapturePrompts bool `json:"capture_prompts"`
			}
		}
		if err := c.do("GET", "/admin/api/tenants", nil, &out); err != nil {
			return err
		}
		return print(out, func(w *tabwriter.Writer) {
			fmt.Fprintln(w, "ID\tNAME\tCAPTURE")
			for _, t := range out.Tenants {
				fmt.Fprintf(w, "%s\t%s\t%v\n", t.ID, t.Name, t.CapturePrompts)
			}
		})
	case cmd == "tenants" && sub == "put":
		fs := flag.NewFlagSet("tenants put", flag.ContinueOnError)
		name := fs.String("name", "", "display name")
		capture := fs.Bool("capture", false, "capture redacted prompts and completions")
		id, rest, err := positional(args[2:])
		if err != nil {
			return err
		}
		if err := fs.Parse(rest); err != nil {
			return err
		}
		var out map[string]any
		if err := c.do("PUT", "/admin/api/tenants/"+url.PathEscape(id), map[string]any{"name": *name, "capture_prompts": *capture}, &out); err != nil {
			return err
		}
		fmt.Println("tenant", id, "saved")
		return nil
	case cmd == "keys" && sub == "list":
		fs := flag.NewFlagSet("keys list", flag.ContinueOnError)
		tenant := fs.String("tenant", "", "only this tenant")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		var out struct{ Keys []map[string]any }
		if err := c.do("GET", "/admin/api/keys?tenant="+url.QueryEscape(*tenant), nil, &out); err != nil {
			return err
		}
		return print(out, func(w *tabwriter.Writer) {
			fmt.Fprintln(w, "ID\tTENANT\tNAME\tRPM\tTPM\tBUDGET\tSPENT\tSTATE")
			for _, k := range out.Keys {
				state := str(k["budget_state"])
				if k["revoked_at"] != nil {
					state = "revoked"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", str(k["id"]), str(k["tenant_id"]), str(k["name"]),
					orDefault(k["rpm"]), orDefault(k["tpm"]), money(k["monthly_budget_usd"]), money(k["spent_this_month_usd"]), state)
			}
		})
	case cmd == "keys" && (sub == "create" || sub == "limits"):
		fs := flag.NewFlagSet("keys "+sub, flag.ContinueOnError)
		tenant := fs.String("tenant", "", "tenant id")
		name := fs.String("name", "", "key name")
		rpm := fs.Int("rpm", -1, "requests per minute (-1 = gateway default)")
		tpm := fs.Int("tpm", -1, "tokens per minute (-1 = gateway default)")
		budgetUSD := fs.Float64("budget", -1, "monthly budget in USD (-1 = none)")
		soft := fs.Float64("soft", 0, "soft budget ratio (default 0.8)")
		models := fs.String("models", "", "comma-separated allowed model aliases")
		rest := args[2:]
		id := ""
		if sub == "limits" {
			var err error
			if id, rest, err = positional(rest); err != nil {
				return err
			}
		}
		if err := fs.Parse(rest); err != nil {
			return err
		}
		body := map[string]any{"soft_budget_ratio": *soft}
		if *rpm >= 0 {
			body["rpm"] = *rpm
		}
		if *tpm >= 0 {
			body["tpm"] = *tpm
		}
		if *budgetUSD >= 0 {
			body["monthly_budget_usd"] = *budgetUSD
		}
		var out map[string]any
		if sub == "limits" {
			if err := c.do("PUT", "/admin/api/keys/"+url.PathEscape(id)+"/limits", body, &out); err != nil {
				return err
			}
			fmt.Println("limits of", id, "updated")
			return nil
		}
		body["tenant_id"], body["name"] = *tenant, *name
		if *models != "" {
			body["allowed_models"] = strings.Split(*models, ",")
		}
		if err := c.do("POST", "/admin/api/keys", body, &out); err != nil {
			return err
		}
		if asJSON {
			return print(out, nil)
		}
		fmt.Printf("created key %s\n\n  %s\n\nStore it now; it is not shown again.\n", out["id"], out["key"])
		return nil
	case cmd == "keys" && sub == "revoke":
		id, _, err := positional(args[2:])
		if err != nil {
			return err
		}
		var out map[string]any
		if err := c.do("POST", "/admin/api/keys/"+url.PathEscape(id)+"/revoke", nil, &out); err != nil {
			return err
		}
		fmt.Println("revoked", id)
		return nil
	case cmd == "usage":
		fs := flag.NewFlagSet("usage", flag.ContinueOnError)
		since := fs.String("since", "24h", "window")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var out struct{ Rows []map[string]any }
		if err := c.do("GET", "/admin/api/usage?since="+url.QueryEscape(*since), nil, &out); err != nil {
			return err
		}
		return print(out, func(w *tabwriter.Writer) {
			fmt.Fprintln(w, "TENANT\tKEY\tALIAS\tPROVIDER\tMODEL\tREQUESTS\tERRORS\tCACHE HITS\tTOKENS IN\tTOKENS OUT\tCOST")
			for _, r := range out.Rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%v\t%v\t%v\t%v\t%v\t%s\n", str(r["tenant_id"]), str(r["key_id"]), str(r["alias"]),
					str(r["provider"]), str(r["model"]), r["requests"], r["errors"], r["cache_hits"], r["input_tokens"], r["output_tokens"], money(r["cost_usd"]))
			}
		})
	case cmd == "providers":
		var out struct{ Providers []map[string]any }
		if err := c.do("GET", "/admin/api/providers", nil, &out); err != nil {
			return err
		}
		return print(out, func(w *tabwriter.Writer) {
			fmt.Fprintln(w, "PROVIDER\tBREAKER\tCALLS\tFAILURE RATE")
			for _, p := range out.Providers {
				fmt.Fprintf(w, "%s\t%s\t%v\t%.0f%%\n", str(p["name"]), str(p["state"]), p["window_calls"], 100*num(p["window_failure_rate"]))
			}
		})
	}
	return errUsage
}

// positional takes the leading positional argument, so flags can follow it.
func positional(args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, errors.New("missing id")
	}
	return args[0], args[1:], nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func num(v any) float64 { f, _ := v.(float64); return f }

func orDefault(v any) string {
	if v == nil {
		return "default"
	}
	return fmt.Sprint(v)
}

func money(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("$%.4f", num(v))
}
