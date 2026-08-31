// Package gate bounds how many agent CLI processes an adapter may have running
// at once.
//
// Every request is a whole subprocess, so an unbounded gateway is a fork bomb
// waiting for a busy client. Requests over the limit wait for a slot and are
// rejected with 429 if none frees up in time.
package gate

import (
	"context"
	"fmt"
	"time"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/ir"
)

// Wrap returns an adapter that admits at most limit concurrent runs, waiting up
// to queueTimeout for a slot. A limit below 1 disables gating.
func Wrap(a adapter.Adapter, limit int, queueTimeout time.Duration) adapter.Adapter {
	if limit < 1 {
		return a
	}
	return &gated{Adapter: a, slots: make(chan struct{}, limit), queueTimeout: queueTimeout}
}

type gated struct {
	adapter.Adapter
	slots        chan struct{}
	queueTimeout time.Duration
}

// EnforcesSchema forwards the capability query to the wrapped adapter.
//
// Embedding an *interface* promotes only the methods that interface declares,
// so an optional one like [adapter.SchemaEnforcer] does not survive the wrap on
// its own: the type assertion sees *gated, which does not have the method, and
// a backend that can hold a schema silently reports that it cannot. Any future
// optional interface needs the same forwarding.
func (g *gated) EnforcesSchema(ctx context.Context) bool {
	e, ok := g.Adapter.(adapter.SchemaEnforcer)
	return ok && e.EnforcesSchema(ctx)
}

// Run holds a slot for the whole life of the run, releasing it once the
// underlying adapter closes its event channel or the caller goes away.
func (g *gated) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	if err := g.acquire(ctx); err != nil {
		return nil, err
	}

	events, err := g.Adapter.Run(ctx, req)
	if err != nil {
		g.release()
		return nil, err
	}

	out := make(chan ir.Event, cap(events))
	go func() {
		defer close(out)
		defer g.release()
		for ev := range events {
			select {
			case out <- ev:
			case <-ctx.Done():
				// Drain so the adapter's goroutine can finish and clean up.
				for range events {
				}
				return
			}
		}
	}()
	return out, nil
}

func (g *gated) acquire(ctx context.Context) error {
	// Fast path: a slot is free right now.
	select {
	case g.slots <- struct{}{}:
		return nil
	default:
	}

	if g.queueTimeout <= 0 {
		return g.busy()
	}
	timer := time.NewTimer(g.queueTimeout)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return g.busy()
	case <-ctx.Done():
		return &ir.Error{Code: ir.CodeCanceled, Message: "request canceled while queued", Err: ctx.Err()}
	}
}

func (g *gated) busy() error {
	id := g.Adapter.ID()
	return &ir.Error{
		Code: ir.CodeOverloaded,
		Message: fmt.Sprintf("all %d %s slots are busy; retry shortly or raise server.max_concurrency",
			cap(g.slots), id),
		// A slot frees up as soon as a run finishes, so retrying soon is worth
		// it — unlike an upstream rate limit.
		RetryAfter: time.Second,
	}
}

func (g *gated) release() { <-g.slots }
