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
// (codex-cli 0.149.1; see testdata/PROVENANCE.md). If a CLI upgrade changes the
// event shapes, this fails loudly instead of quietly returning empty responses.
//
// The prose is asserted by ends and length rather than in full: the point is
// that the message survives intact, and a 632-byte paragraph inline would bury
// that. Truncation, double-counting and a dropped item all still fail here.
func TestGoldenTranscript(t *testing.T) {
	text, thinking, done, failure := collect(replay(t, readFixture(t, "simple.jsonl"), nil))

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if !strings.HasPrefix(text, "The sea stretches endlessly beneath the ") ||
		!strings.HasSuffix(text, "ht open and swallow you whole.") || len(text) != 632 {
		t.Errorf("text = %d bytes starting %q", len(text), truncate(text, 40))
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
	if done.Usage == nil || done.Usage.InputTokens != 12840 || done.Usage.OutputTokens != 134 {
		t.Errorf("usage = %+v", done.Usage)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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

// TestReasoningTranscript covers a recorded run that carries a reasoning item
// and reports cached input tokens, which is where the usage split gets
// interesting. Multiple assistant messages in one turn are covered separately
// by TestMessagesInOneTurnAreJoined: the CLI did not produce that shape in any
// run recorded here, so it is asserted from a constructed transcript instead.
func TestReasoningTranscript(t *testing.T) {
	text, thinking, done, failure := collect(replay(t, readFixture(t, "reasoning.jsonl"), nil))

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Hello from the fixture."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if !strings.Contains(thinking, "repeat a specific phrase") {
		t.Errorf("reasoning not surfaced: %q", thinking)
	}
	if done == nil || done.Usage == nil {
		t.Fatalf("no usage on the done event: %+v", done)
	}
	// Codex counts cached tokens inside input_tokens; the split must add back up.
	u := done.Usage
	if u.InputTokens != 1957 || u.CacheReadInputTokens != 10880 || u.OutputTokens != 22 {
		t.Errorf("usage = %+v, want cached tokens split out of the input count", u)
	}
	if u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens != 12837 {
		t.Errorf("usage = %+v, want the parts to sum to the reported input_tokens", u)
	}
}

// TestFailedTurn replays a real failure: an account with no quota left. The
// 0.148.0 recording this replaced said "out of credits" where 0.149.1 says
// "Quota exceeded" — classify matches on "quota"/"billing", so the reworded
// message still lands on the same code.
func TestFailedTurn(t *testing.T) {
	_, _, done, failure := collect(replay(t, readFixture(t, "quota-exceeded.jsonl"), nil))
	if done != nil {
		t.Error("a failed turn must not be reported as done")
	}
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if failure.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, ir.CodeUpstreamUnavailable)
	}
	if !strings.Contains(failure.Detail, "Quota exceeded") {
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

// The transcripts below are constructed, not recorded: codex-cli 0.149.1 emits
// neither shape (see testdata/PROVENANCE.md). They pin behaviour the parser
// already promises, so a CLI that starts emitting either one fails here rather
// than silently changing what callers get.

// TestMessagesInOneTurnAreJoined covers a turn carrying more than one
// agent_message. A recorded run used to show this; the CLI stopped producing it,
// but the parser still has to join rather than keep only the first.
func TestMessagesInOneTurnAreJoined(t *testing.T) {
	lines := []string{
		`{"type":"thread.started","thread_id":"00000000-0000-7000-8000-000000000001"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"First paragraph."}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Second paragraph."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if want := "First paragraph.\n\nSecond paragraph."; text != want {
		t.Errorf("text = %q, want the messages joined as paragraphs (%q)", text, want)
	}
}

// TestPartialItemDoesNotShadowTheCompletedOne guards the dedup in parser.item.
//
// Items are keyed by id and the first non-empty text wins, which is right when
// item.started arrives empty and item.completed carries the whole message. It
// would be wrong if a future CLI put *partial* text on item.updated: taking the
// partial and marking the id seen would drop the rest of the answer, and a
// truncated answer that reports success is the one failure mode this package
// exists to prevent. This test documents which of the two the parser does.
func TestPartialItemDoesNotShadowTheCompletedOne(t *testing.T) {
	lines := []string{
		`{"type":"thread.started","thread_id":"00000000-0000-7000-8000-000000000001"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"item_0","type":"agent_message","text":""}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"The whole answer."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":3}}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	// An empty item.started must not consume the id.
	if want := "The whole answer."; text != want {
		t.Errorf("text = %q, want %q: an empty item.started swallowed the completed item", text, want)
	}
}

// TestPartialItemTextWouldTruncate pins a known limitation rather than a
// promise. If a future codex-cli puts *partial* text on item.updated, the
// first-non-empty-text-wins dedup keeps the fragment and drops the rest — a
// truncated answer reported as a success, which is exactly what this package is
// supposed to make impossible.
//
// codex-cli 0.149.1 emits only item.completed, so nothing hits this today. The
// test exists so the day that changes, it shows up here as a decision to make
// (buffer per id until turn.completed, or emit deltas) instead of as silently
// clipped answers in production.
func TestPartialItemTextWouldTruncate(t *testing.T) {
	lines := []string{
		`{"type":"thread.started","thread_id":"00000000-0000-7000-8000-000000000001"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.updated","item":{"id":"item_0","type":"agent_message","text":"The whole"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"The whole answer."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":3}}`,
	}
	text, _, _, _ := collect(replay(t, lines, nil))
	if text == "The whole answer." {
		t.Fatal("the parser now handles partial item text: drop this test and cover it as behaviour instead")
	}
	if text != "The whole" {
		t.Errorf("text = %q; the fragment-wins behaviour this test documents has changed", text)
	}
}
