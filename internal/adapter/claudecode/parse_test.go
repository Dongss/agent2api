package claudecode

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

// TestGoldenTranscript replays a recording of real `claude --output-format
// stream-json` output (Claude Code 2.1.231; see testdata/PROVENANCE.md). If a
// CLI upgrade changes the event shapes, this fails loudly instead of quietly
// returning empty responses.
func TestGoldenTranscript(t *testing.T) {
	events := replay(t, readFixture(t, "simple.stream.jsonl"), nil)
	text, thinking, done, failure := collect(events)

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Hello from the fixture."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if thinking == "" {
		t.Error("expected thinking deltas to be surfaced")
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != ir.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q", done.Model)
	}
	if done.Usage == nil || done.Usage.OutputTokens == 0 {
		t.Errorf("usage not reported: %+v", done.Usage)
	}
	if done.Usage.InputTokens == 0 && done.Usage.CacheCreationInputTokens == 0 {
		t.Errorf("no input tokens reported: %+v", done.Usage)
	}
	// Deltas must not be duplicated by the cumulative `assistant` lines that
	// the same run also emits.
	if strings.Count(text, "Hello from the fixture.") != 1 {
		t.Errorf("assistant text duplicated: %q", text)
	}
}

// TestResultFallback covers a CLI that streams no partial messages: the whole
// answer arrives only on the terminal result line.
func TestResultFallback(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-4-5"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"only in the result","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":7}}`,
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

func TestUnknownLinesAreIgnored(t *testing.T) {
	lines := []string{
		`not json at all`,
		`{"type":"some_future_event","payload":{"nested":true}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}}`,
		`{"type":"result","subtype":"success","result":"ok"}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error: %v", failure)
	}
	if text != "ok" {
		t.Errorf("text = %q, want ok", text)
	}
	if done == nil {
		t.Error("expected a done event")
	}
}

func TestAuthFailureBecomesUnavailable(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key · Please run /login"}`,
	}
	_, _, _, failure := collect(replay(t, lines, nil))
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if failure.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, ir.CodeUpstreamUnavailable)
	}
	if !strings.Contains(failure.Message, "auth login") {
		t.Errorf("message should tell the user how to fix it, got %q", failure.Message)
	}
}

func TestRateLimitBecomesOverloaded(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: rate limit exceeded"}`,
	}
	_, _, _, failure := collect(replay(t, lines, nil))
	if failure == nil || failure.Code != ir.CodeOverloaded {
		t.Fatalf("want an overloaded error, got %+v", failure)
	}
}

func TestCLIErrorWinsOverExitStatus(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"context window exceeded"}`,
	}
	exitErr := &ir.Error{Code: ir.CodeUpstreamError, Message: "agent CLI claude failed: exit status 1"}
	_, _, _, failure := collect(replay(t, lines, exitErr))
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if !strings.Contains(failure.Detail, "context window") {
		t.Errorf("the CLI's own explanation should survive, got %+v", failure)
	}
}

func TestTruncatedRunIsAnError(t *testing.T) {
	// Process died after some output but before the result line.
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half"}}}`,
	}
	_, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("a run without a result line must not be reported as done")
	}
	if failure == nil || failure.Code != ir.CodeUpstreamError {
		t.Fatalf("want an upstream error, got %+v", failure)
	}
}

func TestRedactArgs(t *testing.T) {
	a := &Adapter{}
	args := []string{"--print", "--append-system-prompt", "secret instructions", "--model", "opus"}
	got := a.redactArgs(args)
	if got[2] != "<redacted>" {
		t.Errorf("system prompt not redacted: %v", got)
	}
	if args[2] != "secret instructions" {
		t.Error("redactArgs must not mutate its input")
	}
	if got[4] != "opus" {
		t.Errorf("non-prompt args must survive: %v", got)
	}
}

// extra_args is operator-supplied and may carry a credential, so its values
// stay out of the log while its flag names remain.
func TestRedactArgsHidesExtraArgValues(t *testing.T) {
	a := &Adapter{}
	a.opts.Config.ExtraArgs = []string{"--tuning", "sk-secret", "--token=sk-inline", "--harmless"}
	args := append([]string{"--print", "--model", "haiku"}, a.opts.Config.ExtraArgs...)

	got := strings.Join(a.redactArgs(args), " ")
	for _, secret := range []string{"sk-secret", "sk-inline"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q reached the log: %s", secret, got)
		}
	}
	for _, kept := range []string{"--tuning", "--token=<redacted>", "--harmless", "haiku"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q should survive redaction: %s", kept, got)
		}
	}
}
