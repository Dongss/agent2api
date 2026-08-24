package sse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/ir"
)

func TestWriterFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewWriter(rec)

	if w.Wrote() {
		t.Error("a fresh writer must not have committed the response yet")
	}
	if err := w.Event("message_start", map[string]any{"type": "message_start"}); err != nil {
		t.Fatal(err)
	}
	if !w.Wrote() {
		t.Error("Wrote must report the response as committed after a frame")
	}
	if err := w.Comment("keepalive"); err != nil {
		t.Fatal(err)
	}
	if err := w.Raw("[DONE]"); err != nil {
		t.Fatal(err)
	}

	want := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		": keepalive\n\n" +
		"data: [DONE]\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// Headers are written with the first frame, so a failure that happens before
// any output can still be reported with a status code.
func TestWriterLeavesResponseUncommitted(t *testing.T) {
	rec := httptest.NewRecorder()
	NewWriter(rec)
	if rec.Header().Get("Content-Type") != "" {
		t.Error("constructing a writer must not commit the response")
	}
}

func TestRelayDeliversAndStops(t *testing.T) {
	events := make(chan ir.Event, 3)
	events <- ir.TextEvent("a")
	events <- ir.Event{Type: ir.EventDone}
	close(events)

	var seen []ir.EventType
	err := Relay(context.Background(), events, 0,
		func(ev ir.Event) error { seen = append(seen, ev.Type); return nil },
		func() error { t.Error("ping fired with no idle time"); return nil })
	if err != nil {
		t.Fatalf("Relay = %v, want nil once the channel closed", err)
	}
	if len(seen) != 2 || seen[0] != ir.EventTextDelta || seen[1] != ir.EventDone {
		t.Errorf("delivered %v", seen)
	}
}

func TestRelayPingsWhileIdle(t *testing.T) {
	events := make(chan ir.Event)
	defer close(events)

	pinged := make(chan struct{}, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- Relay(ctx, events, time.Millisecond,
			func(ir.Event) error { return nil },
			func() error {
				select {
				case pinged <- struct{}{}:
				default:
				}
				return nil
			})
	}()

	select {
	case <-pinged:
	case <-time.After(2 * time.Second):
		t.Fatal("a silent backend never triggered a keepalive")
	}

	cancel()
	select {
	case err := <-done:
		if e := ir.AsError(err); e.Code != ir.CodeCanceled {
			t.Errorf("Relay = %v, want a canceled error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Relay did not return after its context was canceled")
	}
}

// An error from the frame writer means the client is gone; the relay must stop
// rather than keep formatting frames nobody will read.
func TestRelayStopsOnWriteError(t *testing.T) {
	events := make(chan ir.Event, 1)
	events <- ir.TextEvent("a")

	boom := ir.Errorf(ir.CodeInternal, "client gone")
	err := Relay(context.Background(), events, 0,
		func(ir.Event) error { return boom },
		func() error { return nil })
	if err != boom {
		t.Errorf("Relay = %v, want the write error", err)
	}
	if !strings.Contains(err.Error(), "client gone") {
		t.Errorf("error = %v", err)
	}
}

// The write deadline lives on the connection, not on the response, so a stream
// that installs one has to remove it: the next request on a reused keep-alive
// connection must not inherit a deadline that has already passed.
func TestWriteDeadlineDoesNotLeakToTheNextRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		out := NewWriter(w)
		out.WriteTimeout = 20 * time.Millisecond
		defer out.ClearDeadline()
		if err := out.Event("", map[string]string{"hello": "there"}); err != nil {
			t.Errorf("stream write: %v", err)
		}
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		// Long after the stream's deadline would have expired.
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("late but fine"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// One client, so the second request reuses the first one's connection.
	client := srv.Client()
	for _, path := range []string{"/stream", "/slow"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned nothing", path)
		}
	}
}
