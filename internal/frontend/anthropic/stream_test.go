package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

// event is one decoded SSE frame of an Anthropic stream.
type event struct {
	name string
	data map[string]any
}

func events(t *testing.T, body string) []event {
	t.Helper()
	var out []event
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var ev event
		var data []string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			default:
				t.Fatalf("unexpected SSE line %q", line)
			}
		}
		if ev.name == "" {
			t.Fatalf("every Anthropic frame is a named event, got %q", block)
		}
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev.data); err != nil {
			t.Fatalf("frame data is not JSON: %v\n%s", err, block)
		}
		// The event name and the payload's own type must agree; the SDKs read
		// one or the other.
		if got, _ := ev.data["type"].(string); got != ev.name {
			t.Errorf("event %q carries type %q", ev.name, got)
		}
		out = append(out, ev)
	}
	return out
}

func names(evs []event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.name)
	}
	return out
}

func TestStreamingMessage(t *testing.T) {
	backend := fake.New("fakecli", "Hello there.", "small")
	backend.Thinking = "hmm"
	backend.Usage = &ir.Usage{InputTokens: 11, OutputTokens: 3}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !backend.Requests[0].Stream {
		t.Error("a streaming request must tell the adapter it is streaming")
	}

	evs := events(t, rec.Body.String())
	got := names(evs)

	// The canonical sequence: thinking is its own block, closed before the text
	// block opens, and the message is framed by start/stop.
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start",
	}
	for i, name := range want {
		if i >= len(got) || got[i] != name {
			t.Fatalf("event sequence = %v, want it to start with %v", got, want)
		}
	}
	if tail := got[len(got)-2:]; tail[0] != "message_delta" || tail[1] != "message_stop" {
		t.Errorf("event sequence = %v, want it to end with message_delta, message_stop", got)
	}

	var text, thinking string
	blockIndex := map[float64]string{}
	for _, ev := range evs {
		switch ev.name {
		case "message_start":
			msg, _ := ev.data["message"].(map[string]any)
			if msg["role"] != "assistant" || msg["type"] != "message" || msg["model"] != "fakecli:small" {
				t.Errorf("message_start = %+v", msg)
			}
		case "content_block_start":
			block, _ := ev.data["content_block"].(map[string]any)
			kind, _ := block["type"].(string)
			blockIndex[ev.data["index"].(float64)] = kind
			// The SDKs build a typed block from this event, so the fields it
			// requires must be present even while empty.
			switch kind {
			case "text":
				if _, ok := block["text"]; !ok {
					t.Errorf("content_block_start for text has no text field: %+v", block)
				}
			case "thinking":
				if _, ok := block["thinking"]; !ok {
					t.Errorf("content_block_start for thinking has no thinking field: %+v", block)
				}
				if _, ok := block["signature"]; !ok {
					t.Errorf("content_block_start for thinking has no signature field: %+v", block)
				}
			}
		case "content_block_delta":
			delta, _ := ev.data["delta"].(map[string]any)
			switch kind := blockIndex[ev.data["index"].(float64)]; kind {
			case "thinking":
				if delta["type"] != "thinking_delta" {
					t.Errorf("delta in a thinking block = %+v", delta)
				}
				s, _ := delta["thinking"].(string)
				thinking += s
			case "text":
				if delta["type"] != "text_delta" {
					t.Errorf("delta in a text block = %+v", delta)
				}
				s, _ := delta["text"].(string)
				text += s
			default:
				t.Errorf("delta for an unopened block: %+v", ev.data)
			}
		case "message_delta":
			delta, _ := ev.data["delta"].(map[string]any)
			if delta["stop_reason"] != "end_turn" {
				t.Errorf("message_delta = %+v", delta)
			}
			u, _ := ev.data["usage"].(map[string]any)
			if u["input_tokens"] != float64(11) || u["output_tokens"] != float64(3) {
				t.Errorf("usage = %+v", u)
			}
		}
	}
	if text != "Hello there." {
		t.Errorf("streamed text = %q", text)
	}
	if thinking != "hmm" {
		t.Errorf("streamed thinking = %q", thinking)
	}
}

