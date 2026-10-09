package qwencode

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/ir"
)

// replay feeds lines through a parser and returns everything it emitted.
func replay(t *testing.T, lines []string, runErr error) []ir.Event {
	t.Helper()
	return replayVariant(t, lines, runErr, "")
}

// replayVariant is replay for a request that named a model. A line the parser
// rejects ends the feed, as the runner does by killing the CLI, and its error
// becomes the run's.
func replayVariant(t *testing.T, lines []string, runErr error, variant string) []ir.Event {
	t.Helper()
	var got []ir.Event
	p := newParser(func(e ir.Event) bool {
		got = append(got, e)
		return true
	}, variant)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := p.Line([]byte(line)); err != nil {
			runErr = err
			break
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

// TestGoldenTranscript replays a recording of real `qwen --output-format
// stream-json` output (see testdata/PROVENANCE.md). If a CLI upgrade changes
// the event shapes, this fails loudly instead of quietly returning empty
// responses.
func TestGoldenTranscript(t *testing.T) {
	text, thinking, done, failure := collect(replay(t, readFixture(t, "simple.jsonl"), nil))

	if failure != nil {
		t.Fatalf("unexpected error event: %v", failure)
	}
	if want := "Hello from the fixture."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if thinking == "" {
		t.Error("expected the reasoning block to be surfaced")
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != ir.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.Model != "qwen3.6-plus" {
		t.Errorf("model = %q", done.Model)
	}
	if done.Usage == nil || done.Usage.InputTokens == 0 || done.Usage.OutputTokens == 0 {
		t.Errorf("usage not reported: %+v", done.Usage)
	}
	// The terminal line repeats the answer already streamed; counting it twice
	// would double every response.
	if strings.Count(text, "Hello from the fixture.") != 1 {
		t.Errorf("the assistant text was duplicated by the result line: %q", text)
	}
}

// The tool list on the startup line is the containment guarantee — the deny
// list in the generated settings cannot cover a tool a future release adds, so
// what the CLI reports is checked rather than trusted.
func TestFixtureStartsWithNoTools(t *testing.T) {
	for _, line := range readFixture(t, "simple.jsonl") {
		var l streamLine
		if err := json.Unmarshal([]byte(line), &l); err != nil || l.Subtype != "init" {
			continue
		}
		if len(l.Tools) != 0 {
			t.Fatalf("the recording was made with tools available: %v", l.Tools)
		}
		return
	}
	t.Fatal("no init line in the fixture: the tool check has nothing to assert on")
}

func TestToolsAvailableFailsTheRun(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":["run_shell_command","glob"]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"an answer nobody should read"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"an answer nobody should read"}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("a run that started with tools must not be reported as done")
	}
	if text != "" {
		t.Errorf("text from a run that started with tools reached the caller: %q", text)
	}
	if failure == nil {
		t.Fatal("expected an error event")
	}
	// The message has to name the tools: whoever sees this needs to know which
	// ones the deny list missed.
	if !strings.Contains(failure.Detail, "run_shell_command") {
		t.Errorf("the tools that leaked are not named: %+v", failure)
	}
}

// TestUnknownModelIsNotSwappedForTheDefault replays what 0.25.0 did with
// `--model no-such-model-xyz`: no refusal, an init line naming the configured
// default, and an ordinary answer from it. Reported as a success, the caller
// would read that answer as the model they asked for.
func TestUnknownModelIsNotSwappedForTheDefault(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"qwen3-coder","tools":[]}`,
		`{"type":"assistant","message":{"model":"qwen3-coder","content":[{"type":"text","text":"Hi! How can I help you today?"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Hi! How can I help you today?"}`,
	}
	text, _, done, failure := collect(replayVariant(t, lines, nil, "no-such-model-xyz"))
	if done != nil || failure == nil {
		t.Fatalf("want an error, got done=%+v failure=%+v", done, failure)
	}
	if failure.Code != ir.CodeModelNotFound {
		t.Errorf("code = %s, want %s", failure.Code, ir.CodeModelNotFound)
	}
	for _, name := range []string{"no-such-model-xyz", "qwen3-coder", "modelProviders"} {
		if !strings.Contains(failure.Message, name) {
			t.Errorf("the message should name %q: %s", name, failure.Message)
		}
	}
	if text != "" {
		t.Errorf("the substitute's answer reached the caller: %q", text)
	}
}

// A model the CLI has is echoed back exactly, and the run goes through; with
// no model named, whatever the CLI chose is the answer.
func TestRequestedModelIsServed(t *testing.T) {
	for _, variant := range []string{"qwen3.6-plus", ""} {
		lines := []string{
			`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":[]}`,
			`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
		}
		text, _, done, failure := collect(replayVariant(t, lines, nil, variant))
		if failure != nil || done == nil || text != "ok" {
			t.Errorf("variant %q: text=%q done=%+v failure=%+v", variant, text, done, failure)
		}
	}
}

// The whole answer arrives on the terminal line when nothing was streamed.
func TestResultFallback(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":[]}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"only in the result","usage":{"input_tokens":5,"output_tokens":7}}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil {
		t.Fatalf("unexpected error: %v", failure)
	}
	if text != "only in the result" {
		t.Errorf("text = %q", text)
	}
	if done == nil || done.Usage == nil || done.Usage.InputTokens != 5 {
		t.Errorf("usage = %+v", done)
	}
}

func TestTruncatedRunIsAnError(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":[]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"half an ans"}]}}`,
	}
	_, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("a run with no terminal line must not be reported as done")
	}
	if failure == nil {
		t.Fatal("expected an error event")
	}
}

// Under some flags the CLI writes a banner to stdout ahead of the JSON. Stray
// output must not be fatal.
func TestNonJSONLinesAreIgnored(t *testing.T) {
	lines := []string{
		`⚠ SAFE MODE — all customizations disabled (hooks, extensions, skills, MCP servers, QWEN.md).`,
		`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":[]}`,
		`not json at all`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"fine"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"fine"}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if failure != nil || done == nil {
		t.Fatalf("stray stdout broke the run: %v", failure)
	}
	if text != "fine" {
		t.Errorf("text = %q", text)
	}
}

func TestFailuresAreClassified(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		code ir.Code
	}{
		{
			"unauthenticated",
			"No auth type is selected. Please configure an auth type (e.g. via settings or `--auth-type`) before running in non-interactive mode.",
			ir.CodeUpstreamUnavailable,
		},
		{"out of quota", "Quota exceeded for this billing period.", ir.CodeUpstreamUnavailable},
		{"rate limited", "Rate limit reached, please retry", ir.CodeOverloaded},
		{"anything else", "something went wrong", ir.CodeUpstreamError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, err := json.Marshal(map[string]any{
				"type": "result", "subtype": "error_during_execution", "is_error": true,
				"error": map[string]string{"message": tc.msg},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _, done, failure := collect(replay(t, []string{string(line)}, nil))
			if done != nil {
				t.Error("a failed run must not be reported as done")
			}
			if failure == nil {
				t.Fatal("expected an error event")
			}
			if failure.Code != tc.code {
				t.Errorf("code = %s, want %s", failure.Code, tc.code)
			}
			// The CLI's own explanation is the useful part; it must survive.
			if !strings.Contains(failure.Detail, strings.Fields(tc.msg)[0]) {
				t.Errorf("the CLI's explanation was lost: %+v", failure)
			}
		})
	}
}

// An error the CLI described itself is more specific than "exited with 1".
func TestCLIErrorWinsOverExitStatus(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"error":{"message":"No auth type is selected."}}`,
	}
	_, _, _, failure := collect(replay(t, lines, errors.New("exit status 1")))
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if failure.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want the CLI's own classification", failure.Code)
	}
}

// The CLI reports an upstream API failure as a successful turn whose answer is
// the error text. Passing that through would give the caller a 200 whose body
// is an error dressed as a model response.
func TestUpstreamAPIErrorIsNotAnAnswer(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":[]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"[API Error: 400 Access denied, please make sure your account is in good standing.]"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"[API Error: 400 Access denied, please make sure your account is in good standing.]"}`,
	}
	text, _, done, failure := collect(replay(t, lines, nil))
	if done != nil {
		t.Error("an upstream failure must not be reported as a completed turn")
	}
	if failure == nil {
		t.Fatal("expected an error event")
	}
	if !strings.Contains(failure.Detail, "Access denied") {
		t.Errorf("the CLI's explanation was lost: %+v", failure)
	}
	if text != "" {
		t.Errorf("the error reached the caller as answer text: %q", text)
	}
}

// TestFlaggedAPIErrorIsNotStreamedFirst is the same failure as 0.25.0 reports
// it, from a run against a provider entry holding a bad key: the result line
// now says is_error, but the wrapper still arrives first as an assistant
// message, and streamed it would have opened a 200 before the error was known.
func TestFlaggedAPIErrorIsNotStreamedFirst(t *testing.T) {
	const wrapper = "[API Error: 401 Incorrect API key provided.]"
	lines := []string{
		`{"type":"system","subtype":"init","model":"qwen3.6-plus","tools":[]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + wrapper + `"}]}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"error":{"message":"` + wrapper + `"},"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`,
	}
	events := replay(t, lines, nil)
	text, _, done, failure := collect(events)
	if text != "" || done != nil {
		t.Errorf("want no answer, got text=%q done=%+v", text, done)
	}
	if failure == nil || failure.Code != ir.CodeUpstreamUnavailable {
		t.Fatalf("a 401 should read as not authenticated, got %+v", failure)
	}
	if !strings.Contains(failure.Detail, "Incorrect API key") {
		t.Errorf("the CLI's explanation was lost: %+v", failure)
	}
}

// The match is on the whole answer, so prose that merely mentions an API error
// stays an answer.
func TestProseAboutAPIErrorsIsStillAnAnswer(t *testing.T) {
	answer := "When you see [API Error: 400] in a log, check the account balance first."
	line, err := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": answer,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _, done, failure := collect(replay(t, []string{string(line)}, nil))
	if failure != nil {
		t.Fatalf("a real answer was mistaken for a failure: %v", failure)
	}
	if done == nil || text != answer {
		t.Errorf("text = %q", text)
	}
}
