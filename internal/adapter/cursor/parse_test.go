package cursor

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/ir"
)

// replay feeds lines through a parser and returns everything it emitted.
func replay(t *testing.T, lines []string, runErr error) []ir.Event {
	t.Helper()
	var got []ir.Event
	p := newParser(func(e ir.Event) bool {
		got = append(got, e)
		return true
	})
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := p.Line([]byte(line)); err != nil {
			t.Fatalf("parser rejected a line: %v", err)
		}
	}
	p.Finish(runErr)
	return got
}

func readFixture(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return lines
}

func collect(events []ir.Event) (text, thinking string, done *ir.Event, failure *ir.Error) {
	var t, th strings.Builder
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case ir.EventTextDelta:
			t.WriteString(ev.Text)
		case ir.EventThinkingDelta:
			th.WriteString(ev.Text)
		case ir.EventDone:
			done = &events[i]
		case ir.EventError:
			failure = ev.Err
		}
	}
	return t.String(), th.String(), done, failure
}

// TestPartialOutputTranscript replays a recording of a real
// `cursor-agent --print --output-format stream-json --stream-partial-output`
// run. Its point is the trailing flush line: the CLI repeats everything it has
// streamed since the last boundary just before the result, and that repeat must
// not reach the caller. See testdata/PROVENANCE.md.
func TestPartialOutputTranscript(t *testing.T) {
	events := replay(t, readFixture(t, "partial.jsonl"), nil)
	text, thinking, done, failure := collect(events)

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Hello from the fixture."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if strings.Count(text, "Hello from the fixture.") != 1 {
		t.Errorf("the flush line duplicated the answer: %q", text)
	}
	if !strings.HasPrefix(thinking, "I will reply with exactly") {
		t.Errorf("thinking = %q, want the reasoning deltas surfaced", thinking)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != ir.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.Model != "Auto" {
		t.Errorf("model = %q, want the one the init line reported", done.Model)
	}
	if done.Usage == nil {
		t.Fatal("no usage on the done event")
	}
	// The CLI spells these inputTokens/outputTokens/cacheReadTokens; getting the
	// mapping wrong is invisible without pinning the numbers.
	if u := done.Usage; u.InputTokens != 13196 || u.OutputTokens != 87 ||
		u.CacheReadInputTokens != 2560 || u.CacheCreationInputTokens != 0 {
		t.Errorf("usage = %+v", u)
	}

	// Streaming means more than one delta reached the client.
	var deltas int
	for _, ev := range events {
		if ev.Type == ir.EventTextDelta {
			deltas++
		}
	}
	if deltas < 2 {
		t.Errorf("text arrived in %d delta(s); the partial-output run should stream", deltas)
	}
}

// TestChunkedTranscript replays a real run without --stream-partial-output.
// There the whole answer arrives as one assistant line — which is itself the
// flush — so the parser must emit it rather than mistake it for a repeat.
func TestChunkedTranscript(t *testing.T) {
	events := replay(t, readFixture(t, "chunked.jsonl"), nil)
	text, thinking, done, failure := collect(events)

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Same chunk."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if thinking == "" {
		t.Error("expected the reasoning deltas to be surfaced")
	}
	if done == nil || done.Model != "Auto" {
		t.Fatalf("done = %+v", done)
	}
	if u := done.Usage; u == nil || u.InputTokens != 15242 || u.OutputTokens != 225 ||
		u.CacheReadInputTokens != 512 {
		t.Errorf("usage = %+v", done.Usage)
	}
	// One line in, one delta out: nothing to deduplicate here.
	var deltas int
	for _, ev := range events {
		if ev.Type == ir.EventTextDelta {
			deltas++
		}
	}
	if deltas != 1 {
		t.Errorf("deltas = %d, want the single assistant line emitted once", deltas)
	}
}

