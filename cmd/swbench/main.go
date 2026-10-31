// Command swbench runs switchyard's load and chaos benchmarks against the mock
// provider, so they measure the gateway itself. It expects two mocks
// (mockllm) and a gateway running configs/bench.yaml; see bench/README.md.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type env struct {
	gateway, admin, mockA, mockB, adminToken, key string
	out                                           string
	quick                                         bool
	client                                        *http.Client
}

func main() {
	e := &env{}
	flag.StringVar(&e.gateway, "gateway", "http://127.0.0.1:8080", "gateway URL")
	flag.StringVar(&e.admin, "admin", "http://127.0.0.1:9091", "gateway ops URL (admin API, /metrics)")
	flag.StringVar(&e.mockA, "mock-a", "http://127.0.0.1:9101", "primary mock")
	flag.StringVar(&e.mockB, "mock-b", "http://127.0.0.1:9102", "secondary mock")
	flag.StringVar(&e.adminToken, "admin-token", os.Getenv("SWITCHYARD_ADMIN_TOKEN"), "admin token")
	flag.StringVar(&e.out, "out", "bench/out", "output directory")
	flag.BoolVar(&e.quick, "quick", false, "short runs, for checking the harness")
	flag.Parse()
	which := flag.Args()
	if len(which) == 0 {
		which = []string{"overhead", "throughput", "stream", "chaos"}
	}
	e.client = newClient(4096)
	writeOut := !(len(which) == 1 && which[0] == "traffic")
	if writeOut {
		if err := os.MkdirAll(e.out, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	if e.adminToken == "" {
		log.Fatal("set SWITCHYARD_ADMIN_TOKEN or -admin-token")
	}
	if err := e.setup(); err != nil {
		log.Fatal("setup: ", err)
	}
	if writeOut {
		writeJSON(filepath.Join(e.out, "environment.json"), environment())
	}
	for _, w := range which {
		start := time.Now()
		log.Printf("== %s", w)
		var res any
		var err error
		switch w {
		case "overhead":
			res, err = e.overhead()
		case "throughput":
			res, err = e.throughput()
		case "stream":
			res, err = e.stream()
		case "chaos":
			res, err = e.chaos()
		case "traffic":
			res, err = e.traffic()
		default:
			log.Fatalf("unknown benchmark %q", w)
		}
		if err != nil {
			log.Fatalf("%s: %v", w, err)
		}
		writeJSON(filepath.Join(e.out, w+".json"), res)
		log.Printf("== %s done in %s", w, time.Since(start).Round(time.Second))
	}
}

func newClient(conns int) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost = conns, conns, 0
	return &http.Client{Transport: tr, Timeout: 120 * time.Second}
}

// setup creates a tenant and a key with limits and a budget high enough never
// to refuse, so every request still runs the limiter and budget checks.
func (e *env) setup() error {
	if err := e.adminDo("PUT", "/admin/api/tenants/bench", map[string]any{"name": "bench"}, nil); err != nil {
		return err
	}
	var out struct{ Key string }
	if err := e.adminDo("POST", "/admin/api/keys", map[string]any{"tenant_id": "bench", "name": "bench-" + time.Now().Format("150405"),
		"rpm": 100_000_000, "tpm": 2_000_000_000, "monthly_budget_usd": 1_000_000}, &out); err != nil {
		return err
	}
	e.key = out.Key
	return nil
}

func (e *env) adminDo(method, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.admin+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, rb)
	}
	if out != nil {
		return json.Unmarshal(rb, out)
	}
	return nil
}

// mockConfig replaces a mock's runtime configuration.
func (e *env) mockConfig(base string, cfg map[string]any) error {
	full := map[string]any{"latency_ms": 0, "jitter_ms": 0, "error_rate": 0, "error_status": 500, "down": false,
		"rpm": 0, "tokens_per_second": 0, "output_tokens": 32}
	for k, v := range cfg {
		full[k] = v
	}
	b, _ := json.Marshal(full)
	req, _ := http.NewRequest("PUT", base+"/_mock/config", bytes.NewReader(b))
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// processStats reads the gateway's CPU seconds, RSS and goroutines from /metrics.
type processStats struct {
	CPUSeconds float64 `json:"cpu_seconds"`
	RSSBytes   float64 `json:"rss_bytes"`
	Goroutines float64 `json:"goroutines"`
}

func (e *env) gatewayStats() processStats {
	resp, err := e.client.Get(e.admin + "/metrics")
	if err != nil {
		return processStats{}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var s processStats
	for _, line := range strings.Split(string(b), "\n") {
		var v float64
		switch {
		case strings.HasPrefix(line, "process_cpu_seconds_total "):
			fmt.Sscan(strings.Fields(line)[1], &v)
			s.CPUSeconds = v
		case strings.HasPrefix(line, "process_resident_memory_bytes "):
			fmt.Sscan(strings.Fields(line)[1], &v)
			s.RSSBytes = v
		case strings.HasPrefix(line, "go_goroutines "):
			fmt.Sscan(strings.Fields(line)[1], &v)
			s.Goroutines = v
		}
	}
	return s
}

type envInfo struct {
	Time      string `json:"time"`
	GitSHA    string `json:"git_sha"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	CPUModel  string `json:"cpu_model"`
	MemoryGB  string `json:"memory"`
	Host      string `json:"host"`
	Note      string `json:"note"`
}

func environment() envInfo {
	sha, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	if s := os.Getenv("GITHUB_SHA"); s != "" {
		sha = []byte(s)
	}
	cpu, mem := "", ""
	if runtime.GOOS == "darwin" {
		b, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		cpu = strings.TrimSpace(string(b))
		m, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
		var n float64
		fmt.Sscan(strings.TrimSpace(string(m)), &n)
		mem = fmt.Sprintf("%.0f GB", n/1e9)
	} else if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				cpu = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
		if m, err := os.ReadFile("/proc/meminfo"); err == nil {
			var kb float64
			fmt.Sscanf(strings.SplitN(string(m), "\n", 2)[0], "MemTotal: %f kB", &kb)
			mem = fmt.Sprintf("%.0f GB", kb/1e6)
		}
	}
	host := "local"
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		host = "GitHub Actions " + os.Getenv("RUNNER_OS") + " runner (" + os.Getenv("ImageOS") + ")"
	}
	return envInfo{Time: time.Now().UTC().Format(time.RFC3339), GitSHA: strings.TrimSpace(string(sha)), GoVersion: runtime.Version(),
		OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), CPUModel: cpu, MemoryGB: mem, Host: host,
		Note: "load generator, gateway, mocks, Postgres and Redis share this machine"}
}

func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}
