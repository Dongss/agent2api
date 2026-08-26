package responses

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

// frame is one parsed SSE event.
type frame struct {
	name string
	data map[string]any
}

// stream posts a streaming request and parses the frames it gets back.
func stream(t *testing.T, h *Handler, body string) []frame {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Responses(rec, req)

	var frames []frame
	var name string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("frame %q carries invalid JSON: %v", name, err)
			}
			frames = append(frames, frame{name: name, data: data})
		}
	}
	return frames
}

func names(frames []frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.name
	}
	return out
}

func TestStreamEventOrder(t *testing.T) {
	b := fake.New("fake", "Hello there.")
	b.Thinking = "Thinking about it."
	b.Usage = &ir.Usage{InputTokens: 10, OutputTokens: 4, ReasoningOutputTokens: 2}
	h := newTestHandler(t, b)

	frames := stream(t, h, `{"model":"fake","input":"hi","stream":true}`)
	got := names(frames)

	// Reasoning is a complete item of its own before the message opens: the
	// Responses arrangement, not a block nested in the message.
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
	}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("event %d = %q, want %q\nfull order: %v", i, get(got, i), w, got)
		}
	}
	tail := []string{
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
	}
	if !containsRun(got, tail) {
		t.Errorf("reasoning does not close before the message opens\nfull order: %v", got)
	}
	closing := []string{
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if !containsRun(got, closing) {
		t.Errorf("the message does not close before the response does\nfull order: %v", got)
	}
}

// Clients reassemble output by sequence number, so a gap or a repeat is a
// protocol error even when every event is individually well formed.
func TestSequenceNumbersAreGapFree(t *testing.T) {
	b := fake.New("fake", "Hello there.")
	b.Thinking = "Thinking."
	h := newTestHandler(t, b)

	frames := stream(t, h, `{"model":"fake","input":"hi","stream":true}`)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	for i, f := range frames {
		seq, ok := f.data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("frame %d (%s) has no sequence_number", i, f.name)
		}
		if int(seq) != i {
			t.Fatalf("frame %d (%s) has sequence_number %d", i, f.name, int(seq))
		}
	}
}

func TestStreamDeltasReassembleIntoTheFinalResponse(t *testing.T) {
	b := fake.New("fake", "Hello there.")
	b.Thinking = "Thinking about it."
	b.Usage = &ir.Usage{InputTokens: 10, CacheReadInputTokens: 3, OutputTokens: 4, ReasoningOutputTokens: 2}
	h := newTestHandler(t, b)

	frames := stream(t, h, `{"model":"fake","input":"hi","stream":true}`)

	var text, thinking string
	var final map[string]any
	for _, f := range frames {
		switch f.name {
		case "response.output_text.delta":
			text += f.data["delta"].(string)
		case "response.reasoning_summary_text.delta":
			thinking += f.data["delta"].(string)
		case "response.completed":
			final, _ = f.data["response"].(map[string]any)
		}
	}
	if text != "Hello there." {
		t.Errorf("text = %q", text)
	}
	if thinking != "Thinking about it." {
		t.Errorf("thinking = %q", thinking)
	}
	if final == nil {
		t.Fatal("no response.completed")
	}
	if final["status"] != "completed" {
		t.Errorf("final status = %v", final["status"])
	}

	// The terminal event repeats the whole output, and it has to agree with the
	// deltas the client already accumulated.
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("final output has %d items, want reasoning and message", len(output))
	}
	msg, _ := output[1].(map[string]any)
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != text {
		t.Errorf("final text = %v, deltas said %q", part["text"], text)
	}
	usage, _ := final["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("no usage on the terminal event")
	}
	if usage["input_tokens"] != float64(13) {
		t.Errorf("input_tokens = %v, want the cached tokens counted in", usage["input_tokens"])
	}
}