// An empty answer must still be a well-formed message with one content block.
func TestStreamingEmptyAnswer(t *testing.T) {
	rec := post(t, testHandler(t, fake.New("fakecli", "", "small")),
		`{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := names(events(t, rec.Body.String()))
	want := []string{"message_start", "content_block_start",
		"content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("event sequence = %v, want %v", got, want)
	}
}

// A backend that only thinks out loud still ends with a text block, so a client
// reading the last block finds the (empty) answer where it expects it.
func TestStreamingThinkingOnlyAnswer(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.Thinking = "hmm"
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	evs := events(t, rec.Body.String())
	var kinds []string
	for _, ev := range evs {
		if ev.name == "content_block_start" {
			block, _ := ev.data["content_block"].(map[string]any)
			kind, _ := block["type"].(string)
			kinds = append(kinds, kind)
		}
	}
	if strings.Join(kinds, ",") != "thinking,text" {
		t.Errorf("content blocks = %v, want a thinking block then an empty text block", kinds)
	}
	if last := evs[len(evs)-1]; last.name != "message_stop" {
		t.Errorf("event sequence = %v", names(evs))
	}
}

// A backend that fails before producing anything must come back as an ordinary
// HTTP error: the response has not been committed yet, and an SDK can only act
// on a status code.
func TestStreamingFailureBeforeFirstToken(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.Fail = &ir.Error{Code: ir.CodeUpstreamUnavailable, Message: "the CLI is not logged in"}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want a JSON error rather than a stream", ct)
	}
	if env := decodeError(t, rec); !strings.Contains(env.Error.Message, "not logged in") {
		t.Errorf("error = %+v", env.Error)
	}
}

// Once frames are out the status line is spent, so the failure travels as the
// dialect's own error event.
func TestStreamingFailureMidStream(t *testing.T) {
	backend := fake.New("fakecli", "half an answer ", "small")
	backend.Fail = &ir.Error{Code: ir.CodeUpstreamError, Message: "the CLI died", Detail: "signal: killed"}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the already-committed 200; body = %s", rec.Code, rec.Body)
	}
	evs := events(t, rec.Body.String())
	last := evs[len(evs)-1]
	if last.name != "error" {
		t.Fatalf("event sequence = %v, want it to end with an error event", names(evs))
	}
	body, _ := last.data["error"].(map[string]any)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "the CLI died") || !strings.Contains(msg, "signal: killed") {
		t.Errorf("error event = %+v", body)
	}
	if body["type"] != "api_error" {
		t.Errorf("error type = %v, want api_error", body["type"])
	}
}

// A run that ends without a terminal event is a broken backend, not an empty
// answer, and the caller has to be able to tell.
func TestStreamingTruncatedRunIsReported(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.Silent = true
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(decodeError(t, rec).Error.Message, "without completing") {
		t.Errorf("error = %s", rec.Body)
	}
}

// A backend that is slow to say anything must not look like a dead connection.
// The ping cannot come first, though: the SDKs reject any event before
// message_start, so opening the message is part of sending one.
func TestStreamingKeepaliveOpensASlowStream(t *testing.T) {
	backend := fake.New("fakecli", "eventually", "small")
	backend.FirstTokenDelay = 150 * time.Millisecond
	h := testHandlerTuned(t, backend, func(h *Handler) { h.Heartbeat = 5 * time.Millisecond })

	rec := post(t, h, `{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := names(events(t, rec.Body.String()))
	if got[0] != "message_start" {
		t.Fatalf("event sequence = %v, want message_start first", got)
	}
	if got[1] != "ping" {
		t.Errorf("event sequence = %v, want a ping while the backend was quiet", got)
	}
	if last := got[len(got)-1]; last != "message_stop" {
		t.Errorf("event sequence = %v, want it to end with message_stop", got)
	}
}

// A stream that has started but gone quiet must be kept alive with pings, and
// they must not disturb the event sequence.
func TestStreamingPing(t *testing.T) {
	backend := fake.New("fakecli", "slow", "small")
	backend.Delay = 100 * time.Millisecond
	h := testHandlerTuned(t, backend, func(h *Handler) { h.Heartbeat = 5 * time.Millisecond })

	rec := post(t, h, `{"model":"fakecli:small","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	evs := events(t, rec.Body.String())
	var pings int
	for _, ev := range evs {
		if ev.name == "ping" {
			pings++
		}
	}
	if pings == 0 {
		t.Errorf("no ping in a stream that idled: %s", rec.Body)
	}
	if last := evs[len(evs)-1]; last.name != "message_stop" {
		t.Errorf("event sequence = %v, want it to end with message_stop", names(evs))
	}
}
