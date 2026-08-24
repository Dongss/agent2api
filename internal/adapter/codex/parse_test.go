package codex

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

// TestGoldenTranscript replays a recording of real `codex exec --json` output
// (codex-cli 0.148.0). If a CLI upgrade changes the event shapes, this fails
// loudly instead of quietly returning empty responses.
func TestGoldenTranscript(t *testing.T) {
	text, thinking, done, failure := collect(replay(t, readFixture(t, "simple.jsonl"), nil))

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Hello from the fixture."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if thinking != "" {
		t.Errorf("this run had no reasoning items, got %q", thinking)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != ir.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 181 || done.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", done.Usage)
	}
}

// The same run also carries two non-fatal "error" items — deprecation warnings
// the CLI raises before it starts the turn. They must not fail the request.
func TestWarningItemsAreNotFatal(t *testing.T) {
	lines := readFixture(t, "simple.jsonl")
	var warnings int
	for _, line := range lines {
		if strings.Contains(line, `"type":"error"`) {
			warnings++
		}
	}
	if warnings == 0 {
		t.Fatal("fixture no longer contains the warning items this test covers")
	}
	if _, _, done, failure := collect(replay(t, lines, nil)); failure != nil || done == nil {
		t.Errorf("warnings turned a successful run into a failure: %v", failure)
	}
}

// TestReasoningTranscript covers a run with a reasoning item and more than one
// assistant message, both recorded from the real CLI.
func TestReasoningTranscript(t *testing.T) {
	text, thinking, done, failure := collect(replay(t, readFixture(t, "reasoning.jsonl"), nil))

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Hello from the fixture.\n\nSecond paragraph."; text != want {
		t.Errorf("text = %q, want the messages joined as paragraphs (%q)", text, want)
	}
	if !strings.Contains(thinking, "The user wants a greeting") {
		t.Errorf("reasoning not surfaced: %q", thinking)
	}
	if done == nil || done.Usage == nil {
		t.Fatalf("no usage on the done event: %+v", done)
	}
	// Codex counts cached tokens inside input_tokens; the split must add back up.
	u := done.Usage
	if u.InputTokens != 176 || u.CacheReadInputTokens != 1024 || u.OutputTokens != 42 {
		t.Errorf("usage = %+v, want cached tokens split out of the input count", u)
	}
	if u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens != 1200 {
		t.Errorf("usage = %+v, want the parts to sum to the reported input_tokens", u)
	}
}

// TestFailedTurn replays a real failure: a workspace with no credit left.
func TestFailedTurn(t *testing.T) {
	_, _, done, failure := collect(replay(t, readFixture(t, "out-of-credits.jsonl"), nil))
	if done != nil {
		t.Error("a failed turn must not be reported as done")
	}
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if failure.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, ir.CodeUpstreamUnavailable)
	}
	if !strings.Contains(failure.Detail, "out of credits") {
		t.Errorf("the CLI's own explanation should survive: %+v", failure)
	}
}

// Startup warnings (config deprecations, model metadata) are noise once the
// turn itself has something to say, so they stay out of the error.
func TestStartupWarningsStayOutOfTurnFailures(t *testing.T) {
	lines := []string{
		`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"[features].codex_hooks is deprecated"}}`,
		`{"type":"turn.started"}`,
		`{"type":"error","message":"Reconnecting... 1/5"}`,
		`{"type":"turn.failed","error":{"message":"the model gave up"}}`,
	}
	_, _, _, failure := collect(replay(t, lines, nil))
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if strings.Contains(failure.Detail, "deprecated") {
		t.Errorf("startup warning leaked into the failure detail: %q", failure.Detail)
	}
	if !strings.Contains(failure.Detail, "Reconnecting") {
		t.Errorf("what happened during the turn should be reported: %q", failure.Detail)
	}
}

// With nothing said during the turn, the startup warnings are all there is.
func TestStartupWarningsSurviveWhenNothingElseHappened(t *testing.T) {
	lines := []string{
		`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata for X not found"}}`,
		`{"type":"turn.started"}`,
	}
	_, _, _, failure := collect(replay(t, lines, nil))
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if !strings.Contains(failure.Detail, "Model metadata") {
		t.Errorf("detail = %q, want the only clue there was", failure.Detail)
	}
}

func TestAuthFailureBecomesUnavailable(t *testing.T) {
	lines := []string{
		`{"type":"turn.started"}`,
		`{"type":"turn.failed","error":{"message":"401 Unauthorized: please log in again"}}`,
	}
	_, _, _, failure := collect(replay(t, lines, nil))
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if failure.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, ir.CodeUpstreamUnavailable)
	}
	if !strings.Contains(failure.Message, "codex login") {
		t.Errorf("message should tell the user how to fix it, got %q", failure.Message)
	}
}

func TestRateLimitBecomesOverloaded(t *testing.T) {
	lines := []string{`{"type":"turn.failed","error":{"message":"429 rate limit exceeded"}}`}
	_, _, _, failure := collect(replay(t, lines, nil))
	if failure == nil || failure.Code != ir.CodeOverloaded {
		t.Fatalf("want an overloaded error, got %+v", failure)
	}
}

// Transient reconnects arrive as top-level error events. A run that recovers
// must still succeed, and its complaints must not reach the caller.
func TestTransientErrorsDoNotFailARecoveredRun(t *testing.T) {
	lines := []string{
		`{"type":"turn.started"}`,
		`{"type":"error","message":"Reconnecting... 1/5 (stream disconnected before completion)"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"recovered"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":2}}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("a recovered run must not fail: %v", failure)
	}
	if text != "recovered" || done == nil {
		t.Errorf("text = %q, done = %+v", text, done)
	}
}

// A run that dies without completing its turn must be an error, and the CLI's
// last complaint is the most useful thing to report.
func TestTruncatedRunIsAnError(t *testing.T) {
	lines := []string{
		`{"type":"turn.started"}`,
		`{"type":"error","message":"stream disconnected before completion"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"half"}}`,
	}
	_, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("a run without turn.completed must not be reported as done")
	}
	if failure == nil || failure.Code != ir.CodeUpstreamError {
		t.Fatalf("want an upstream error, got %+v", failure)
	}
	if !strings.Contains(failure.Detail, "stream disconnected") {
		t.Errorf("the CLI's own complaint should survive: %+v", failure)
	}
}

// An item reported as started, then updated, then completed is still one
// message.
func TestRepeatedItemIsEmittedOnce(t *testing.T) {
	lines := []string{
		`{"type":"item.started","item":{"id":"item_0","type":"agent_message","text":"only once"}}`,
		`{"type":"item.updated","item":{"id":"item_0","type":"agent_message","text":"only once"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"only once"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	text, _, _, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatal(failure)
	}
	if text != "only once" {
		t.Errorf("text = %q, want the item emitted exactly once", text)
	}
}

// Tool-shaped items belong to an agent, not to an LLM API: agent2api runs the
// CLI with nothing to use them on, and must not leak them into the answer.
func TestToolItemsAreIgnored(t *testing.T) {
	lines := []string{
		`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls","aggregated_output":"file.txt"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"web_search","query":"weather"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"answer"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	text, _, _, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatal(failure)
	}
	if text != "answer" {
		t.Errorf("text = %q, want only the assistant message", text)
	}
}

func TestUnknownLinesAreIgnored(t *testing.T) {
	lines := []string{
		`not json at all`,
		`{"type":"some_future_event","payload":{"nested":true}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ok"}}`,
		`{"type":"turn.completed"}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error: %v", failure)
	}
	if text != "ok" || done == nil {
		t.Errorf("text = %q, done = %+v", text, done)
	}
}
