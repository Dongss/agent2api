// Package router resolves the model name in a request to the adapter that
// serves it.
//
// Names are namespaced "<adapter>" or "<adapter>:<model>". A bare adapter id
// means the CLI's own default model; anything after the colon goes to the CLI
// verbatim, since it is the authority on what it accepts and agent2api keeps no
// list to go stale. A name naming no served adapter is a 404.
package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/ir"
)

// Resolution is a routed model.
type Resolution struct {
	// Model is the canonical name the backend was reached by, e.g.
	// "claude-code:opus" or bare "claude-code".
	Model string
	// Variant is the model name to hand the CLI, empty for its own default.
	Variant string
	Adapter adapter.Adapter
}

// Entry is one row of GET /v1/models.
type Entry struct {
	ID      string
	Adapter string
}

// Router maps model names to adapters. It is immutable after construction and
// safe for concurrent use.
type Router struct {
	adapters map[string]adapter.Adapter
	ids      []string
}

// New builds a router over the given adapters.
func New(adapters []adapter.Adapter) (*Router, error) {
	r := &Router{adapters: make(map[string]adapter.Adapter, len(adapters))}
	for _, a := range adapters {
		id := a.ID()
		if _, dup := r.adapters[id]; dup {
			return nil, fmt.Errorf("router: adapter %q registered twice", id)
		}
		r.adapters[id] = a
		r.ids = append(r.ids, id)
	}
	sort.Strings(r.ids)
	return r, nil
}

// Resolve maps a requested model name to its adapter.
func (r *Router) Resolve(name string) (Resolution, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Resolution{}, ir.InvalidRequest("model", "a model name is required")
	}
	// Everything after the colon is the CLI's business, not ours.
	id, variant, _ := strings.Cut(name, ":")
	if a, ok := r.adapters[strings.TrimSpace(id)]; ok {
		return Resolution{Model: name, Variant: strings.TrimSpace(variant), Adapter: a}, nil
	}
	if len(r.ids) == 0 {
		return Resolution{}, &ir.Error{
			Code:    ir.CodeModelNotFound,
			Param:   "model",
			Message: "no backends are being served; run `agent2api doctor` to see why",
		}
	}
	return Resolution{}, &ir.Error{
		Code:  ir.CodeModelNotFound,
		Param: "model",
		Message: fmt.Sprintf(
			"model %q is not available; ask for one of %s, or append a model name the CLI knows, as in %q",
			name, strings.Join(r.ids, ", "), r.ids[0]+":<model>"),
	}
}

// Entries lists what GET /v1/models advertises: one row per backend, named by
// its adapter id. Callers may append ":<model>" to any of them.
func (r *Router) Entries() []Entry {
	out := make([]Entry, 0, len(r.ids))
	for _, id := range r.ids {
		out = append(out, Entry{ID: id, Adapter: id})
	}
	return out
}

// ModelIDs lists advertised model names in a stable order.
func (r *Router) ModelIDs() []string { return append([]string(nil), r.ids...) }

// Adapters returns the routed adapters, ordered by id.
func (r *Router) Adapters() []adapter.Adapter {
	out := make([]adapter.Adapter, 0, len(r.ids))
	for _, id := range r.ids {
		out = append(out, r.adapters[id])
	}
	return out
}
