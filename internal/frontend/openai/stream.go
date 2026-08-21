package openai

import (
	"errors"
	"net/http"

	"github.com/Dongss/agent2api/internal/frontend/sse"
	"github.com/Dongss/agent2api/internal/ir"
)

// errAborted stops the relay after the failure has already been reported as an
// HTTP status, so nothing more is written to the response.
var errAborted = errors.New("openai: stream aborted before it started")

// streamCompletion renders one adapter run as an OpenAI SSE stream:
//
//	data: {chat.completion.chunk with delta.role}
//	data: {chat.completion.chunk with delta.content}   (repeated)
//	data: {chat.completion.chunk with finish_reason}
//	data: {chat.completion.chunk with usage}           (stream_options only)
//	data: [DONE]
//
// The first frame waits for the backend's first output: while nothing is sent
// the response is uncommitted, so a missing or logged-out CLI still fails as a
// 503 rather than inside a 200 OK stream. A merely slow backend crosses the
// keepalive interval instead, and that first comment opens the stream.
func (h *Handler) streamCompletion(w http.ResponseWriter, r *http.Request, req *chatRequest, events <-chan ir.Event) {
	var (
		out      = sse.NewWriter(w)
		id       = newID()
		created  = h.now().Unix()
		usageOut *ir.Usage
		opened   bool
		finished bool
	)

	frame := func(choices []chunkChoice, u *usage) chatChunk {
		return chatChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: choices,
			Usage:   u,
		}
	}

	// delta sends one content frame, opening the choice with a role frame first,
	// exactly as the OpenAI API does.
	delta := func(d chunkDelta, finish *string) error {
		if !opened {
			opened = true
			if err := out.Event("", frame([]chunkChoice{{Delta: chunkDelta{Role: "assistant"}}}, nil)); err != nil {
				return err
			}
		}
		return out.Event("", frame([]chunkChoice{{Delta: d, FinishReason: finish}}, nil))
	}

	end := func(stop ir.StopReason) error {
		reason := finishReason(stop)
		if err := delta(chunkDelta{}, &reason); err != nil {
			return err
		}
		if u := toUsage(usageOut); req.includeUsage() && u != nil {
			if err := out.Event("", frame([]chunkChoice{}, u)); err != nil {
				return err
			}
		}
		finished = true
		return out.Raw("[DONE]")
	}

	// The write deadline lives on the connection; a keep-alive connection reused
	// for the next request must not inherit this stream's.
	defer out.ClearDeadline()

	err := sse.Relay(r.Context(), events, h.Heartbeat,
		func(ev ir.Event) error {
			switch ev.Type {
			case ir.EventTextDelta:
				return delta(chunkDelta{Content: ev.Text}, nil)
			case ir.EventThinkingDelta:
				return delta(chunkDelta{ReasoningContent: ev.Text}, nil)
			case ir.EventUsage:
				usageOut = ev.Usage
			case ir.EventDone:
				if ev.Usage != nil {
					usageOut = ev.Usage
				}
				return end(ev.StopReason)
			case ir.EventError:
				return h.streamFailure(w, out, ev.Err, &finished)
			}
			return nil
		},
		func() error {
			// A comment is a legal SSE frame that no client parses as data, so
			// it keeps the connection warm without committing to any chunk
			// beyond the 200 OK itself.
			return out.Comment("keepalive")
		})

	switch {
	case errors.Is(err, errAborted):
		return
	case err != nil:
		if e := ir.AsError(err); e.Code == ir.CodeCanceled {
			h.Log.Debug("client disconnected mid-stream", "model", req.Model)
			return
		}
		h.Log.Warn("cannot write the response stream", "model", req.Model, "error", err)
		return
	case !finished:
		// The adapter closed its channel without a terminal event. Something is
		// wrong with the backend, and the caller must not read a truncated
		// answer as a complete one.
		_ = h.streamFailure(w, out, ir.IncompleteRun(), &finished)
	}
}

// streamFailure reports a failed run. Before the first frame the response is
// still uncommitted and the caller gets a real status code; afterwards the only
// channel left is an error frame inside the stream.
func (h *Handler) streamFailure(w http.ResponseWriter, out *sse.Writer, cause *ir.Error, finished *bool) error {
	e := cause
	if e == nil {
		e = ir.Errorf(ir.CodeInternal, "the backend failed without reporting a reason")
	}
	if !out.Wrote() {
		WriteError(w, e)
		return errAborted
	}
	if err := out.Event("", streamError{Error: errorBodyOf(e)}); err != nil {
		return err
	}
	*finished = true
	return out.Raw("[DONE]")
}
