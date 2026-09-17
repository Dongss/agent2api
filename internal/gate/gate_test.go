package gate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

// blocking is an adapter whose runs stay open until released, so the gate's
// limit can be observed directly.
type blocking struct {
	*fake.Adapter
	release  chan struct{}
	inFlight atomic.Int32
	peak     atomic.Int32
}

func newBlocking() *blocking {
	return &blocking{Adapter: fake.New("blocking", "ok", "m"), release: make(chan struct{})}
}

func (b *blocking) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	now := b.inFlight.Add(1)
	for {
		peak := b.peak.Load()
		if now <= peak || b.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	events := make(chan ir.Event, 4)
	go func() {
		defer close(events)
		defer b.inFlight.Add(-1)
		select {
		case <-b.release:
		case <-ctx.Done():
			return
		}
		events <- ir.Event{Type: ir.EventDone}
	}()
	return events, nil
}

func drain(events <-chan ir.Event) {
	for range events {
	}
}

func TestConcurrencyLimit(t *testing.T) {
	backend := newBlocking()
	g := Wrap(backend, 2, time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, err := g.Run(context.Background(), ir.Request{})
			if err != nil {
				t.Errorf("Run: %v", err)
				return
			}
			drain(events)
		}()
	}

	// Give both runs time to occupy their slots.
	deadline := time.Now().Add(2 * time.Second)
	for backend.inFlight.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := backend.inFlight.Load(); got != 2 {
		t.Fatalf("in flight = %d, want 2", got)
	}

	// A third must not start while both slots are held.
	third := make(chan error, 1)
	go func() {
		_, err := g.Run(context.Background(), ir.Request{})
		third <- err
	}()
	select {
	case err := <-third:
		t.Fatalf("third run was admitted immediately: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(backend.release)
	wg.Wait()
	if err := <-third; err != nil {
		t.Errorf("third run should have been admitted once a slot freed: %v", err)
	}
	if got := backend.peak.Load(); got > 2 {
		t.Errorf("peak concurrency = %d, limit was 2", got)
	}
}

func TestQueueTimeoutIs429(t *testing.T) {
	backend := newBlocking()
	g := Wrap(backend, 1, 50*time.Millisecond)

	events, err := g.Run(context.Background(), ir.Request{})
	if err != nil {
		t.Fatal(err)
	}
	// Release first, then drain: draining a run that is still blocked would
	// wait forever.
	defer func() { close(backend.release); drain(events) }()

	start := time.Now()
	_, err = g.Run(context.Background(), ir.Request{})
	if err == nil {
		t.Fatal("want an error once the queue times out")
	}
	if e := ir.AsError(err); e.Code != ir.CodeOverloaded {
		t.Errorf("code = %s, want overloaded", e.Code)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Errorf("gave up after %v, should have waited for the queue timeout", waited)
	}
}

func TestSlotIsReleasedAfterRun(t *testing.T) {
	backend := fake.New("fakecli", "hello", "m")
	g := Wrap(backend, 1, 100*time.Millisecond)
	// Run more times than the limit, sequentially: each must succeed, which it
	// can only do if the previous run gave its slot back.
	for i := 0; i < 5; i++ {
		events, err := g.Run(context.Background(), ir.Request{})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		drain(events)
	}
}

func TestZeroLimitDisablesGating(t *testing.T) {
	backend := fake.New("fakecli", "hello", "m")
	if got := Wrap(backend, 0, time.Second); got != adapter.Adapter(backend) {
		t.Error("a limit below 1 should return the adapter unwrapped")
	}
}

func TestCanceledWhileQueued(t *testing.T) {
	backend := newBlocking()
	g := Wrap(backend, 1, time.Minute)

	events, err := g.Run(context.Background(), ir.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(backend.release); drain(events) }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := g.Run(ctx, ir.Request{}); ir.AsError(err).Code != ir.CodeCanceled {
		t.Errorf("want a canceled error, got %v", err)
	}
}

// The gate embeds the Adapter interface, which promotes only the methods that
// interface declares. An optional capability must be forwarded explicitly, or a
// backend that can hold a schema reports that it cannot once wrapped.
func TestWrapForwardsSchemaCapability(t *testing.T) {
	backend := fake.New("fake", "reply")
	backend.Schema = true

	wrapped := Wrap(backend, 2, time.Second)
	e, ok := wrapped.(adapter.SchemaEnforcer)
	if !ok {
		t.Fatal("the wrapped adapter no longer reports the capability at all")
	}
	if !e.EnforcesSchema(context.Background()) {
		t.Error("EnforcesSchema = false through the gate, true without it")
	}

	backend.Schema = false
	if e.EnforcesSchema(context.Background()) {
		t.Error("EnforcesSchema = true for a backend that says it cannot")
	}
}

// The gate embeds the Adapter interface, so every optional capability needs
// forwarding of its own; #14 found this the hard way with schemas.
func TestWrapForwardsEffortLevels(t *testing.T) {
	backend := fake.New("fake", "reply")
	backend.Efforts = []ir.Effort{ir.EffortLow, ir.EffortMax}

	wrapped := Wrap(backend, 2, time.Second)
	e, ok := wrapped.(adapter.EffortSetter)
	if !ok {
		t.Fatal("the wrapped adapter no longer reports the capability at all")
	}
	if got := e.EffortLevels(context.Background()); len(got) != 2 {
		t.Errorf("EffortLevels = %v through the gate, want both levels", got)
	}
}
