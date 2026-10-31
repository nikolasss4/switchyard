package gateway

import (
	"context"
	"time"

	"github.com/nikolasss4/switchyard/internal/cache"
	"github.com/nikolasss4/switchyard/internal/llm"
	"github.com/nikolasss4/switchyard/internal/oai"
	"github.com/nikolasss4/switchyard/internal/router"
)

// Caches holds the optional response caches and the embedder used by the
// semantic cache.
type Caches struct {
	Exact    cache.Exact
	Vectors  cache.Vectors
	Embedder *router.Target
}

// pending is what a miss needs to fill the caches once the response is in.
type pending struct {
	exactKey string
	scope    string
	vector   []float32
	ttl      time.Duration
}

// lookup checks the exact cache, then the semantic cache, for the call's alias.
// Cache errors are logged and treated as misses.
func (g *Gateway) lookup(ctx context.Context, c *Call) (*llm.Response, string, *pending) {
	m := c.Route.Model
	if m == nil || c.Key == nil || (!m.Cache.Exact && !m.Cache.Semantic.Enabled) {
		return nil, "", nil
	}
	p := &pending{ttl: m.Cache.TTL}
	if m.Cache.Exact && g.Caches.Exact != nil {
		key, err := cache.Fingerprint(c.Key.TenantID, c.Route.Alias, c.Req)
		if err == nil {
			p.exactKey = key
			resp, ok, err := g.Caches.Exact.Get(ctx, key)
			if err != nil {
				g.Log.Warn("exact cache read failed", "err", err)
			} else if ok {
				return resp, "exact", nil
			}
		}
	}
	if m.Cache.Semantic.Enabled && g.Caches.Vectors != nil && g.Caches.Embedder != nil {
		text, scope, ok := cache.SemanticInput(c.Key.TenantID, c.Route.Alias, c.Req)
		if ok {
			ectx, cancel := context.WithTimeout(ctx, 2*time.Second)
			emb, err := g.Caches.Embedder.Provider.Embed(ectx, &oai.EmbeddingRequest{Model: g.Caches.Embedder.Model, Input: []string{text}})
			cancel()
			if err != nil || len(emb.Data) == 0 {
				g.Log.Warn("semantic cache embedding failed", "err", err)
			} else {
				p.scope, p.vector = scope, emb.Data[0].Embedding
				resp, sim, ok, err := g.Caches.Vectors.Nearest(ctx, scope, p.vector)
				if err != nil {
					g.Log.Warn("semantic cache read failed", "err", err)
				} else if ok && sim >= m.Cache.Semantic.Threshold {
					return resp, "semantic", nil
				}
			}
		}
	}
	if p.exactKey == "" && p.vector == nil {
		return nil, "", nil
	}
	return nil, "", p
}

// fill stores a fresh response in the caches, off the request path.
func (g *Gateway) fill(p *pending, resp *llm.Response) {
	if p == nil || resp == nil || resp.StopReason == "refusal" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if p.exactKey != "" {
			if err := g.Caches.Exact.Set(ctx, p.exactKey, resp, p.ttl); err != nil {
				g.Log.Warn("exact cache write failed", "err", err)
			}
		}
		if p.vector != nil {
			if err := g.Caches.Vectors.Put(ctx, p.scope, p.vector, resp, p.ttl); err != nil {
				g.Log.Warn("semantic cache write failed", "err", err)
			}
		}
	}()
}
