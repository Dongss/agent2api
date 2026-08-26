package responses

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Dongss/agent2api/internal/frontend/sse"
	"github.com/Dongss/agent2api/internal/ir"
)

// streamResponse renders one adapter run as the Responses event sequence:
//
//	response.created → response.in_progress
//	  output_item.added (reasoning) → reasoning_summary_part.added →
//	  reasoning_summary_text.delta* → …_text.done → …_part.done → output_item.done
//	  output_item.added (message) → content_part.added →
//	  output_text.delta* → output_text.done → content_part.done → output_item.done
//	response.completed
//
// with keepalive comments while the backend is quiet.
//
// response.created waits for the backend's first output: while nothing is sent
// the response is uncommitted, so a missing or logged-out CLI still fails as a
// 503 rather than inside a 200 OK stream. A merely slow backend crosses the
// keepalive interval instead, and the comment that carries is not an event, so
// it can go out before the stream has formally opened.
func (h *Handler) streamResponse(w http.ResponseWriter, r *http.Request, req *responsesRequest, events <-chan ir.Event) {
	s := &responseStream{
		h:     h,
		out:   sse.NewWriter(w),
		req:   req,
		shell: req.shell(newID("resp"), h.now().Unix()),
	}
	// The write deadline lives on the connection; a keep-alive connection reused
	// for the next request must not inherit this stream's.
	defer s.out.ClearDeadline()

	err := sse.Relay(r.Context(), events, h.Heartbeat,
		func(ev ir.Event) error {
			switch ev.Type {
			case ir.EventTextDelta:
				return s.delta(kindText, ev.Text)
			case ir.EventThinkingDelta:
				return s.delta(kindReasoning, ev.Text)
			case ir.EventUsage:
				s.usage = ev.Usage
			case ir.EventDone:
				if ev.Usage != nil {
					s.usage = ev.Usage
				}
				return s.end()
			case ir.EventError:
				return s.fail(w, ev.Err)
			}
			return nil
		},
		func() error {
			// A comment, not an event: it needs no sequence number and cannot
			// arrive out of order, so it is safe before response.created.
			return s.out.Comment("keepalive")
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

// itemKind is which of the two output items a delta belongs to. They are
// ordered: reasoning, when there is any, always precedes the message.
type itemKind int

const (
	kindNone itemKind = iota
	kindReasoning
	kindText
)

// responseStream tracks where a stream is: whether it has opened, which output
// item is currently open, and what has accumulated in it. The accumulated text
// is needed twice over — the `…done` events repeat the finished string, and
// response.completed carries the whole output again.
type responseStream struct {
	h     *Handler
	out   *sse.Writer
	req   *responsesRequest
	shell response

	usage    *ir.Usage
	seq      int
	started  bool
	finished bool

	// open is the item being written, or kindNone between items.
	open itemKind
	// outputIndex is the index of the open item, or of the next one to open.
	outputIndex int
	// items holds the finished items, in order, for response.completed.
	items []item

	itemID  string
	buf     strings.Builder
	sawText bool
}

// next returns the sequence number for the next event. Every event carries one
// and clients rely on it being gap-free and monotonic.
func (s *responseStream) next() int {
	n := s.seq
	s.seq++
	return n
}

func (s *responseStream) start() error {
	if s.started {
		return nil
	}
	s.started = true

	created := s.shell
	created.Status = "in_progress"
	if err := s.out.Event("response.created", createdEvent{
		Type: "response.created", Sequence: s.next(), Response: created,
	}); err != nil {
		return err
	}
	return s.out.Event("response.in_progress", createdEvent{
		Type: "response.in_progress", Sequence: s.next(), Response: created,
	})
}

// delta emits one delta, opening (and, on a switch of kind, closing) the output
// item that holds it.
func (s *responseStream) delta(kind itemKind, text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.openItem(kind); err != nil {
		return err
	}
	s.buf.WriteString(text)

	if kind == kindReasoning {
		return s.out.Event("response.reasoning_summary_text.delta", summaryDeltaEvent{
			Type: "response.reasoning_summary_text.delta", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, SummaryIndex: 0, Delta: text,
		})
	}
	return s.out.Event("response.output_text.delta", textDeltaEvent{
		Type: "response.output_text.delta", Sequence: s.next(),
		ItemID: s.itemID, OutputIndex: s.outputIndex, ContentIndex: 0, Delta: text,
	})
}

// openItem starts an output item of the given kind, closing whatever item is
// open if the kind changed.
//
// A backend that goes back to thinking after speaking would open a second
// reasoning item after the message. That ordering is unusual but not wrong, and
// reproducing what the CLI did beats reordering it behind the caller's back.
func (s *responseStream) openItem(kind itemKind) error {
	if s.open == kind {
		return nil
	}
	if err := s.closeItem(); err != nil {
		return err
	}
	s.itemID = newID(map[itemKind]string{kindReasoning: "rs", kindText: "msg"}[kind])
	s.buf.Reset()

	// The added item is empty: its content arrives as deltas, and the matching
	// output_item.done repeats it filled in.
	opening := item{Type: "reasoning", ID: s.itemID, Summary: []summaryPart{}}
	if kind == kindText {
		opening = item{
			Type: "message", ID: s.itemID, Status: "in_progress", Role: "assistant",
			Content: []outputPart{},
		}
	}
	if err := s.out.Event("response.output_item.added", outputItemEvent{
		Type: "response.output_item.added", Sequence: s.next(),
		OutputIndex: s.outputIndex, Item: opening,
	}); err != nil {
		return err
	}

	if kind == kindReasoning {
		if err := s.out.Event("response.reasoning_summary_part.added", summaryPartEvent{
			Type: "response.reasoning_summary_part.added", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, SummaryIndex: 0,
			Part: summaryPart{Type: "summary_text", Text: ""},
		}); err != nil {
			return err
		}
	} else {
		if err := s.out.Event("response.content_part.added", contentPartEvent{
			Type: "response.content_part.added", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, ContentIndex: 0,
			Part: outputPart{Type: "output_text", Text: "", Annotations: []any{}},
		}); err != nil {
			return err
		}
	}

	s.open = kind
	s.sawText = s.sawText || kind == kindText
	return nil
}

func (s *responseStream) closeItem() error {
	if s.open == kindNone {
		return nil
	}
	text := s.buf.String()

	if s.open == kindReasoning {
		if err := s.out.Event("response.reasoning_summary_text.done", summaryDoneEvent{
			Type: "response.reasoning_summary_text.done", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, SummaryIndex: 0, Text: text,
		}); err != nil {
			return err
		}
		if err := s.out.Event("response.reasoning_summary_part.done", summaryPartEvent{
			Type: "response.reasoning_summary_part.done", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, SummaryIndex: 0,
			Part: summaryPart{Type: "summary_text", Text: text},
		}); err != nil {
			return err
		}
	} else {
		if err := s.out.Event("response.output_text.done", textDoneEvent{
			Type: "response.output_text.done", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, ContentIndex: 0, Text: text,
		}); err != nil {
			return err
		}
		if err := s.out.Event("response.content_part.done", contentPartEvent{
			Type: "response.content_part.done", Sequence: s.next(),
			ItemID: s.itemID, OutputIndex: s.outputIndex, ContentIndex: 0,
			Part: outputPart{Type: "output_text", Text: text, Annotations: []any{}},
		}); err != nil {
			return err
		}
	}

	done := messageItem(s.itemID, text)
	if s.open == kindReasoning {
		done = reasoningItem(s.itemID, text)
	}
	if err := s.out.Event("response.output_item.done", outputItemEvent{
		Type: "response.output_item.done", Sequence: s.next(),
		OutputIndex: s.outputIndex, Item: done,
	}); err != nil {
		return err
	}

	s.items = append(s.items, done)
	s.open = kindNone
	s.outputIndex++
	s.buf.Reset()
	return nil
}

func (s *responseStream) end() error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.closeItem(); err != nil {
		return err
	}
	if !s.sawText {
		// Every response ends with a message item, even when the backend
		// answered with nothing but reasoning, so a client reading the last
		// item always finds the answer where it expects it. It carries no
		// delta: there was nothing to deliver.
		if err := s.openItem(kindText); err != nil {
			return err
		}
		if err := s.closeItem(); err != nil {
			return err
		}
	}

	final := s.shell
	final.Status = "completed"
	final.Output = s.items
	final.Usage = toUsage(s.usage)
	s.finished = true
	return s.out.Event("response.completed", createdEvent{
		Type: "response.completed", Sequence: s.next(), Response: final,
	})
}

// fail reports a failed run. Before the first frame the response is still
// uncommitted and the caller gets a real status code; afterwards the dialect's
// own failure event is the only channel left.
//
// response.failed carries the response object so a client that tracked it by id
// can close it out; the standalone error event carries the reason in the shape
// the SDKs already parse. Both go out, in that order.
func (s *responseStream) fail(w http.ResponseWriter, cause *ir.Error) error {
	e := cause
	if e == nil {
		e = ir.Errorf(ir.CodeInternal, "the backend failed without reporting a reason")
	}
	if !s.out.Wrote() {
		WriteError(w, e)
		return errAborted
	}

	body := errorBodyOf(e)
	final := s.shell
	final.Status = "failed"
	final.Output = s.items
	final.Usage = toUsage(s.usage)
	final.Error = &body

	s.finished = true
	if err := s.out.Event("response.failed", createdEvent{
		Type: "response.failed", Sequence: s.next(), Response: final,
	}); err != nil {
		return err
	}
	return s.out.Event("error", errorEvent{
		Type: "error", Sequence: s.next(), errBody: body,
	})
}
