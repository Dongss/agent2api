package ir

import (
	"context"
	"strings"
)

// Completion is a fully collected response, as a non-streaming frontend needs
// it. Adapters always stream internally; "non-streaming" is just this.
type Completion struct {
	Text       string
	Thinking   string
	Usage      *Usage
	StopReason StopReason
	Model      string
}

// IncompleteRun is the error for a run that ended without a terminal event: a
// backend that stopped mid-answer. It must never be reported as a short answer,
// because the caller has no way to tell the difference.
func IncompleteRun() *Error {
	return &Error{
		Code:    CodeUpstreamError,
		Message: "the backend ended the run without completing it",
	}
}

// Collect drains an adapter's event channel until the run ends.
//
// The channel is consumed to completion even after an error, so the adapter's
// goroutine always finishes and its subprocess resources are released.
func Collect(ctx context.Context, events <-chan Event) (Completion, error) {
	var (
		out      Completion
		text     strings.Builder
		thinking strings.Builder
		failure  *Error
		finished bool
	)
	for {
		select {
		case <-ctx.Done():
			return out, &Error{Code: CodeCanceled, Message: "request canceled", Err: ctx.Err()}
		case ev, ok := <-events:
			if !ok {
				out.Text = text.String()
				out.Thinking = thinking.String()
				switch {
				case failure != nil:
					return out, failure
				case !finished:
					return out, IncompleteRun()
				}
				return out, nil
			}
			switch ev.Type {
			case EventTextDelta:
				text.WriteString(ev.Text)
			case EventThinkingDelta:
				thinking.WriteString(ev.Text)
			case EventStart:
				if ev.Model != "" {
					out.Model = ev.Model
				}
			case EventUsage:
				out.Usage = ev.Usage
			case EventDone:
				finished = true
				if ev.Usage != nil {
					out.Usage = ev.Usage
				}
				if ev.StopReason != "" {
					out.StopReason = ev.StopReason
				}
				if ev.Model != "" {
					out.Model = ev.Model
				}
			case EventError:
				finished = true
				if failure == nil {
					failure = ev.Err
				}
			}
		}
	}
}
