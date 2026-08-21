package anthropic

import (
	"errors"
	"net/http"

	"github.com/Dongss/agent2api/internal/frontend/sse"
	"github.com/Dongss/agent2api/internal/ir"
)

// errAborted stops the relay after the failure has already been reported as an
// HTTP status, so nothing more is written to the response.
var errAborted = errors.New("anthropic: stream aborted before it started")

// streamMessage renders one adapter run as the canonical Messages API event
// sequence:
//
//	message_start → content_block_start → content_block_delta* →
//	content_block_stop → message_delta → message_stop
//
// with pings while the backend is quiet. Thinking, where the CLI exposes any,
// is its own block before the text block, as in the vendor API.
//
// message_start waits for the backend's first output: while nothing is sent the
// response is uncommitted, so a missing or logged-out CLI still fails as a 503
// rather than inside a 200 OK stream. A merely slow backend crosses the
// keepalive interval instead, and that first ping opens the message.
func (h *Handler) streamMessage(w http.ResponseWriter, r *http.Request, req *messagesRequest, events <-chan ir.Event) {
	s := &messageStream{h: h, out: sse.NewWriter(w), req: req, id: newID()}
	// The write deadline lives on the connection; a keep-alive connection reused
	// for the next request must not inherit this stream's.
	defer s.out.ClearDeadline()

	err := sse.Relay(r.Context(), events, h.Heartbeat,
		func(ev ir.Event) error {
			switch ev.Type {
			case ir.EventTextDelta:
				return s.delta("text", ev.Text)
			case ir.EventThinkingDelta:
				return s.delta("thinking", ev.Text)
			case ir.EventUsage:
				s.usage = ev.Usage
			case ir.EventDone:
				if ev.Usage != nil {
					s.usage = ev.Usage
				}
				return s.end(ev.StopReason)
			case ir.EventError:
				return s.fail(w, ev.Err)
			}
			return nil
		},
		func() error {
			// A ping before message_start is a protocol error the SDKs reject
			// outright, so opening the message is part of sending one.
			if err := s.start(); err != nil {
				return err
			}
			return s.out.Event("ping", pingEvent{Type: "ping"})
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
	case !s.finished:
		// The adapter closed its channel without a terminal event: the caller
		// must not read a truncated answer as a complete one.
		_ = s.fail(w, ir.IncompleteRun())
	}
}

// messageStream tracks where a stream is: whether message_start has gone out,
// and which content block is currently open.
type messageStream struct {
	h   *Handler
	out *sse.Writer
	req *messagesRequest
	id  string

	usage    *ir.Usage
	started  bool
	finished bool
	// blockKind is "text", "thinking", or "" when no block is open.
	blockKind string
	// blockIndex is the index of the open block, or of the next one to open.
	blockIndex int
	// sawText records whether a text block was opened, which every message
	// ends up with even if the backend only ever thought out loud.
	sawText bool
}

func (s *messageStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	// Input tokens are not known until the CLI reports them, so message_start
	// carries zeros and message_delta carries the real totals — which is the
	// order every Anthropic SDK accumulates in.
	return s.out.Event("message_start", messageStartEvent{
		Type: "message_start",
		Message: message{
			ID:      s.id,
			Type:    "message",
			Role:    "assistant",
			Model:   s.req.Model,
			Content: []block{},
			Usage:   usage{},
		},
	})
}

// delta emits one delta, opening (and, on a switch of kind, closing) the
// content block that holds it.
func (s *messageStream) delta(kind, text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.openBlock(kind); err != nil {
		return err
	}
	d := blockDelta{Type: kind + "_delta"}
	if kind == "thinking" {
		d.Thinking = text
	} else {
		d.Text = text
	}
	return s.out.Event("content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: s.blockIndex,
		Delta: d,
	})
}

// openBlock starts a content block of the given kind, closing whatever block
// is open if the kind changed.
func (s *messageStream) openBlock(kind string) error {
	if s.blockKind == kind {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	opening := textBlock("")
	if kind == "thinking" {
		opening = thinkingBlock("")
	}
	if err := s.out.Event("content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        s.blockIndex,
		ContentBlock: opening,
	}); err != nil {
		return err
	}
	s.blockKind = kind
	s.sawText = s.sawText || kind == "text"
	return nil
}

func (s *messageStream) closeBlock() error {
	if s.blockKind == "" {
		return nil
	}
	if err := s.out.Event("content_block_stop", contentBlockStopEvent{
		Type:  "content_block_stop",
		Index: s.blockIndex,
	}); err != nil {
		return err
	}
	s.blockKind = ""
	s.blockIndex++
	return nil
}

func (s *messageStream) end(stop ir.StopReason) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	if !s.sawText {
		// Every message ends with a text block, even when the backend answered
		// with nothing but thinking, so a client reading the last block always
		// finds the answer where it expects it. It carries no delta: there was
		// nothing to deliver.
		if err := s.openBlock("text"); err != nil {
			return err
		}
		if err := s.closeBlock(); err != nil {
			return err
		}
	}

	if stop == "" {
		stop = ir.StopEndTurn
	}
	ev := messageDeltaEvent{Type: "message_delta", Usage: toUsage(s.usage)}
	ev.Delta.StopReason = stopReason(stop)
	if err := s.out.Event("message_delta", ev); err != nil {
		return err
	}
	s.finished = true
	return s.out.Event("message_stop", messageStopEvent{Type: "message_stop"})
}

// fail reports a failed run. Before the first frame the response is still
// uncommitted and the caller gets a real status code; afterwards the dialect's
// own error event is the only channel left.
func (s *messageStream) fail(w http.ResponseWriter, cause *ir.Error) error {
	e := cause
	if e == nil {
		e = ir.Errorf(ir.CodeInternal, "the backend failed without reporting a reason")
	}
	if !s.out.Wrote() {
		WriteError(w, e)
		return errAborted
	}
	s.finished = true
	return s.out.Event("error", envelopeOf(e))
}
