// Package adapter defines the contract every agent CLI backend implements, and
// the registry that turns a config block into a live adapter.
//
// Adapters are pure translators: they build argv and parse CLI output into
// [ir.Event]s. Process lifecycle belongs to internal/runner, and no adapter
// holds state across requests.
package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/ir"
)

// SchemaEnforcer is implemented by adapters whose CLI can hold an answer to a
// JSON Schema.
//
// It is an optional interface rather than a method on [Adapter] because the
// answer is a property of the installed binary, not of the adapter: the same
// adapter reports false against a CLI too old for the flag. Frontends ask
// before accepting a caller's schema so the refusal can name the backend that
// cannot do it, instead of reading as a gateway-wide limitation.
type SchemaEnforcer interface {
	EnforcesSchema(ctx context.Context) bool
}

// Health is the outcome of probing a backend, as reported by `agent2api doctor`.
type Health struct {
	// Binary is the resolved absolute path of the CLI, when it was found.
	Binary string
	// Version is whatever the CLI reports for --version.
	Version string
	// Account describes the authenticated identity, when the CLI exposes one.
	Account string
	// Notes carry non-fatal remarks, e.g. flags the installed version lacks.
	Notes []string
}

// Adapter turns one agent CLI into an inference backend.
//
// An adapter advertises no model list. A request carries the model name the
// caller asked for in [ir.Request.Variant]; the adapter passes it to the CLI,
// which is the only authority on which names it accepts. An empty variant means
// "whatever model the CLI would pick on its own".
type Adapter interface {
	// ID is the adapter id used to namespace models, e.g. "claude-code".
	ID() string
	// Probe checks that the CLI is installed, runnable and authenticated.
	Probe(ctx context.Context) (Health, error)
	// Run executes one request. It always streams: non-streaming callers just
	// collect events until the terminal one. The returned channel is closed
	// after an [ir.EventDone] or [ir.EventError].
	Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error)
}

// Options carries everything an adapter needs from the global config, with
// inheritance already resolved.
type Options struct {
	ID     string
	Config config.Adapter
	// ScratchRoot is the parent directory for per-request workdirs; empty means
	// the OS temp directory.
	ScratchRoot string
	// RequestTimeout caps a whole run; IdleTimeout caps silence within one.
	RequestTimeout time.Duration
	IdleTimeout    time.Duration
	// Logger is the gateway logger, already scoped to this adapter.
	Logger *slog.Logger
}

// Builder constructs an adapter from its resolved configuration.
type Builder func(Options) (Adapter, error)

var (
	mu       sync.RWMutex
	builders = map[string]Builder{}
)

// Register makes an adapter implementation available to the gateway. It is
// called from the implementation package's init, so linking the package in is
// what enables the backend.
func Register(id string, b Builder) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := builders[id]; dup {
		panic("adapter: duplicate registration of " + id)
	}
	builders[id] = b
}

// Registered lists the adapter ids compiled into this build.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	ids := make([]string, 0, len(builders))
	for id := range builders {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// IsRegistered reports whether this build can serve the given adapter id.
func IsRegistered(id string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := builders[id]
	return ok
}

// Build constructs the adapter registered under id.
func Build(id string, opts Options) (Adapter, error) {
	mu.RLock()
	b, ok := builders[id]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("adapter %q is not implemented in this build (available: %v)", id, Registered())
	}
	return b(opts)
}
