// Package runner owns agent CLI subprocess lifecycle: spawning, streaming
// stdout line by line, capturing a bounded stderr tail, enforcing idle and
// absolute deadlines, and making sure nothing outlives the request.
//
// Adapters do not touch os/exec directly; they only build argv and parse lines.
package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Dongss/agent2api/internal/ir"
)

// Defaults for the size limits. They are generous on purpose: a single
// stream-json line can carry a whole assistant message.
const (
	DefaultMaxLineBytes    = 16 << 20 // 16 MiB
	DefaultStderrTailBytes = 8 << 10  // 8 KiB
)

// Spec describes one subprocess run.
type Spec struct {
	// Binary is a command name resolved through PATH, or an absolute path.
	Binary string
	Args   []string
	// Adapter is the adapter id this run belongs to, used to name the config
	// keys in a timeout message. Empty is fine for calls that are not a
	// request, like probing an install.
	Adapter string
	// Dir is the working directory. Callers should point this at an empty
	// scratch directory so the CLI has nothing real within reach.
	Dir string
	// Env is the complete environment for the child; nothing is inherited
	// implicitly. Build it with [Environ].
	Env []string
	// Stdin is written to the child's stdin, which is then closed. Passing the
	// prompt this way avoids OS argv length limits on long conversations.
	Stdin string

	// RequestTimeout is the absolute cap on the run. Zero means no cap.
	RequestTimeout time.Duration
	// IdleTimeout kills the child when it has produced no output line for this
	// long. Zero disables the watchdog.
	IdleTimeout time.Duration

	MaxLineBytes    int
	StderrTailBytes int
}

// Result reports how the child ended. It is filled in even when Run returns an
// error, so callers can inspect the CLI's own complaint.
type Result struct {
	ExitCode int
	// Stderr holds the last StderrTailBytes bytes the child wrote to stderr.
	Stderr string
}

var (
	errIdleTimeout    = errors.New("no output from the agent CLI")
	errRequestTimeout = errors.New("request deadline exceeded")
)

// Run starts the process and calls onLine for every line the child writes to
// stdout, in order, from a single goroutine. Run returns once the child has
// exited and all output has been consumed.
//
// If onLine returns an error, the child is killed and that error is returned.
// Infrastructure failures (missing binary, timeout, non-zero exit) come back as
// [*ir.Error] so frontends can map them to a status without re-classifying.
func Run(ctx context.Context, spec Spec, onLine func([]byte) error) (Result, error) {
	var res Result

	maxLine := spec.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	tailBytes := spec.StderrTailBytes
	if tailBytes <= 0 {
		tailBytes = DefaultStderrTailBytes
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	if spec.RequestTimeout > 0 {
		t := time.AfterFunc(spec.RequestTimeout, func() { cancel(errRequestTimeout) })
		defer t.Stop()
	}

	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	// exec.CommandContext's default cancel closes only the process; agent CLIs
	// spawn children of their own, so take the whole tree down instead.
	tree := newProcTree()
	defer tree.close()
	tree.prepare(cmd)
	cmd.Cancel = func() error { return tree.kill(cmd) }
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, ir.Errorf(ir.CodeInternal, "cannot pipe stdout of %s: %v", spec.Binary, err)
	}
	stderrTail := newRingBuffer(tailBytes)
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return res, &ir.Error{
				Code:    ir.CodeUpstreamUnavailable,
				Message: "agent CLI " + spec.Binary + " was not found on PATH",
				Err:     err,
			}
		}
		return res, &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "cannot start agent CLI " + spec.Binary,
			Detail:  err.Error(),
			Err:     err,
		}
	}

	// A child that could not be grouped still runs; the only loss is that a
	// cancellation reclaims it alone rather than its whole tree, which is what
	// any caller could do anyway. Not worth failing a request over.
	_ = tree.adopt(cmd)

	activity := newActivityClock()
	var watchdog sync.WaitGroup
	stopIdle := make(chan struct{})
	// Must run before watchdog.Wait, and exactly once.
	stopWatchdog := sync.OnceFunc(func() {
		close(stopIdle)
		watchdog.Wait()
	})
	defer stopWatchdog()
	if spec.IdleTimeout > 0 {
		watchdog.Add(1)
		go func() {
			defer watchdog.Done()
			watchIdle(stopIdle, activity, spec.IdleTimeout, func() { cancel(errIdleTimeout) })
		}()
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
	var lineErr error
	for scanner.Scan() {
		activity.mark()
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if err := onLine(line); err != nil {
			lineErr = err
			cancel(err)
			break
		}
	}
	if lineErr == nil && scanner.Err() != nil && !errors.Is(scanner.Err(), io.ErrClosedPipe) {
		if errors.Is(scanner.Err(), bufio.ErrTooLong) {
			lineErr = ir.Errorf(ir.CodeUpstreamError,
				"agent CLI %s emitted a line larger than %d bytes", spec.Binary, maxLine)
		}
	}
	// Drain anything left so the child never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)

	waitErr := cmd.Wait()
	stopWatchdog()

	res.Stderr = stderrTail.String()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if lineErr != nil {
		return res, lineErr
	}

	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errIdleTimeout):
		return res, &ir.Error{
			Code: ir.CodeTimeout,
			Message: "the agent CLI produced no output for " + spec.IdleTimeout.String() +
				" and was stopped; " + raise(spec.Adapter, "idle_timeout"),
			Detail: res.Stderr,
		}
	case errors.Is(cause, errRequestTimeout):
		return res, &ir.Error{
			Code: ir.CodeTimeout,
			Message: "the agent CLI did not finish within " + spec.RequestTimeout.String() +
				" and was stopped; " + raise(spec.Adapter, "request_timeout"),
			Detail: res.Stderr,
		}
	case errors.Is(cause, context.Canceled):
		return res, &ir.Error{Code: ir.CodeCanceled, Message: "request canceled", Err: cause}
	}

	if waitErr != nil {
		var exitErr *exec.ExitError
		reason := waitErr.Error()
		if errors.As(waitErr, &exitErr) {
			reason = exitErr.String() // "exit status 1", "signal: killed"
		}
		return res, &ir.Error{
			Code:    ir.CodeUpstreamError,
			Message: "agent CLI " + spec.Binary + " failed: " + reason,
			Detail:  res.Stderr,
			Err:     waitErr,
		}
	}
	return res, nil
}

// raise names the configuration keys that control a deadline, so the message
// says what to change rather than only what went wrong.
func raise(adapter, key string) string {
	if adapter == "" {
		return "raise server." + key + " if this is expected to take longer"
	}
	return "raise server." + key + " if this is expected to take longer"
}

// activityClock records when the child last produced output.
type activityClock struct {
	mu   sync.Mutex
	last time.Time
}

func newActivityClock() *activityClock {
	return &activityClock{last: time.Now()}
}

func (a *activityClock) mark() {
	a.mu.Lock()
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *activityClock) idleFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.last)
}

func watchIdle(stop <-chan struct{}, a *activityClock, limit time.Duration, fire func()) {
	// Poll at a fraction of the limit so the kill lands close to the deadline
	// without a timer per line.
	interval := limit / 4
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if a.idleFor() >= limit {
				fire()
				return
			}
		}
	}
}
