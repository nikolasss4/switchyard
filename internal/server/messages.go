package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/provider"
	"github.com/nikolasss4/switchyard/internal/sse"
)

// parseMessages decodes and validates an Anthropic Messages request. The raw
// body is kept so an Anthropic upstream receives it unchanged.
func parseMessages(body []byte, h http.Header) (*llm.Request, error) {
	var req llm.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, badRequest("invalid JSON: " + err.Error())
	}
	if req.MaxTokens <= 0 {
		return nil, badRequest("max_tokens: field required")
	}
	if len(req.Messages) == 0 {
		return nil, badRequest("messages: at least one message is required")
	}
	for i, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, badRequest("messages[" + itoa(i) + "].role must be user or assistant")
		}
	}
	req.Raw = body
	for _, v := range h.Values("anthropic-beta") {
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				req.Betas = append(req.Betas, b)
			}
		}
	}
	return &req, nil
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.anthropicError(w, err)
		return
	}
	req, err := parseMessages(body, r.Header)
	if err != nil {
		s.anthropicError(w, err)
		return
	}
	route, err := s.router.Resolve(req.Model)
	if err != nil {
		s.anthropicError(w, err)
		return
	}
	if !req.Stream {
		resp, _, err := s.router.Complete(r.Context(), route, req)
		if err != nil {
			s.anthropicError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	stream, _, err := s.router.Stream(r.Context(), route, req)
	if err != nil {
		s.anthropicError(w, err)
		return
	}
	defer stream.Close()
	s.relayAnthropic(w, stream)
}

// relayAnthropic writes a canonical stream as Messages API events. Events from
// an Anthropic upstream are relayed byte for byte.
func (s *Server) relayAnthropic(w http.ResponseWriter, stream provider.Stream) {
	sw := sse.NewWriter(w)
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			ae := toAPIError(err)
			b, _ := json.Marshal(llm.Event{Type: llm.EventError, Error: &llm.APIError{Type: ae.Type, Message: ae.Message}})
			_ = sw.Write(llm.EventError, b)
			return
		}
		data := ev.Raw
		if data == nil {
			data, _ = json.Marshal(ev)
		}
		if sw.Write(ev.Type, data) != nil {
			return
		}
		if ev.Type == llm.EventMessageStop {
			return
		}
	}
}
