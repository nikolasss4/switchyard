// Package mock is a fake LLM provider for tests and benchmarks. It speaks the
// OpenAI Chat Completions, OpenAI Embeddings and Anthropic Messages wire
// formats, streams at a configurable speed, and injects latency, errors, rate
// limits and full outages that can be changed at runtime through /_mock/config.
package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/sse"
)

// Config controls the mock's behaviour. All fields can be changed at runtime.
type Config struct {
	LatencyMS       int     `json:"latency_ms"`        // delay before the first byte
	JitterMS        int     `json:"jitter_ms"`         // uniform extra delay in [0, jitter)
	ErrorRate       float64 `json:"error_rate"`        // fraction of requests that fail
	ErrorStatus     int     `json:"error_status"`      // status of injected errors
	Down            bool    `json:"down"`              // drop connections without a response
	RPM             int     `json:"rpm"`               // requests per minute before 429s; 0 = unlimited
	TokensPerSecond float64 `json:"tokens_per_second"` // stream speed; 0 = as fast as possible
	OutputTokens    int     `json:"output_tokens"`     // tokens generated per reply
}

// DefaultConfig is a fast, healthy provider.
func DefaultConfig() Config {
	return Config{ErrorStatus: 500, OutputTokens: 32}
}

// Stats are counters exposed at /_mock/stats.
type Stats struct {
	Requests    int64 `json:"requests"`
	Completed   int64 `json:"completed"`
	Injected    int64 `json:"injected_errors"`
	RateLimited int64 `json:"rate_limited"`
	Dropped     int64 `json:"dropped"`
	Cancelled   int64 `json:"cancelled"`
	InFlight    int64 `json:"in_flight"`
}

// Server is the mock provider.
type Server struct {
	mu     sync.RWMutex
	cfg    Config
	window struct {
		start time.Time
		count int
	}
	stats struct {
		requests, completed, injected, limited, dropped, cancelled, inflight atomic.Int64
	}
	name string
	mux  *http.ServeMux
}

// New returns a mock named name (used in response model ids and logs).
func New(name string, cfg Config) *Server {
	s := &Server{cfg: cfg, name: name, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/chat/completions", s.wrap(s.chat))
	s.mux.HandleFunc("POST /v1/messages", s.wrap(s.messages))
	s.mux.HandleFunc("POST /v1/embeddings", s.wrap(s.embeddings))
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("GET /_mock/config", s.getConfig)
	s.mux.HandleFunc("PUT /_mock/config", s.putConfig)
	s.mux.HandleFunc("GET /_mock/stats", s.getStats)
	s.mux.HandleFunc("POST /_mock/reset", s.reset)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Config returns the current configuration.
func (s *Server) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetConfig replaces the configuration.
func (s *Server) SetConfig(c Config) {
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Requests: s.stats.requests.Load(), Completed: s.stats.completed.Load(),
		Injected: s.stats.injected.Load(), RateLimited: s.stats.limited.Load(),
		Dropped: s.stats.dropped.Load(), Cancelled: s.stats.cancelled.Load(),
		InFlight: s.stats.inflight.Load(),
	}
}

type handler func(w http.ResponseWriter, r *http.Request, cfg Config) error

// wrap applies outages, rate limits, injected errors and latency before the
// endpoint runs, and counts cancellations.
func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.stats.requests.Add(1)
		s.stats.inflight.Add(1)
		defer s.stats.inflight.Add(-1)
		cfg := s.Config()
		// Reading the whole body lets net/http notice a client disconnect and
		// cancel r.Context(); it only watches the connection after the body is done.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		if cfg.Down {
			s.stats.dropped.Add(1)
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		if !s.allow(cfg.RPM) {
			s.stats.limited.Add(1)
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusTooManyRequests, "rate_limit_error", "mock rate limit exceeded")
			return
		}
		if cfg.ErrorRate > 0 && rand.Float64() < cfg.ErrorRate {
			s.stats.injected.Add(1)
			status := cfg.ErrorStatus
			if status == 0 {
				status = 500
			}
			writeError(w, r, status, "api_error", "mock injected error")
			return
		}
		delay := time.Duration(cfg.LatencyMS) * time.Millisecond
		if cfg.JitterMS > 0 {
			delay += time.Duration(rand.IntN(cfg.JitterMS)) * time.Millisecond
		}
		if !sleep(r.Context(), delay) {
			s.stats.cancelled.Add(1)
			return
		}
		if err := h(w, r, cfg); err != nil {
			if r.Context().Err() != nil {
				s.stats.cancelled.Add(1)
				return
			}
			writeError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.stats.completed.Add(1)
	}
}