// A flush that repeats the accumulated text is dropped; the same text arriving
// again after a boundary is kept.
func TestFlushDeduplication(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"ab"}`,
	}
	if text, _, _, _ := collect(replay(t, lines, nil)); text != "ab" {
		t.Errorf("text = %q, want the flush before the result dropped", text)
	}

	// Text identical to what came before, but after a boundary: the CLI reset
	// its own buffer there, so this is a fresh chunk and dropping it would
	// silently truncate the answer. No real run produced this, which is why it
	// is stated by hand rather than recorded.
	lines = []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"tool_call","subtype":"started","call_id":"c1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"abab"}`,
	}
	if text, _, _, _ := collect(replay(t, lines, nil)); text != "abab" {
		t.Errorf("text = %q, want abab: a repeat after a boundary is real text", text)
	}

	// The same pair, but with a real delta in between: now the middle line was
	// text, not a flush.
	lines = []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"c"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ababc"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"ababc"}`,
	}
	if text, _, _, _ := collect(replay(t, lines, nil)); text != "ababc" {
		t.Errorf("text = %q, want ababc", text)
	}
}

// Without any assistant line, the terminal line is the only copy of the answer.
func TestResultFallback(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"Composer 1"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"only in the result","usage":{"inputTokens":5,"outputTokens":7}}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error: %v", failure)
	}
	if text != "only in the result" {
		t.Errorf("text = %q", text)
	}
	if done == nil || done.Usage == nil || done.Usage.OutputTokens != 7 {
		t.Errorf("usage not carried over from the result line: %+v", done)
	}
}

// The real thing this CLI does when nobody is logged in: it writes to stderr and
// exits non-zero, with nothing on stdout.
func TestAuthFailureBecomesUnavailable(t *testing.T) {
	runErr := &ir.Error{
		Code:    ir.CodeUpstreamError,
		Message: "agent CLI cursor-agent failed: exit status 1",
		Detail:  "Error: Authentication required. Please run 'agent login' first, or set CURSOR_API_KEY env var.",
	}
	_, _, done, failure := collect(replay(t, nil, runErr))
	if done != nil {
		t.Error("a failed run must not be reported as done")
	}
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if failure.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, ir.CodeUpstreamUnavailable)
	}
	if !strings.Contains(failure.Message, "cursor-agent login") {
		t.Errorf("message should tell the user how to fix it, got %q", failure.Message)
	}
	if !strings.Contains(failure.Detail, "Authentication required") {
		t.Errorf("the CLI's own complaint should survive: %+v", failure)
	}
}

func TestRateLimitBecomesOverloaded(t *testing.T) {
	runErr := &ir.Error{
		Code:    ir.CodeUpstreamError,
		Message: "agent CLI cursor-agent failed: exit status 1",
		Detail:  "Error: 429 Too Many Requests",
	}
	_, _, _, failure := collect(replay(t, nil, runErr))
	if failure == nil || failure.Code != ir.CodeOverloaded {
		t.Fatalf("want an overloaded error, got %+v", failure)
	}
}

func TestTruncatedRunIsAnError(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"half"}]}}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("a run without a result line must not be reported as done")
	}
	if failure == nil || failure.Code != ir.CodeUpstreamError {
		t.Fatalf("want an upstream error, got %+v", failure)
	}
	// What did arrive is still worth delivering; the error says it is partial.
	if text != "half" {
		t.Errorf("text = %q, want the partial answer to survive", text)
	}
}

func TestUnknownLinesAreIgnored(t *testing.T) {
	lines := []string{
		`not json at all`,
		`{"type":"some_future_event","payload":{"nested":true}}`,
		`{"type":"system","subtype":"task_notification","message":"a string where an object usually goes"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error: %v", failure)
	}
	if text != "ok" || done == nil {
		t.Errorf("text = %q, done = %+v", text, done)
	}
}

func TestFailedResultIsAnError(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the model refused to answer"}`,
	}
	_, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("an error result must not be reported as done")
	}
	if failure == nil || failure.Code != ir.CodeUpstreamError {
		t.Fatalf("want an upstream error, got %+v", failure)
	}
	if !strings.Contains(failure.Detail, "refused") {
		t.Errorf("the CLI's own explanation should survive: %+v", failure)
	}
}
