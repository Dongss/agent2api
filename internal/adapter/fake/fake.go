// Package fake provides an in-memory adapter for tests: it exercises the
// gateway end to end without spawning a CLI.
package fake

import (
	"context"
	"strings"
	"time"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/ir"
)

// Adapter is a scripted [adapter.Adapter].
type Adapter struct {
	// AdapterID namespaces the models; defaults to "fake".
	AdapterID string
	// Variants are the model variants to advertise.
	Variants []string
	// Reply is returned as the assistant's text, split into a few deltas.
	Reply string
	// Thinking, when set, is emitted as reasoning deltas.
	Thinking string
	// Fail, when set, is emitted instead of a completion.
	Fail *ir.Error
	// ProbeErr is returned by Probe.
	ProbeErr error
	// FirstTokenDelay is slept after the start event and before any text, as a
	// backend that is thinking does.
	FirstTokenDelay time.Duration
	// Delay is slept after the text and before the terminal event, to exercise
	// cancellation and streaming keepalives.
	Delay time.Duration
	// Silent, when set, closes the event channel without a terminal event, as a
	// CLI that dies mid-run does.
	Silent bool
	// Usage, when set, is reported on the done event.
	Usage *ir.Usage

	// Requests records what the gateway asked for.
	Requests []ir.Request
}

// New builds a fake adapter serving one variant with a fixed reply.
func New(id, reply string, variants ...string) *Adapter {
	if len(variants) == 0 {
		variants = []string{"default-model"}
	}
	return &Adapter{AdapterID: id, Variants: variants, Reply: reply}
}

// ID implements adapter.Adapter.
func (a *Adapter) ID() string {
	if a.AdapterID == "" {
		return "fake"
	}
	return a.AdapterID
}

// Probe implements adapter.Adapter.
func (a *Adapter) Probe(ctx context.Context) (adapter.Health, error) {
	return adapter.Health{Binary: "/usr/bin/" + a.ID(), Version: "fake 1.0"}, a.ProbeErr
}

// Run implements adapter.Adapter.
func (a *Adapter) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	a.Requests = append(a.Requests, req)

	events := make(chan ir.Event, 16)
	go func() {
		defer close(events)
		emit := func(e ir.Event) bool {
			select {
			case events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !emit(ir.Event{Type: ir.EventStart, Model: req.Model}) {
			return
		}
		if a.FirstTokenDelay > 0 {
			select {
			case <-time.After(a.FirstTokenDelay):
			case <-ctx.Done():
				return
			}
		}
		if a.Thinking != "" && !emit(ir.ThinkingEvent(a.Thinking)) {
			return
		}
		for _, word := range strings.SplitAfter(a.Reply, " ") {
			if word == "" {
				continue
			}
			if !emit(ir.TextEvent(word)) {
				return
			}
		}
		if a.Delay > 0 {
			select {
			case <-time.After(a.Delay):
			case <-ctx.Done():
				return
			}
		}
		if a.Silent {
			return
		}
		if a.Fail != nil {
			emit(ir.ErrorEvent(a.Fail))
			return
		}
		emit(ir.Event{
			Type:       ir.EventDone,
			Usage:      a.Usage,
			StopReason: ir.StopEndTurn,
			Model:      req.Model,
		})
	}()
	return events, nil
}
