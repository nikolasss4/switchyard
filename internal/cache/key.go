// Package cache implements the response caches: an exact-match cache in Redis
// keyed by a fingerprint of the whole request, and an opt-in semantic cache
// that embeds the last user message and looks for a close enough earlier one
// in pgvector.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/nikolasss4/switchyard/internal/llm"
)

// Fingerprint identifies a request for the exact cache. It covers the tenant
// (entries are never shared across tenants), the resolved alias and every
// field that can change the answer; the stream flag and requested model name
// are left out, so streamed and non-streamed calls share entries.
func Fingerprint(tenant, alias string, r *llm.Request) (string, error) {
	var body []byte
	if len(r.Raw) > 0 {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r.Raw, &m); err != nil {
			return "", err
		}
		delete(m, "model")
		delete(m, "stream")
		var err error
		if body, err = json.Marshal(m); err != nil { // map keys are sorted
			return "", err
		}
	} else {
		c := *r
		c.Model, c.Stream = "", false
		var err error
		if body, err = json.Marshal(c); err != nil {
			return "", err
		}
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(tenant), []byte(alias), body} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SemanticInput splits a request into the text to embed (the last user
// message) and a scope hash of everything else (tenant, alias, system prompt,
// earlier turns, max_tokens). Entries only match within a scope. Requests with
// tools, images or a non-text last turn are not eligible.
func SemanticInput(tenant, alias string, r *llm.Request) (text, scope string, ok bool) {
	if len(r.Tools) > 0 || len(r.Messages) == 0 {
		return "", "", false
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role != "user" {
		return "", "", false
	}
	for _, b := range last.Content {
		if b.Type != "text" {
			return "", "", false
		}
	}
	text = last.Text()
	if text == "" {
		return "", "", false
	}
	prefix := struct {
		System    string        `json:"s"`
		History   []llm.Message `json:"h"`
		MaxTokens int           `json:"m"`
	}{r.SystemText(), r.Messages[:len(r.Messages)-1], r.MaxTokens}
	b, err := json.Marshal(prefix)
	if err != nil {
		return "", "", false
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(tenant), []byte(alias), b} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return text, hex.EncodeToString(h.Sum(nil)), true
}
