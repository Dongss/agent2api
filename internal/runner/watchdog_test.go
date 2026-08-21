package runner

import (
	"testing"
	"time"
)

// The idle watchdog exists to kill a CLI that has stopped talking, and it must
// not kill one that is merely taking a long time. Both halves are checked here
// rather than through a subprocess: driving the clock directly is exact, while
// a real child's startup time is not, and that difference is the whole reason
// the subprocess version of this test was once flaky.
func TestWatchIdleMeasuresSilenceNotElapsedTime(t *testing.T) {
	const limit = 200 * time.Millisecond

	clock := newActivityClock()
	fired := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)

	go watchIdle(stop, clock, limit, func() { fired <- struct{}{} })

	// Keep reporting activity for several times the limit.
	deadline := time.Now().Add(4 * limit)
	for time.Now().Before(deadline) {
		clock.mark()
		time.Sleep(limit / 10)
	}
	select {
	case <-fired:
		t.Fatal("the watchdog fired while output was still flowing")
	default:
	}

	// Stop reporting: now it must fire.
	select {
	case <-fired:
	case <-time.After(20 * limit):
		t.Fatal("the watchdog never fired after the output stopped")
	}
}

// A watchdog that has already fired must not fire again, and stopping it must
// end the goroutine — runner.Run waits on it before returning.
func TestWatchIdleStopsWhenTold(t *testing.T) {
	clock := newActivityClock()
	done := make(chan struct{})
	stop := make(chan struct{})

	go func() {
		watchIdle(stop, clock, time.Hour, func() {})
		close(done)
	}()

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not return after being stopped")
	}
}
