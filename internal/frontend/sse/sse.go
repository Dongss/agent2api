// Package sse encodes Server-Sent Events and pumps adapter events into them.
//
// Both API dialects stream over SSE, so framing, flushing and keepalives live
// here; the frontends only decide what each frame contains.
package sse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	// DefaultHeartbeat is the keepalive cadence used when none is configured.
	DefaultHeartbeat = 15 * time.Second

	// DefaultWriteTimeout bounds one frame write, so a client that stops reading
	// cannot pin a goroutine and its CLI subprocess for the whole request
	// timeout: a failed write ends the stream, cancelling the request.
	DefaultWriteTimeout = 30 * time.Second
)

// Writer writes SSE frames to an HTTP response, flushing each one so the client
// sees it as it happens.
//
// Headers go out with the first frame, not at construction: while nothing has
// been sent the response is uncommitted, so a run that fails before producing
// output can still be reported as an HTTP status rather than as an error buried
// in a stream that already claimed 200 OK.
type Writer struct {
	// WriteTimeout bounds a single frame write; zero means
	// [DefaultWriteTimeout], negative disables the deadline.
	WriteTimeout time.Duration

	w     http.ResponseWriter
	rc    *http.ResponseController
	wrote bool
}

// NewWriter wraps an HTTP response.
func NewWriter(w http.ResponseWriter) *Writer {
	return &Writer{w: w, rc: http.NewResponseController(w)}
}

// Wrote reports whether any frame has been sent, i.e. whether the response is
// already committed to being an event stream.
func (s *Writer) Wrote() bool { return s.wrote }

// Event writes one named event carrying the JSON encoding of payload. An empty
// name writes a data-only frame, which is what the OpenAI dialect uses.
func (s *Writer) Event(name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sse: cannot encode %q event: %w", name, err)
	}
	return s.write(name, body)
}

// Raw writes a data frame verbatim, for sentinels like OpenAI's "[DONE]".
func (s *Writer) Raw(data string) error { return s.write("", []byte(data)) }

// Comment writes an SSE comment: ignored by every client, which makes it the
// cheapest possible keepalive.
func (s *Writer) Comment(text string) error {
	s.begin()
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	return s.flush()
}

// ClearDeadline removes the write deadline this writer installed. Callers must
// do this when the stream ends: the deadline lives on the connection, and a
// keep-alive connection reused for the next request must not inherit it.
func (s *Writer) ClearDeadline() {
	if !s.wrote {
		return
	}
	_ = s.rc.SetWriteDeadline(time.Time{})
}

func (s *Writer) begin() {
	if s.wrote {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// A buffering proxy in front of the gateway would defeat the point.
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.wrote = true
}

func (s *Writer) write(name string, data []byte) error {
	s.begin()
	var buf bytes.Buffer
	if name != "" {
		buf.WriteString("event: ")
		buf.WriteString(name)
		buf.WriteByte('\n')
	}
	// A data field cannot contain a newline; JSON encoding never emits one, but
	// splitting keeps the framing correct for any payload.
	for _, line := range bytes.Split(data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := s.w.Write(buf.Bytes()); err != nil {
		return err
	}
	return s.flush()
}

func (s *Writer) flush() error {
	s.deadline()
	if err := s.rc.Flush(); err != nil {
		// An unflushable writer would buffer the whole stream, which is worse
		// than failing: the caller asked for tokens as they arrive.
		return fmt.Errorf("sse: cannot flush the response: %w", err)
	}
	return nil
}

// deadline arms the write deadline for the next frame. Writers that do not
// support deadlines (an httptest recorder, a middleware that does not unwrap)
// simply do not get one; the write itself still reports any real problem.
func (s *Writer) deadline() {
	timeout := s.WriteTimeout
	if timeout == 0 {
		timeout = DefaultWriteTimeout
	}
	if timeout < 0 {
		return
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(timeout))
}
