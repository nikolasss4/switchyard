package main

import (
	"bytes"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"time"
)

// traffic sends a steady mix of demo requests so the dashboard has something
// to show: both surfaces, streamed and not, every demo alias, a few repeated
// prompts for the caches and a burst on a tightly limited key now and then.
func (e *env) traffic() (any, error) {
	limited := ""
	var out struct{ Key string }
	if err := e.adminDo("POST", "/admin/api/keys", map[string]any{"tenant_id": "bench", "name": "traffic-limited", "rpm": 30}, &out); err == nil {
		limited = out.Key
	}
	questions := []string{"What is the capital of France?", "what is the capital of france", "Explain a circuit breaker.",
		"Why never retry a stream after the first token?", "Give me a pancake recipe.", "How does a token bucket work?"}
	aliases := []string{"fast", "fast", "smart", "hedged", "faq"}
	client := newClient(256)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	n := 0
	for range tick.C {
		n++
		key := e.key
		if limited != "" && n%400 < 40 {
			key = limited // a burst that runs into the rate limit
		}
		alias := aliases[rand.IntN(len(aliases))]
		q := questions[rand.IntN(len(questions))]
		stream := rand.IntN(3) == 0
		go func() {
			var req *http.Request
			if rand.IntN(2) == 0 {
				body := fmt.Sprintf(`{"model":%q,"stream":%v,"max_tokens":48,"messages":[{"role":"user","content":%q}]}`, alias, stream, q)
				req, _ = http.NewRequest("POST", e.gateway+"/v1/chat/completions", bytes.NewBufferString(body))
				req.Header.Set("Authorization", "Bearer "+key)
			} else {
				body := fmt.Sprintf(`{"model":%q,"stream":%v,"max_tokens":48,"messages":[{"role":"user","content":%q}]}`, alias, stream, q)
				req, _ = http.NewRequest("POST", e.gateway+"/v1/messages", bytes.NewBufferString(body))
				req.Header.Set("x-api-key", key)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("traffic: %v", err)
				return
			}
			_, _ = ioDiscard(resp)
		}()
	}
	return nil, nil
}
