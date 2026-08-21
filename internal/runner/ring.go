package runner

import (
	"strings"
	"sync"
)

// ringBuffer is an io.Writer that keeps only the last n bytes written. It backs
// the stderr tail: agent CLIs can be chatty, and only the end is diagnostic.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	n   int
	// overflowed records whether anything was dropped, so the tail can say so.
	overflowed bool
}

func newRingBuffer(n int) *ringBuffer {
	return &ringBuffer{n: n, buf: make([]byte, 0, min(n, 4096))}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.n {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-r.n:]...)
		r.overflowed = true
	}
	return len(p), nil
}

// String returns the retained tail with surrounding whitespace trimmed.
func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := strings.TrimSpace(string(r.buf))
	if r.overflowed && s != "" {
		return "...\n" + s
	}
	return s
}