// Every response ends with a message item, so a client reading the last item
// always finds the answer where it expects it — even when the backend only
// ever thought out loud.
func TestThinkingOnlyRunStillEndsWithAMessage(t *testing.T) {
	b := fake.New("fake", "")
	b.Thinking = "Thought, but said nothing."
	h := newTestHandler(t, b)

	frames := stream(t, h, `{"model":"fake","input":"hi","stream":true}`)
	var final map[string]any
	for _, f := range frames {
		if f.name == "response.completed" {
			final, _ = f.data["response"].(map[string]any)
		}
	}
	if final == nil {
		t.Fatal("no response.completed")
	}
	output, _ := final["output"].([]any)
	if len(output) == 0 {
		t.Fatal("no output items")
	}
	last, _ := output[len(output)-1].(map[string]any)
	if last["type"] != "message" {
		t.Errorf("last item = %v, want a message", last["type"])
	}
}

// Before the first frame the response is uncommitted, so a backend that fails
// immediately must produce a real status code rather than a 200 with an error
// buried in the body.
func TestFailureBeforeTheFirstFrameIsAStatusCode(t *testing.T) {
	b := fake.New("fake", "")
	b.Fail = &ir.Error{Code: ir.CodeUpstreamUnavailable, Message: "the fake CLI is not logged in"}
	h := newTestHandler(t, b)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"fake","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.Responses(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "response.created") {
		t.Error("the stream opened before the failure was known")
	}
}

// Once frames have gone out the status is already 200, so the failure has to
// travel as events instead.
func TestFailureMidStreamBecomesEvents(t *testing.T) {
	b := fake.New("fake", "Partial answer.")
	b.Fail = &ir.Error{Code: ir.CodeUpstreamError, Message: "the fake CLI died"}
	h := newTestHandler(t, b)

	frames := stream(t, h, `{"model":"fake","input":"hi","stream":true}`)
	got := names(frames)
	if !containsRun(got, []string{"response.failed", "error"}) {
		t.Fatalf("want response.failed then error, got %v", got)
	}
	for _, f := range frames {
		if f.name != "response.failed" {
			continue
		}
		final, _ := f.data["response"].(map[string]any)
		if final["status"] != "failed" {
			t.Errorf("status = %v, want failed", final["status"])
		}
		if final["error"] == nil {
			t.Error("response.failed carries no error")
		}
	}
}

// A backend that stops without a terminal event must not read as a complete
// short answer.
func TestTruncatedRunFails(t *testing.T) {
	b := fake.New("fake", "Half an ans")
	b.Silent = true
	h := newTestHandler(t, b)

	got := names(stream(t, h, `{"model":"fake","input":"hi","stream":true}`))
	if !containsRun(got, []string{"response.failed", "error"}) {
		t.Fatalf("a truncated run completed cleanly: %v", got)
	}
	for _, name := range got {
		if name == "response.completed" {
			t.Fatal("a truncated run reported response.completed")
		}
	}
}

// A slow backend must not look dead to a proxy. The keepalive is a comment, not
// an event, so it can precede response.created without breaking the numbering.
func TestKeepaliveIsAComment(t *testing.T) {
	b := fake.New("fake", "Sorry for the wait.")
	b.FirstTokenDelay = 40 * time.Millisecond
	h := newTestHandler(t, b)
	h.Heartbeat = 10 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"fake","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.Responses(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, ":") {
		t.Fatal("no comment line in the stream")
	}
	// The numbering must still be gap-free: comments carry no sequence number.
	frames := stream(t, h, `{"model":"fake","input":"hi","stream":true}`)
	for i, f := range frames {
		if int(f.data["sequence_number"].(float64)) != i {
			t.Fatalf("keepalives disturbed the numbering at frame %d (%s)", i, f.name)
		}
	}
}

func get(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "(missing)"
}

// containsRun reports whether want appears in got as a contiguous run.
func containsRun(got, want []string) bool {
	if len(want) > len(got) {
		return false
	}
	for i := 0; i+len(want) <= len(got); i++ {
		match := true
		for j := range want {
			if got[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
