package router

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/provider"
)

// fake is a scripted provider. Each call pops the next behaviour; when the
// script runs out the last one repeats.
type fake struct {
	name   string
	mu     sync.Mutex
	script []behaviour
	calls  atomic.Int64
}

type behaviour struct {
	delay     time.Duration
	err       error // returned before any event
	midStream error // returned after the first content event
}

func ok() behaviour { return behaviour{} }
func fail(status int) behaviour {
	return behaviour{err: &provider.Error{Provider: "f", Status: status}}
}
func slow(d time.Duration) behaviour { return behaviour{delay: d} }

func (f *fake) Name() string { return f.name }
func (f *fake) Kind() string { return "fake" }

func (f *fake) next() behaviour {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	return b
}

func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fake) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	b := f.next()
	if err := wait(ctx, b.delay); err != nil {
		return nil, err
	}
	if b.err != nil {
		return nil, b.err
	}
	return &llm.Response{ID: f.name, Model: req.Model, Content: []llm.Block{{Type: "text", Text: f.name}}}, nil
}

func (f *fake) Stream(ctx context.Context, req *llm.Request) (provider.Stream, error) {
	b := f.next()
	if err := wait(ctx, b.delay); err != nil {
		return nil, err
	}
	evs := []*llm.Event{{Type: llm.EventMessageStart, Message: &llm.Response{ID: f.name, Model: req.Model}}}
	if b.err != nil {
		// The upstream accepted the request, then failed before any content.
		return &fakeStream{events: evs, err: b.err}, nil
	}
	evs = append(evs,
		&llm.Event{Type: llm.EventContentBlockStart, Index: llm.Idx(0), ContentBlock: &llm.Block{Type: "text"}},
		&llm.Event{Type: llm.EventContentBlockDelta, Index: llm.Idx(0), Delta: &llm.Delta{Type: "text_delta", Text: f.name}})
	if b.midStream != nil {
		return &fakeStream{events: evs, err: b.midStream}, nil
	}
	evs = append(evs, &llm.Event{Type: llm.EventContentBlockStop, Index: llm.Idx(0)}, &llm.Event{Type: llm.EventMessageStop})
	return &fakeStream{events: evs, err: io.EOF}, nil
}

func (f *fake) Embed(context.Context, *oai.EmbeddingRequest) (*oai.EmbeddingResponse, error) {
	return &oai.EmbeddingResponse{}, nil
}

type fakeStream struct {
	events []*llm.Event
	err    error
	closed atomic.Bool
}

func (s *fakeStream) Next() (*llm.Event, error) {
	if len(s.events) == 0 {
		return nil, s.err
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *fakeStream) Close() error { s.closed.Store(true); return nil }
