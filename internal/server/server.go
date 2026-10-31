// Package server implements the gateway's HTTP surfaces: the OpenAI-compatible
// /v1/chat/completions and /v1/embeddings, and the Anthropic-native /v1/messages.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/router"
	"github.com/nikolasss4/switchyard/internal/sse"
	"github.com/nikolasss4/switchyard/internal/translate"
)

// maxBody caps request bodies (images are sent inline as base64).
const maxBody = 32 << 20

// Server holds the HTTP handlers.
type Server struct {
	router *router.Router
	log    *slog.Logger
	now    func() time.Time
}

// New returns a server.
func New(r *router.Router, log *slog.Logger) *Server {
	return &Server{router: r, log: log, now: time.Now}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/embeddings", s.embeddings)
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

// apiError is a gateway error with the status to return.
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
	if errors.Is(err, translate.ErrInvalid) {
		return badRequest(err.Error())
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
	return &apiError{Status: http.StatusInternalServerError, Type: "api_error", Message: "internal error"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d > 0 {
		secs := int(d.Seconds() + 0.999)
		w.Header().Set("Retry-After", itoa(secs))
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (s *Server) openAIError(w http.ResponseWriter, err error) {
	ae := toAPIError(err)
	writeRetryAfter(w, ae.RetryAfter)
	writeJSON(w, ae.Status, oai.ErrorResponse{Error: oai.ErrorBody{Message: ae.Message, Type: ae.Type}})
}

func (s *Server) anthropicError(w http.ResponseWriter, err error) {
	ae := toAPIError(err)
	writeRetryAfter(w, ae.RetryAfter)
	writeJSON(w, ae.Status, map[string]any{"type": "error", "error": llm.APIError{Type: ae.Type, Message: ae.Message}})
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, &apiError{Status: http.StatusRequestEntityTooLarge, Type: "request_too_large", Message: "request body too large"}
	}
	return b, nil
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	var in oai.ChatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		s.openAIError(w, badRequest("invalid JSON: "+err.Error()))
		return
	}
	req, err := translate.FromOpenAI(&in)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	route, err := s.router.Resolve(req.Model)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	created := s.now().Unix()
	if !req.Stream {
		resp, _, err := s.router.Complete(r.Context(), route, req)
		if err != nil {
			s.openAIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, translate.ToOpenAIResponse(resp, created))
		return
	}
	stream, _, err := s.router.Stream(r.Context(), route, req)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	defer stream.Close()
	includeUsage := in.StreamOptions != nil && in.StreamOptions.IncludeUsage
	s.relayOpenAI(w, stream, translate.NewOpenAIStreamWriter(created, includeUsage))
}

// relayOpenAI writes a canonical stream as OpenAI chunks. Once the first chunk
// is out, an upstream error can only be reported in-band.
func (s *Server) relayOpenAI(w http.ResponseWriter, stream provider.Stream, conv *translate.OpenAIStreamWriter) {
	sw := sse.NewWriter(w)
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			ae := toAPIError(err)
			b, _ := json.Marshal(oai.ErrorResponse{Error: oai.ErrorBody{Message: ae.Message, Type: ae.Type}})
			_ = sw.Write("", b)
			return
		}
		chunks, done := conv.Event(ev)
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			if sw.Write("", b) != nil {
				return
			}
		}
		if done {
			_ = sw.Write("", []byte("[DONE]"))
			return
		}
	}
}

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	var in oai.EmbeddingRequest
	if err := json.Unmarshal(body, &in); err != nil {
		s.openAIError(w, badRequest("invalid JSON: "+err.Error()))
		return
	}
	route, err := s.router.Resolve(in.Model)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	t := route.Targets[0]
	up := in
	up.Model = t.Model
	resp, err := t.Provider.Embed(r.Context(), &up)
	if err != nil {
		s.openAIError(w, err)
		return
	}
	resp.Model = in.Model
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	aliases := s.router.Aliases()
	sort.Strings(aliases)
	data := make([]map[string]any, 0, len(aliases))
	for _, a := range aliases {
		data = append(data, map[string]any{"id": a, "object": "model", "created": 0, "owned_by": "switchyard"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
