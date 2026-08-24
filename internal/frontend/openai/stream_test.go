package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

// frames splits an SSE body into its data payloads, dropping keepalive
// comments the way a client does.
func frames(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var data []string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ": "):
				continue
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			default:
				t.Fatalf("unexpected SSE line %q in an OpenAI stream", line)
			}
		}
		if len(data) > 0 {
			out = append(out, strings.Join(data, "\n"))
		}
	}
	return out
}

func decodeChunks(t *testing.T, body string) []chatChunk {
	t.Helper()
	all := frames(t, body)
	if len(all) == 0 {
		t.Fatalf("empty stream")
	}
	if last := all[len(all)-1]; last != "[DONE]" {
		t.Errorf("stream must end with the [DONE] sentinel, got %q", last)
	}
	chunks := make([]chatChunk, 0, len(all)-1)
	for _, frame := range all[:len(all)-1] {
		var c chatChunk
		if err := json.Unmarshal([]byte(frame), &c); err != nil {
			t.Fatalf("frame is not a chunk: %v\n%s", err, frame)
		}
		chunks = append(chunks, c)
	}
	return chunks
}

func TestStreamingCompletion(t *testing.T) {
	backend := fake.New("fakecli", "Hello there.", "small")
	backend.Thinking = "hmm"
	backend.Usage = &ir.Usage{InputTokens: 10, OutputTokens: 3}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !backend.Requests[0].Stream {
		t.Error("a streaming request must tell the adapter it is streaming")
	}

	chunks := decodeChunks(t, rec.Body.String())
	if len(chunks) < 3 {
		t.Fatalf("want at least role, content and finish chunks, got %d", len(chunks))
	}
	if first := chunks[0].Choices[0].Delta; first.Role != "assistant" {
		t.Errorf("the first chunk must open the choice with the role, got %+v", first)
	}

	var text, thinking string
	var finish string
	for _, c := range chunks {
		if c.Object != "chat.completion.chunk" || c.ID != chunks[0].ID {
			t.Errorf("inconsistent chunk envelope: %+v", c)
		}
		if c.Model != "fakecli:small" {
			t.Errorf("model = %q, want the name the caller asked for", c.Model)
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
			thinking += ch.Delta.ReasoningContent
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			t.Error("usage must be omitted unless stream_options asked for it")
		}
	}
	if text != "Hello there." {
		t.Errorf("streamed text = %q", text)
	}
	if thinking != "hmm" {
		t.Errorf("streamed reasoning = %q", thinking)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
}

func TestStreamingUsageOptIn(t *testing.T) {
	backend := fake.New("fakecli", "ok", "small")
	backend.Usage = &ir.Usage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 5}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","stream":true,"stream_options":{"include_usage":true},
		  "messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	chunks := decodeChunks(t, rec.Body.String())
	last := chunks[len(chunks)-1]
	if len(last.Choices) != 0 {
		t.Errorf("the usage chunk carries no choices, got %+v", last.Choices)
	}
	if last.Usage == nil || last.Usage.PromptTokens != 16 || last.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want cached input counted in prompt_tokens", last.Usage)
	}
	// The finish_reason must still arrive, on the chunk before the usage one.
	prev := chunks[len(chunks)-2]
	if len(prev.Choices) != 1 || prev.Choices[0].FinishReason == nil {
		t.Errorf("finish_reason missing from the chunk before usage: %+v", prev)
	}
}

// A backend that fails before producing anything must come back as an ordinary
// HTTP error: the response has not been committed yet, and an SDK can only act
// on a status code.
func TestStreamingFailureBeforeFirstToken(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.Fail = &ir.Error{
		Code:    ir.CodeUpstreamUnavailable,
		Message: "the CLI is not logged in",
		Detail:  "run claude auth login",
	}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want a JSON error rather than a stream", ct)
	}
	body := decodeError(t, rec)
	if !strings.Contains(body.Message, "not logged in") || !strings.Contains(body.Message, "auth login") {
		t.Errorf("error message = %q", body.Message)
	}
}