func (s *Server) allow(rpm int) bool {
	if rpm <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.window.start) >= time.Minute {
		s.window.start, s.window.count = now, 0
	}
	if s.window.count >= rpm {
		return false
	}
	s.window.count++
	return true
}

// errCancelled marks a client that went away mid-response.
var errCancelled = context.Canceled

func (s *Server) chat(w http.ResponseWriter, r *http.Request, cfg Config) error {
	var req oai.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return err
	}
	prompt, lastIsTool := "", false
	for _, m := range req.Messages {
		parts, _ := m.Parts()
		for _, p := range parts {
			prompt += p.Text + " "
		}
		lastIsTool = m.Role == "tool"
	}
	limit := 0
	if req.MaxCompletionTokens != nil {
		limit = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		limit = *req.MaxTokens
	}
	reply := s.generate(prompt, cfg.OutputTokens, limit)
	var call *oai.ToolCall
	if len(req.Tools) > 0 && !lastIsTool {
		call = &oai.ToolCall{
			ID: "call_" + shortHash(prompt), Type: "function",
			Function: oai.FunctionCall{Name: req.Tools[0].Function.Name, Arguments: argsFor(req.Tools[0].Function.Parameters)},
		}
	}
	inTok := countTokens(prompt)
	id := "chatcmpl-mock-" + shortHash(prompt)
	created := time.Now().Unix()
	model := req.Model

	if !req.Stream {
		if !sleep(r.Context(), genTime(len(reply.words), cfg.TokensPerSecond)) {
			return errCancelled
		}
		msg := &oai.RespMsg{Role: "assistant"}
		finish := reply.finish
		if call != nil {
			msg.ToolCalls = []oai.ToolCall{*call}
			finish = "tool_calls"
		} else {
			text := strings.Join(reply.words, " ")
			msg.Content = &text
		}
		writeJSON(w, http.StatusOK, oai.ChatResponse{
			ID: id, Object: "chat.completion", Created: created, Model: model,
			Choices: []oai.Choice{{Index: 0, Message: msg, FinishReason: &finish}},
			Usage:   &oai.Usage{PromptTokens: inTok, CompletionTokens: len(reply.words), TotalTokens: inTok + len(reply.words)},
		})
		return nil
	}

	sw := sse.NewWriter(w)
	chunk := func(c oai.Chunk) error {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, model
		b, _ := json.Marshal(c)
		return sw.Write("", b)
	}
	empty := ""
	if err := chunk(oai.Chunk{Choices: []oai.Choice{{Delta: &oai.RespMsg{Role: "assistant", Content: &empty}}}}); err != nil {
		return errCancelled
	}
	finish := reply.finish
	outTok := len(reply.words)
	if call != nil {
		finish = "tool_calls"
		args := call.Function.Arguments
		half := len(args) / 2
		first := oai.ToolCall{Index: llm.Idx(0), ID: call.ID, Type: "function", Function: oai.FunctionCall{Name: call.Function.Name, Arguments: args[:half]}}
		if err := chunk(oai.Chunk{Choices: []oai.Choice{{Delta: &oai.RespMsg{ToolCalls: []oai.ToolCall{first}}}}}); err != nil {
			return errCancelled
		}
		rest := oai.ToolCall{Index: llm.Idx(0), Function: oai.FunctionCall{Arguments: args[half:]}}
		if err := chunk(oai.Chunk{Choices: []oai.Choice{{Delta: &oai.RespMsg{ToolCalls: []oai.ToolCall{rest}}}}}); err != nil {
			return errCancelled
		}
	} else {
		per := perToken(cfg.TokensPerSecond)
		for i, word := range reply.words {
			if i > 0 && !sleep(r.Context(), per) {
				return errCancelled
			}
			text := word
			if i > 0 {
				text = " " + word
			}
			if err := chunk(oai.Chunk{Choices: []oai.Choice{{Delta: &oai.RespMsg{Content: &text}}}}); err != nil {
				return errCancelled
			}
		}
	}
	if err := chunk(oai.Chunk{Choices: []oai.Choice{{Delta: &oai.RespMsg{}, FinishReason: &finish}}}); err != nil {
		return errCancelled
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		if err := chunk(oai.Chunk{Choices: []oai.Choice{}, Usage: &oai.Usage{PromptTokens: inTok, CompletionTokens: outTok, TotalTokens: inTok + outTok}}); err != nil {
			return errCancelled
		}
	}
	if err := sw.Write("", []byte("[DONE]")); err != nil {
		return errCancelled
	}
	return nil
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, cfg Config) error {
	var req llm.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return err
	}
	if req.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens: field required")
	}
	if req.Temperature != nil || req.TopP != nil {
		// Mirrors the 2026-10-21 API version, which rejects sampling parameters.
		return fmt.Errorf("temperature and top_p are not supported on this API version")
	}
	prompt := req.SystemText() + " "
	lastIsToolResult := false
	for _, m := range req.Messages {
		prompt += m.Text() + " "
		lastIsToolResult = false
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				lastIsToolResult = true
				prompt += b.ToolResultText() + " "
			}
		}
	}
	reply := s.generate(prompt, cfg.OutputTokens, req.MaxTokens)
	stop := map[string]string{"stop": "end_turn", "length": "max_tokens"}[reply.finish]
	var toolBlock *llm.Block
	if len(req.Tools) > 0 && !lastIsToolResult {
		toolBlock = &llm.Block{Type: "tool_use", ID: "toolu_mock_" + shortHash(prompt), Name: req.Tools[0].Name, Input: json.RawMessage(argsFor(req.Tools[0].InputSchema))}
		stop = "tool_use"
	}
	inTok := countTokens(prompt)
	id := "msg_mock_" + shortHash(prompt)
	text := strings.Join(reply.words, " ")

	if !req.Stream {
		if !sleep(r.Context(), genTime(len(reply.words), cfg.TokensPerSecond)) {
			return errCancelled
		}
		content := []llm.Block{{Type: "text", Text: text}}
		if toolBlock != nil {
			content = []llm.Block{*toolBlock}
		}
		writeJSON(w, http.StatusOK, llm.Response{
			ID: id, Type: "message", Role: "assistant", Model: req.Model, Content: content,
			StopReason: stop, Usage: llm.Usage{InputTokens: inTok, OutputTokens: len(reply.words)},
		})
		return nil
	}

	sw := sse.NewWriter(w)
	send := func(e llm.Event) error {
		b, _ := json.Marshal(e)
		return sw.Write(e.Type, b)
	}
	start := llm.Response{ID: id, Type: "message", Role: "assistant", Model: req.Model, Content: []llm.Block{}, Usage: llm.Usage{InputTokens: inTok, OutputTokens: 1}}
	if err := send(llm.Event{Type: llm.EventMessageStart, Message: &start}); err != nil {
		return errCancelled
	}
	if toolBlock != nil {
		input := string(toolBlock.Input)
		evs := []llm.Event{
			{Type: llm.EventContentBlockStart, Index: llm.Idx(0), ContentBlock: &llm.Block{Type: "tool_use", ID: toolBlock.ID, Name: toolBlock.Name, Input: json.RawMessage("{}")}},
			{Type: llm.EventContentBlockDelta, Index: llm.Idx(0), Delta: &llm.Delta{Type: "input_json_delta", PartialJSON: input[:len(input)/2]}},
			{Type: llm.EventContentBlockDelta, Index: llm.Idx(0), Delta: &llm.Delta{Type: "input_json_delta", PartialJSON: input[len(input)/2:]}},
			{Type: llm.EventContentBlockStop, Index: llm.Idx(0)},
		}
		for _, e := range evs {
			if err := send(e); err != nil {
				return errCancelled
			}
		}
	} else {
		if err := send(llm.Event{Type: llm.EventContentBlockStart, Index: llm.Idx(0), ContentBlock: &llm.Block{Type: "text"}}); err != nil {
			return errCancelled
		}
		per := perToken(cfg.TokensPerSecond)
		for i, word := range reply.words {
			if i > 0 && !sleep(r.Context(), per) {
				return errCancelled
			}
			t := word
			if i > 0 {
				t = " " + word
			}
			if err := send(llm.Event{Type: llm.EventContentBlockDelta, Index: llm.Idx(0), Delta: &llm.Delta{Type: "text_delta", Text: t}}); err != nil {
				return errCancelled
			}
		}
		if err := send(llm.Event{Type: llm.EventContentBlockStop, Index: llm.Idx(0)}); err != nil {
			return errCancelled
		}
	}
	if err := send(llm.Event{Type: llm.EventMessageDelta, Delta: &llm.Delta{StopReason: stop}, Usage: &llm.Usage{OutputTokens: len(reply.words)}}); err != nil {
		return errCancelled
	}
	if err := send(llm.Event{Type: llm.EventMessageStop}); err != nil {
		return errCancelled
	}
	return nil
}

