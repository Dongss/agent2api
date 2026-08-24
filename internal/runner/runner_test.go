package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/ir"
)

func collectLines(t *testing.T, spec Spec) ([]string, Result, error) {
	t.Helper()
	var lines []string
	res, err := Run(context.Background(), spec, func(b []byte) error {
		lines = append(lines, string(b))
		return nil
	})
	return lines, res, err
}

func TestRunStreamsLines(t *testing.T) {
	lines, res, err := collectLines(t, helper(t, "emit", "one", "two", "three"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d", res.ExitCode)
	}
	if strings.Join(lines, ",") != "one,two,three" {
		t.Errorf("lines = %v", lines)
	}
}

func TestStdinIsDelivered(t *testing.T) {
	spec := helper(t, "cat")
	spec.Stdin = "hello from stdin"
	lines, _, err := collectLines(t, spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(lines) != 1 || lines[0] != "hello from stdin" {
		t.Errorf("lines = %v", lines)
	}
}

func TestNonZeroExitIsUpstreamError(t *testing.T) {
	_, res, err := collectLines(t, helper(t, "fail", "3", "boom"))
	if err == nil {
		t.Fatal("want an error for a non-zero exit")
	}
	e := ir.AsError(err)
	if e.Code != ir.CodeUpstreamError {
		t.Errorf("code = %s, want upstream_error", e.Code)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(e.Detail, "boom") {
		t.Errorf("stderr tail should reach the caller, got %q", e.Detail)
	}
}

func TestMissingBinaryIsUnavailable(t *testing.T) {
	_, _, err := collectLines(t, Spec{Binary: "definitely-not-a-real-binary-xyz", Env: Environ(nil, nil)})
	if err == nil {
		t.Fatal("want an error for a missing binary")
	}
	if e := ir.AsError(err); e.Code != ir.CodeUpstreamUnavailable {
		t.Errorf("code = %s, want upstream_unavailable", e.Code)
	}
}

func TestRequestTimeout(t *testing.T) {
	spec := helper(t, "sleep", "30s")
	spec.RequestTimeout = 200 * time.Millisecond
	start := time.Now()
	_, _, err := collectLines(t, spec)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if e := ir.AsError(err); e.Code != ir.CodeTimeout {
		t.Errorf("code = %s, want timeout", e.Code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; the process was not killed promptly", elapsed)
	}
}

func TestIdleTimeout(t *testing.T) {
	// Emits one line, then goes quiet: the idle watchdog must fire even though
	// the absolute deadline is far away.
	spec := helper(t, "emit-then-sleep", "alive", "30s")
	spec.IdleTimeout = 300 * time.Millisecond
	spec.RequestTimeout = 30 * time.Second
	start := time.Now()
	lines, _, err := collectLines(t, spec)
	if err == nil {
		t.Fatal("want an idle timeout error")
	}
	if e := ir.AsError(err); e.Code != ir.CodeTimeout {
		t.Errorf("code = %s, want timeout", e.Code)
	}
	if len(lines) != 1 {
		t.Errorf("lines = %v, want the one line emitted before going quiet", lines)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; the idle watchdog did not fire", elapsed)
	}
}

// A timeout is a configuration answer as much as a failure: the message has to
// name the knob to turn.
func TestTimeoutMessagesNameTheKnob(t *testing.T) {
	cases := []struct {
		name   string
		spec   func() Spec
		wantIn []string
	}{
		{
			name: "idle",
			spec: func() Spec {
				s := helper(t, "emit-then-sleep", "alive", "30s")
				s.Adapter = "claude-code"
				s.IdleTimeout = 200 * time.Millisecond
				return s
			},
			wantIn: []string{"server.idle_timeout"},
		},
		{
			name: "absolute",
			spec: func() Spec {
				s := helper(t, "sleep", "30s")
				s.Adapter = "codex"
				s.RequestTimeout = 200 * time.Millisecond
				return s
			},
			wantIn: []string{"server.request_timeout"},
		},
		{
			name: "no adapter",
			spec: func() Spec {
				s := helper(t, "sleep", "30s")
				s.RequestTimeout = 200 * time.Millisecond
				return s
			},
			wantIn: []string{"server.request_timeout"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := collectLines(t, tc.spec())
			if err == nil {
				t.Fatal("want a timeout error")
			}
			e := ir.AsError(err)
			if e.Code != ir.CodeTimeout {
				t.Fatalf("code = %s, want timeout", e.Code)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(e.Message, want) {
					t.Errorf("message %q should name %q", e.Message, want)
				}
			}
		})
	}
}

// The watchdog has to measure silence, not elapsed time. That property is
// checked deterministically in TestWatchIdleMeasuresSilenceNotElapsedTime; what
// this test adds is that the runner actually wires a real subprocess's output to
// it, so the margins here are deliberately loose.
//
// They have to be: the helper is this test binary, and under -race it needs a
// full second just to start. A tighter version of this test passed normally and
// failed under -race — a flake, not a finding.
func TestAChattyProcessIsNotKilled(t *testing.T) {
	const (
		ticks    = 15
		interval = 100 * time.Millisecond
	)
	spec := helper(t, "tick", strconv.Itoa(ticks), interval.String())
	spec.IdleTimeout = 3 * time.Second
	lines, _, err := collectLines(t, spec)
	if err != nil {
		t.Fatalf("a chatty process must not be killed: %v", err)
	}
	if len(lines) != ticks {
		t.Errorf("lines = %d, want %d", len(lines), ticks)
	}
}

func TestContextCancellationKillsTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spec := helper(t, "emit-then-sleep", "started", "30s")
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Run(ctx, spec, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("want an error after cancellation")
	}
	if e := ir.AsError(err); e.Code != ir.CodeCanceled {
		t.Errorf("code = %s, want canceled", e.Code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; the process outlived its context", elapsed)
	}
}

func TestOnLineErrorStopsTheRun(t *testing.T) {
	sentinel := errors.New("stop here")
	var seen int
	_, err := Run(context.Background(), helper(t, "flood", "100"), func([]byte) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the parser's error", err)
	}
	if seen != 1 {
		t.Errorf("parser called %d times after returning an error", seen)
	}
}

func TestStderrTailIsBounded(t *testing.T) {
	spec := helper(t, "noise", "5000")
	spec.StderrTailBytes = 512
	_, res, err := collectLines(t, spec)
	if err == nil {
		t.Fatal("want an error for the non-zero exit")
	}
	if len(res.Stderr) > 700 {
		t.Errorf("stderr tail is %d bytes, want it bounded near 512", len(res.Stderr))
	}
	if !strings.Contains(res.Stderr, "5000") {
		t.Errorf("the tail should keep the most recent output, got %q", res.Stderr)
	}
}

func TestEnvironIsAnAllowlist(t *testing.T) {
	t.Setenv("SOME_UNRELATED_SECRET", "leaked")
	t.Setenv("ANTHROPIC_TEST_TOKEN", "vendor")
	env := Environ([]string{"ANTHROPIC_"}, map[string]string{"EXTRA": "set"})

	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "SOME_UNRELATED_SECRET") {
		t.Error("unrelated environment variables must not reach the CLI")
	}
	if !strings.Contains(joined, "ANTHROPIC_TEST_TOKEN=vendor") {
		t.Error("allowed vendor prefixes should pass through")
	}
	if !strings.Contains(joined, "EXTRA=set") {
		t.Error("explicit extras should be set")
	}
	if !strings.Contains(joined, "PATH=") {
		t.Error("PATH is required for the CLI to run")
	}
}

func TestScratchDirIsEmptyAndRemovable(t *testing.T) {
	root := t.TempDir()
	dir, cleanup, err := ScratchDir(root, "test-")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch dir is not empty: %v", entries)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch dir survived cleanup: %v", err)
	}
}

func TestRunUsesTheScratchDirAsCwd(t *testing.T) {
	dir, cleanup, err := ScratchDir("", "test-")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	spec := helper(t, "pwd")
	spec.Dir = dir
	lines, _, err := collectLines(t, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want the working directory", lines)
	}
	// Compare resolved paths: macOS reports /private/var for /var, and Windows
	// has its own aliases.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("cwd = %s, want %s", got, want)
	}
}

// The whole point of the process-tree handling: a CLI that leaves a worker
// running must not leave it running after the request is over. This is the test
// that used to be unix-only, and the platform it most needed to cover was the
// one it skipped.
func TestProcessTreeIsKilled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "beat")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	_, _ = Run(ctx, helper(t, "spawn-child", marker), func([]byte) error { return nil })

	// Let anything that survived keep writing.
	time.Sleep(200 * time.Millisecond)
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the grandchild never wrote its marker, so this proves nothing: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	after, _ := os.ReadFile(marker)
	if len(after) > len(before) {
		t.Errorf("a grandchild survived cancellation: marker grew from %d to %d bytes",
			len(before), len(after))
	}
}