// Once frames are out the status line is spent, so the failure has to travel
// inside the stream.
func TestStreamingFailureMidStream(t *testing.T) {
	backend := fake.New("fakecli", "half an answer ", "small")
	backend.Fail = &ir.Error{Code: ir.CodeUpstreamError, Message: "the CLI died", Detail: "signal: killed"}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the already-committed 200; body = %s", rec.Code, rec.Body)
	}
	all := frames(t, rec.Body.String())
	if all[len(all)-1] != "[DONE]" {
		t.Errorf("a failed stream must still be terminated: %v", all)
	}
	var failed bool
	for _, frame := range all[:len(all)-1] {
		var env errorEnvelope
		if err := json.Unmarshal([]byte(frame), &env); err == nil && env.Error.Message != "" {
			failed = true
			if !strings.Contains(env.Error.Message, "the CLI died") ||
				!strings.Contains(env.Error.Message, "signal: killed") {
				t.Errorf("error frame = %+v", env.Error)
			}
		}
	}
	if !failed {
		t.Errorf("no error frame in the stream: %v", all)
	}
}

// A run that ends without a terminal event is a broken backend, not an empty
// answer, and the caller has to be able to tell.
func TestStreamingTruncatedRunIsReported(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.Silent = true
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(decodeError(t, rec).Message, "without completing") {
		t.Errorf("error = %s", rec.Body)
	}
}

// A backend that is slow to say anything must not look like a dead connection:
// the keepalive opens the stream and holds it open.
func TestStreamingKeepaliveOpensASlowStream(t *testing.T) {
	backend := fake.New("fakecli", "eventually", "small")
	backend.FirstTokenDelay = 150 * time.Millisecond
	h := testHandlerTuned(t, backend, func(h *Handler) { h.Heartbeat = 5 * time.Millisecond })

	rec := post(t, h, `{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	firstData := strings.Index(body, "data: ")
	firstPing := strings.Index(body, ": keepalive")
	if firstPing < 0 {
		t.Fatalf("no keepalive while the backend was quiet: %s", body)
	}
	if firstData >= 0 && firstPing > firstData {
		t.Error("the keepalive should arrive before the first chunk, not after it")
	}
	if got := decodeChunks(t, body); len(got) < 3 {
		t.Errorf("chunks = %d, want the full sequence after the wait", len(got))
	}
}

// The flip side of that trade-off: once a keepalive has committed the response,
// a failure can only travel inside the stream.
func TestStreamingSlowFailureTravelsInStream(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.FirstTokenDelay = 150 * time.Millisecond
	backend.Fail = &ir.Error{Code: ir.CodeTimeout, Message: "the CLI gave up"}
	h := testHandlerTuned(t, backend, func(h *Handler) { h.Heartbeat = 5 * time.Millisecond })

	rec := post(t, h, `{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the response already committed by the keepalive; body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "the CLI gave up") {
		t.Errorf("the failure never reached the client: %s", rec.Body)
	}
}

// A stream that has started but gone quiet must be kept alive, and the
// keepalives must not disturb the chunk sequence.
func TestStreamingHeartbeat(t *testing.T) {
	backend := fake.New("fakecli", "slow", "small")
	// The backend goes quiet after its text, with a cadence far below that gap.
	backend.Delay = 100 * time.Millisecond
	h := testHandlerTuned(t, backend, func(h *Handler) { h.Heartbeat = 5 * time.Millisecond })

	rec := post(t, h, `{"model":"fakecli:small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "\n: keepalive\n") {
		t.Errorf("no keepalive comment in a stream that idled: %s", rec.Body)
	}
	if got := decodeChunks(t, rec.Body.String()); len(got) < 3 {
		t.Errorf("chunks = %d, want the full sequence alongside the keepalives", len(got))
	}
}
