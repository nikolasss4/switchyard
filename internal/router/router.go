// Package router resolves a requested model to a chain of provider targets and
// executes requests along that chain.
package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/nikolasss4/switchyard/internal/config"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/provider"
)

// Target is a resolved provider and upstream model.
type Target struct {
	Provider provider.Provider
	Model    string
}

// Route is the resolved chain for one request.
type Route struct {
	Alias   string
	Targets []Target
	Model   *config.Model
}

// ErrUnknownModel is returned for models that are neither an alias nor provider/model.
var ErrUnknownModel = fmt.Errorf("unknown model")

// Router resolves models and runs requests.
type Router struct {
	providers map[string]provider.Provider
	models    map[string]*config.Model
}

// New returns a router over the configured aliases.
func New(cfg *config.Config, providers map[string]provider.Provider) *Router {
	r := &Router{providers: providers, models: map[string]*config.Model{}}
	for i := range cfg.Models {
		r.models[cfg.Models[i].Alias] = &cfg.Models[i]
	}
	return r
}

// Resolve maps a model name to a route. Names are aliases from the config, or
// "provider/model" to address one upstream model directly.
func (r *Router) Resolve(model string) (*Route, error) {
	if m, ok := r.models[model]; ok {
		route := &Route{Alias: model, Model: m}
		for _, t := range m.Targets {
			route.Targets = append(route.Targets, Target{Provider: r.providers[t.Provider], Model: t.Model})
		}
		return route, nil
	}
	if name, upstream, ok := strings.Cut(model, "/"); ok {
		if p, found := r.providers[name]; found && upstream != "" {
			return &Route{Alias: model, Targets: []Target{{Provider: p, Model: upstream}}, Model: &config.Model{Alias: model}}, nil
		}
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownModel, model)
}

// Complete sends a non-streaming request to the route's first target.
func (r *Router) Complete(ctx context.Context, route *Route, req *llm.Request) (*llm.Response, *Target, error) {
	t := route.Targets[0]
	up := *req
	up.Model = t.Model
	resp, err := t.Provider.Complete(ctx, &up)
	return resp, &t, err
}

// Stream opens a stream on the route's first target.
func (r *Router) Stream(ctx context.Context, route *Route, req *llm.Request) (provider.Stream, *Target, error) {
	t := route.Targets[0]
	up := *req
	up.Model = t.Model
	s, err := t.Provider.Stream(ctx, &up)
	return s, &t, err
}

// Aliases lists the configured aliases.
func (r *Router) Aliases() []string {
	out := make([]string, 0, len(r.models))
	for a := range r.models {
		out = append(out, a)
	}
	return out
}