// EmbeddingDims is the size of the mock's embedding vectors.
const EmbeddingDims = 256

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request, _ Config) error {
	var req oai.EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return err
	}
	if len(req.Input) == 0 {
		return fmt.Errorf("input: field required")
	}
	resp := oai.EmbeddingResponse{Object: "list", Model: req.Model}
	for i, in := range req.Input {
		resp.Data = append(resp.Data, oai.Embedding{Object: "embedding", Index: i, Embedding: HashEmbedding(in, EmbeddingDims)})
		resp.Usage.PromptTokens += countTokens(in)
	}
	resp.Usage.TotalTokens = resp.Usage.PromptTokens
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]string{{"id": s.name, "object": "model"}}})
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Config())
}

// putConfig merges the given fields into the current configuration.
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg := s.cfg
	err := json.NewDecoder(r.Body).Decode(&cfg)
	if err == nil {
		s.cfg = cfg
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) getStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Stats())
}

func (s *Server) reset(w http.ResponseWriter, _ *http.Request) {
	for _, c := range []*atomic.Int64{&s.stats.requests, &s.stats.completed, &s.stats.injected, &s.stats.limited, &s.stats.dropped, &s.stats.cancelled} {
		c.Store(0)
	}
	w.WriteHeader(http.StatusNoContent)
}

