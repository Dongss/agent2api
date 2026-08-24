package sse

import (
	"context"
	"time"

	"github.com/Dongss/agent2api/internal/ir"
)

// Relay forwards adapter events to on until the run ends, calling ping whenever
// the backend has produced nothing for interval.
//
// The idle timer restarts on every event, so a ping means the backend really
// has gone quiet.
//
// It returns nil once the adapter closes its channel, the error from on or ping,
// or a canceled error if ctx ended first. An early return does not drain the
// channel: the request context ending is what releases the adapter goroutine
// and its subprocess.
func Relay(ctx context.Context, events <-chan ir.Event, interval time.Duration, on func(ir.Event) error, ping func() error) error {
	var idle *time.Timer
	var quiet <-chan time.Time
	if interval > 0 {
		idle = time.NewTimer(interval)
		defer idle.Stop()
		quiet = idle.C
	}
	for {
		select {
		case <-ctx.Done():
			return &ir.Error{Code: ir.CodeCanceled, Message: "request canceled", Err: ctx.Err()}
		case <-quiet:
			if err := ping(); err != nil {
				return err
			}
			idle.Reset(interval)
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := on(ev); err != nil {
				return err
			}
			if idle != nil {
				if !idle.Stop() {
					// The timer already fired; drain it so the reset below is
					// the only pending tick.
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(interval)
			}
		}
	}
}
