// Package server implements the gateway's HTTP surfaces: the OpenAI-compatible
// /v1/chat/completions and /v1/embeddings, and the Anthropic-native /v1/messages.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nikolasss4/switchyard/internal/auth"
	"github.com/nikolasss4/switchyard/internal/budget"
	"github.com/nikolasss4/switchyard/internal/gateway"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
	"github.com/nikolasss4/switchyard/internal/translate"
)

// maxBody caps request bodies (images are sent inline as base64).
const maxBody = 32 << 20

// Server holds the HTTP handlers.
type Server struct {
	gw   *gateway.Gateway
	auth *auth.Authenticator
	log  *slog.Logger
	now  func() time.Time
}

// New returns a server.
func New(gw *gateway.Gateway, a *auth.Authenticator, log *slog.Logger) *Server {
	return &Server{gw: gw, auth: a, log: log, now: time.Now}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.authed(openAISurface{}, s.chatCompletions))
	mux.HandleFunc("POST /v1/embeddings", s.authed(openAISurface{}, s.embeddings))
	mux.HandleFunc("POST /v1/messages", s.authed(anthropicSurface{}, s.messages))
	mux.HandleFunc("GET /v1/models", s.authed(openAISurface{}, s.models))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

// surface writes responses in one API's format.
type surface interface {
	name() string
	writeError(w http.ResponseWriter, err error)
}

type openAISurface struct{}

func (openAISurface) name() string { return "openai" }
func (openAISurface) writeError(w http.ResponseWriter, err error) {
	ae := toAPIError(err)
	writeRetryAfter(w, ae.RetryAfter)
	writeJSON(w, ae.Status, oai.ErrorResponse{Error: oai.ErrorBody{Message: ae.Message, Type: ae.Type}})
}

type anthropicSurface struct{}

func (anthropicSurface) name() string { return "anthropic" }
func (anthropicSurface) writeError(w http.ResponseWriter, err error) {
	ae := toAPIError(err)
	writeRetryAfter(w, ae.RetryAfter)
	writeJSON(w, ae.Status, map[string]any{"type": "error", "error": llm.APIError{Type: ae.Type, Message: ae.Message}})
}

// authed resolves the API key from "Authorization: Bearer" (OpenAI SDKs) or
// "x-api-key" (Anthropic SDKs).
func (s *Server) authed(sf surface, h func(http.ResponseWriter, *http.Request, *auth.Key)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("x-api-key")
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			token = strings.TrimSpace(bearer)
		}
		key, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				s.log.Error("key lookup failed", "err", err)
				sf.writeError(w, &apiError{Status: http.StatusServiceUnavailable, Type: "api_error", Message: "key lookup unavailable"})
				return
			}
			sf.writeError(w, &apiError{Status: http.StatusUnauthorized, Type: "authentication_error", Message: "invalid API key"})
			return
		}
		h(w, r, key)
	}
}

// apiError is an error with the status to return.
type apiError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string { return e.Message }

func badRequest(msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}

// toAPIError maps any error to a client-facing status. Upstream client errors
// keep their status; upstream failures become 502.
func toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var ge *gateway.Error
	if errors.As(err, &ge) {
		return &apiError{Status: ge.Status, Type: ge.Type, Message: ge.Message, RetryAfter: ge.RetryAfter}
	}
	if errors.Is(err, translate.ErrInvalid) {
		return badRequest(err.Error())
	}
	if errors.Is(err, router.ErrCircuitOpen) {
		return &apiError{Status: http.StatusServiceUnavailable, Type: "overloaded_error", Message: "no healthy upstream for this model", RetryAfter: time.Second}
	}
	if errors.Is(err, router.ErrUnknownModel) {
		return &apiError{Status: http.StatusNotFound, Type: "not_found_error", Message: err.Error()}
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Status >= 400 && pe.Status < 500 && !pe.Retryable() {
			return &apiError{Status: pe.Status, Type: pe.Type, Message: pe.Message}
		}
		return &apiError{Status: http.StatusBadGateway, Type: "api_error", Message: "upstream error: " + pe.Error(), RetryAfter: pe.RetryAfter}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &apiError{Status: http.StatusGatewayTimeout, Type: "api_error", Message: "upstream timeout"}
	}
	return &apiError{Status: http.StatusInternalServerError, Type: "api_error", Message: "internal error"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((d+time.Second-1)/time.Second)))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, &apiError{Status: http.StatusRequestEntityTooLarge, Type: "request_too_large", Message: "request body too large"}
	}
	return b, nil
}