type reply struct {
	words  []string
	finish string // stop | length
}

var vocab = strings.Fields("the a gateway routes each request to a provider and streams tokens back while it meters usage checks budgets and records traces so operators can see latency cost and errors per model")

// generate returns a deterministic reply for prompt: n words, cut to limit.
func (s *Server) generate(prompt string, n, limit int) reply {
	if n <= 0 {
		n = 32
	}
	finish := "stop"
	if limit > 0 && limit < n {
		n, finish = limit, "length"
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(prompt))
	seed := h.Sum64()
	words := make([]string, n)
	for i := range words {
		words[i] = vocab[(seed+uint64(i)*2654435761)%uint64(len(vocab))]
	}
	return reply{words: words, finish: finish}
}

// argsFor builds tool arguments that satisfy the schema's required fields.
func argsFor(schema json.RawMessage) string {
	var sc struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(schema, &sc)
	args := map[string]any{}
	for _, name := range sc.Required {
		switch sc.Properties[name].Type {
		case "number", "integer":
			args[name] = 1
		case "boolean":
			args[name] = true
		case "array":
			args[name] = []any{}
		case "object":
			args[name] = map[string]any{}
		default:
			args[name] = "mock"
		}
	}
	b, _ := json.Marshal(args)
	return string(b)
}

// HashEmbedding is a deterministic bag-of-words embedding (feature hashing of
// words and word bigrams, L2-normalised). Similar texts share features, so it
// is good enough to exercise a semantic cache in tests; it is not a model.
func HashEmbedding(text string, dims int) []float32 {
	v := make([]float64, dims)
	words := strings.Fields(strings.ToLower(text))
	add := func(feat string, weight float64) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(feat))
		x := h.Sum32()
		sign := 1.0
		if x&1 == 1 {
			sign = -1
		}
		v[int(x>>1)%dims] += sign * weight
	}
	for i, w := range words {
		w = strings.Trim(w, ".,!?;:\"'()")
		add(w, 1)
		if i > 0 {
			add(words[i-1]+" "+w, 0.5)
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	out := make([]float32, dims)
	for i, x := range v {
		if norm > 0 {
			out[i] = float32(x / norm)
		}
	}
	return out
}

func countTokens(s string) int { return max(1, len(strings.Fields(s))) }

func shortHash(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

func perToken(tps float64) time.Duration {
	if tps <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / tps)
}

func genTime(tokens int, tps float64) time.Duration {
	if tps <= 0 || tokens <= 1 {
		return 0
	}
	return time.Duration(tokens-1) * perToken(tps)
}

// sleep waits for d or until ctx is done; it reports whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers in the error envelope of the endpoint's API.
func writeError(w http.ResponseWriter, r *http.Request, status int, typ, msg string) {
	if strings.HasSuffix(r.URL.Path, "/messages") {
		writeJSON(w, status, map[string]any{"type": "error", "error": llm.APIError{Type: typ, Message: msg}})
		return
	}
	writeJSON(w, status, oai.ErrorResponse{Error: oai.ErrorBody{Message: msg, Type: typ}})
}
