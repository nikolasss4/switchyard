// Command semeval measures the semantic cache's trade-off on a labeled set of
// near-duplicate prompt pairs: at each similarity threshold, how often a true
// paraphrase would hit (hit rate) and how often a different question would be
// answered from the cache (wrong-hit rate).
//
//	semeval -pairs bench/semantic/pairs.jsonl -hash                 # bag-of-words baseline
//	semeval -pairs ... -base-url http://127.0.0.1:8099/v1 -model all-MiniLM-L6-v2
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/nikolasss4/switchyard/internal/cache"
	"github.com/nikolasss4/switchyard/internal/mock"
	"github.com/nikolasss4/switchyard/internal/oai"
)

type pair struct {
	A, B     string
	Same     bool
	Category string
}

type row struct {
	Threshold    float64 `json:"threshold"`
	HitRate      float64 `json:"hit_rate"`
	WrongHitRate float64 `json:"wrong_hit_rate"`
	Hits         int     `json:"hits"`
	WrongHits    int     `json:"wrong_hits"`
}

func main() {
	pairsPath := flag.String("pairs", "bench/semantic/pairs.jsonl", "labeled pairs")
	baseURL := flag.String("base-url", "", "OpenAI-compatible embeddings endpoint (…/v1)")
	model := flag.String("model", "", "embedding model name")
	hash := flag.Bool("hash", false, "use the mock's hashed bag-of-words embedding")
	out := flag.String("out", "", "write JSON results here")
	flag.Parse()

	pairs := load(*pairsPath)
	texts := map[string]bool{}
	for _, p := range pairs {
		texts[p.A], texts[p.B] = true, true
	}
	var list []string
	for t := range texts {
		list = append(list, t)
	}
	sort.Strings(list)
	vecs := map[string][]float32{}
	name := *model
	if *hash {
		name = "hashed bag-of-words (mock)"
		for _, t := range list {
			vecs[t] = mock.HashEmbedding(t, mock.EmbeddingDims)
		}
	} else {
		if *baseURL == "" {
			log.Fatal("set -base-url and -model, or -hash")
		}
		embed(*baseURL, *model, list, vecs)
	}

	type scored struct {
		pair
		Sim float64 `json:"similarity"`
	}
	var all []scored
	pos, neg := 0, 0
	for _, p := range pairs {
		all = append(all, scored{p, cache.Cosine(vecs[p.A], vecs[p.B])})
		if p.Same {
			pos++
		} else {
			neg++
		}
	}
	var rows []row
	for _, t := range []float64{0.70, 0.75, 0.80, 0.85, 0.88, 0.90, 0.92, 0.94, 0.95, 0.96, 0.97, 0.98, 0.99} {
		r := row{Threshold: t}
		for _, s := range all {
			if s.Sim >= t {
				if s.Same {
					r.Hits++
				} else {
					r.WrongHits++
				}
			}
		}
		r.HitRate, r.WrongHitRate = float64(r.Hits)/float64(pos), float64(r.WrongHits)/float64(neg)
		rows = append(rows, r)
	}

	fmt.Printf("embedder: %s, %d paraphrase pairs, %d different-question pairs\n\n", name, pos, neg)
	fmt.Println("| threshold | hit rate (paraphrases) | wrong-hit rate (different questions) |")
	fmt.Println("|---:|---:|---:|")
	for _, r := range rows {
		fmt.Printf("| %.2f | %.0f%% (%d/%d) | %.0f%% (%d/%d) |\n", r.Threshold, 100*r.HitRate, r.Hits, pos, 100*r.WrongHitRate, r.WrongHits, neg)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Sim > all[j].Sim })
	fmt.Println("\nmost similar different-question pairs:")
	n := 0
	for _, s := range all {
		if !s.Same && n < 8 {
			fmt.Printf("  %.3f  %-12s %q vs %q\n", s.Sim, s.Category, s.A, s.B)
			n++
		}
	}
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{"embedder": name, "positives": pos, "negatives": neg, "thresholds": rows, "pairs": all}, "", "  ")
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func load(path string) []pair {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var out []pair
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var p pair
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			log.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func embed(baseURL, model string, texts []string, into map[string][]float32) {
	for i := 0; i < len(texts); i += 64 {
		batch := texts[i:min(i+64, len(texts))]
		b, _ := json.Marshal(oai.EmbeddingRequest{Model: model, Input: batch})
		resp, err := http.Post(strings.TrimRight(baseURL, "/")+"/embeddings", "application/json", bytes.NewReader(b))
		if err != nil {
			log.Fatal(err)
		}
		var out oai.EmbeddingResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			log.Fatal(err)
		}
		resp.Body.Close()
		for _, d := range out.Data {
			into[batch[d.Index]] = d.Embedding
		}
	}
}
