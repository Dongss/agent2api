package agentcli

import (
	"context"
	"log/slog"
	"time"

	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/runner"
)

// Parser turns one CLI's stdout into IR events. One parser handles one run.
type Parser interface {
	// Line consumes one line of stdout. Returning an error kills the process,
	// so unrecognized output should be ignored rather than rejected: CLIs add
	// events between releases.
	Line(b []byte) error
	// Finish emits the run's terminal event. runErr is whatever the runner
	// reported, nil on a clean exit.
	Finish(runErr error)
}

// Run describes one CLI invocation to stream.
type Run struct {
	Spec runner.Spec
	// Model is the namespaced model name, reported on the start event.
	Model string
	// LogArgs is argv with caller content already redacted; it is what gets
	// logged. Empty means log nothing.
	LogArgs []string
	// Cleanup releases per-request resources, e.g. the scratch directory. It
	// runs after the last event.
	Cleanup func()
}

// Stream spawns the process and returns the events it produces. The channel is
// closed after the terminal event, and every exit path runs Cleanup.
//
// This is the whole run loop an adapter needs: it emits the start event, feeds
// stdout to the parser, and lets the parser have the last word on how the run
// ended.
func Stream(ctx context.Context, log *slog.Logger, r Run, newParser func(emit func(ir.Event) bool) Parser) <-chan ir.Event {
	events := make(chan ir.Event, 64)
	go func() {
		defer close(events)
		if r.Cleanup != nil {
			defer r.Cleanup()
		}

		// emit reports whether the event was delivered; a false return means
		// the caller is gone and the run should wind down.
		emit := func(e ir.Event) bool {
			select {
			case events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !emit(ir.Event{Type: ir.EventStart, Model: r.Model}) {
			return
		}

		if log != nil && len(r.LogArgs) > 0 {
			log.Debug("running agent cli", "binary", r.Spec.Binary, "argv", r.LogArgs, "workdir", r.Spec.Dir)
		}

		started := time.Now()
		p := newParser(emit)
		_, runErr := runner.Run(ctx, r.Spec, p.Line)
		p.Finish(runErr)

		if log != nil {
			log.Debug("agent cli run finished",
				"model", r.Model,
				"duration", time.Since(started).Round(time.Millisecond),
				"error", runErr != nil)
		}
	}()
	return events
}