// setHeaders reports routing decisions to the client.
func (s *Server) setHeaders(w http.ResponseWriter, c *gateway.Call, o *gateway.Outcome) {
	h := w.Header()
	h.Set("X-Request-Id", c.ID)
	if o != nil {
		if o.Provider != "" {
			h.Set("X-Switchyard-Provider", o.Provider)
			h.Set("X-Switchyard-Model", o.Model)
		}
		h.Set("X-Switchyard-Attempts", itoa(o.Attempts))
		h.Set("X-Switchyard-Fallback-Depth", itoa(o.FallbackDepth))
		if o.Cache != "" {
			h.Set("X-Switchyard-Cache", o.Cache)
		}
		if o.Hedged {
			h.Set("X-Switchyard-Hedged", "true")
		}
	}
	if c.Budget == budget.Soft {
		h.Set("X-Switchyard-Budget", "soft-limit-reached")
	}
}

// serve runs a canonical request through the gateway and writes the result
// with the surface's writers.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, sf surface, key *auth.Key, req *llm.Request,
	writeResp func(*llm.Response), relay func(provider.Stream)) {
	c := s.gw.NewCall(sf.name(), key, req, r.Header)
	if err := s.gw.Admit(r.Context(), c); err != nil {
		s.setHeaders(w, c, nil)
		sf.writeError(w, err)
		s.gw.Finish(c, &gateway.Outcome{Err: err, Status: gateway.StatusOf(err)})
		return
	}
	if !req.Stream {
		resp, o, err := s.gw.Complete(r.Context(), c)
		s.setHeaders(w, c, o)
		if err != nil {
			sf.writeError(w, err)
		} else {
			writeResp(resp)
		}
		s.gw.Finish(c, o)
		return
	}
	stream, o, err := s.gw.Stream(r.Context(), c)
	s.setHeaders(w, c, o)
	if err != nil {
		sf.writeError(w, err)
		s.gw.Finish(c, o)
		return
	}
	defer stream.Close()
	relay(stream)
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request, key *auth.Key) {
	sf := openAISurface{}
	body, err := readBody(w, r)
	if err != nil {
		sf.writeError(w, err)
		return
	}
	var in oai.ChatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		sf.writeError(w, badRequest("invalid JSON: "+err.Error()))
		return
	}
	req, err := translate.FromOpenAI(&in)
	if err != nil {
		sf.writeError(w, err)
		return
	}
	created := s.now().Unix()
	includeUsage := in.StreamOptions != nil && in.StreamOptions.IncludeUsage
	s.serve(w, r, sf, key, req,
		func(resp *llm.Response) { writeJSON(w, http.StatusOK, translate.ToOpenAIResponse(resp, created)) },
		func(st provider.Stream) { relayOpenAI(w, st, translate.NewOpenAIStreamWriter(created, includeUsage)) })
}

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request, key *auth.Key) {
	sf := openAISurface{}
	body, err := readBody(w, r)
	if err != nil {
		sf.writeError(w, err)
		return
	}
	var in oai.EmbeddingRequest
	if err := json.Unmarshal(body, &in); err != nil {
		sf.writeError(w, badRequest("invalid JSON: "+err.Error()))
		return
	}
	text := strings.Join(in.Input, " ")
	req := &llm.Request{Model: in.Model, Messages: []llm.Message{{Role: "user", Content: []llm.Block{{Type: "text", Text: text}}}}}
	c := s.gw.NewCall("openai", key, req, r.Header)
	if err := s.gw.Admit(r.Context(), c); err != nil {
		sf.writeError(w, err)
		s.gw.Finish(c, &gateway.Outcome{Err: err})
		return
	}
	resp, o, err := s.gw.Embed(r.Context(), c, &in)
	s.setHeaders(w, c, o)
	if err != nil {
		sf.writeError(w, err)
	} else {
		resp.Model = in.Model
		writeJSON(w, http.StatusOK, resp)
	}
	s.gw.Finish(c, o)
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request, key *auth.Key) {
	aliases := s.gw.Router.Aliases()
	sort.Strings(aliases)
	data := make([]map[string]any, 0, len(aliases))
	for _, a := range aliases {
		if key.Allows(a) {
			data = append(data, map[string]any{"id": a, "object": "model", "created": 0, "owned_by": "switchyard"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
