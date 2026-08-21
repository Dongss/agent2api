//go:build conformance

package mock

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/ir"
)

// ID is the adapter id used to namespace this backend's models.
const ID = "mock"

func init() {
	adapter.Register(ID, New)
	// Without this, a config naming the mock adapter is rejected as unknown.
	config.RegisterAdapterID(ID)
}

// DefaultVariant is the script a request for the bare adapter id gets, standing
// in for "whatever model the CLI would pick on its own".
const DefaultVariant = "ok"

// SlowDelay is how long the "slow" variant stays quiet before answering. It is
// meant to be longer than a test's configured heartbeat interval, so keepalives
// are exercised end to end.
const SlowDelay = 2500 * time.Millisecond

// script is one scripted outcome.
type script struct {
	// thinking and text are emitted as several deltas each, so streaming
	// clients see a sequence rather than one blob.
	thinking string
	text     string
	// delay is slept before any output, as a backend that is thinking does.
	delay time.Duration
	// fail, when set, ends the run instead of completing it. With text set as
	// well, it arrives after the text, which is what forces a frontend to
	// report the failure inside an already-committed stream.
	fail *ir.Error
	// truncate ends the run with no terminal event at all, as a CLI killed
	// mid-answer does.
	truncate bool
	usage    *ir.Usage
}

var scripts = map[string]script{
	"ok": {
		text:  "Mock reply: ok.",
		usage: &ir.Usage{InputTokens: 11, OutputTokens: 5, CacheReadInputTokens: 3},
	},
	"thinking": {
		thinking: "Considering the question carefully.",
		text:     "Mock reply: thought about it.",
		usage:    &ir.Usage{InputTokens: 12, OutputTokens: 9},
	},
	"empty": {
		usage: &ir.Usage{InputTokens: 7, OutputTokens: 0},
	},
	"slow": {
		delay: SlowDelay,
		text:  "Mock reply: sorry for the wait.",
		usage: &ir.Usage{InputTokens: 9, OutputTokens: 7},
	},
	"unauthenticated": {
		fail: &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the mock CLI is not logged in; run `mock login` and try again",
			Detail:  "mock: no credentials found",
		},
	},
	"rate-limited": {
		fail: &ir.Error{
			Code:       ir.CodeOverloaded,
			Message:    "the mock CLI is rate limited",
			Detail:     "mock: slow down",
			RetryAfter: ir.DefaultRateLimitRetry,
		},
	},
	"crash": {
		fail: &ir.Error{
			Code:    ir.CodeUpstreamError,
			Message: "the mock CLI failed",
			Detail:  "mock: signal: killed",
		},
	},
	"timeout": {
		fail: &ir.Error{
			Code:    ir.CodeTimeout,
			Message: "the agent CLI produced no output for 2m0s and was stopped; raise server.idle_timeout if this is expected to take longer",
		},
	},
	"truncated": {
		truncate: true,
	},
	"mid-stream-failure": {
		text: "Mock reply: this answer stops halfway ",
		fail: &ir.Error{
			Code:    ir.CodeUpstreamError,
			Message: "the mock CLI failed halfway through",
			Detail:  "mock: exit status 1",
		},
	},
}

// Variants lists the scripted model variants, for a config file or a test.
func Variants() []string {
	out := make([]string, 0, len(scripts))
	for name := range scripts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Adapter is the scripted backend.
type Adapter struct {
	opts adapter.Options
	log  *slog.Logger
}

// New builds the adapter from its resolved configuration.
func New(opts adapter.Options) (adapter.Adapter, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{opts: opts, log: log.With("adapter", ID)}, nil
}

// ID implements adapter.Adapter.
func (a *Adapter) ID() string { return ID }

// Probe implements adapter.Adapter. There is nothing to install, so the mock is
// always healthy — which is what lets `doctor` be exercised too.
func (a *Adapter) Probe(context.Context) (adapter.Health, error) {
	return adapter.Health{
		Binary:  "(built in)",
		Version: "mock 1.0",
		Account: "scripted",
		Notes:   []string{"scripted backend for conformance testing; it never talks to a model"},
	}, nil
}

// Run plays back the script the requested variant names.
func (a *Adapter) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	variant := req.Variant
	if variant == "" {
		variant = DefaultVariant
	}
	s, ok := scripts[variant]
	if !ok {
		return nil, ir.InvalidRequest("model", "the mock backend has no script for %q; known variants are %s",
			variant, strings.Join(Variants(), ", "))
	}

	events := make(chan ir.Event, 16)
	go func() {
		defer close(events)
		emit := func(e ir.Event) bool {
			select {
			case events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !emit(ir.Event{Type: ir.EventStart, Model: req.Model}) {
			return
		}
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-ctx.Done():
				return
			}
		}
		for _, part := range chunks(s.thinking) {
			if !emit(ir.ThinkingEvent(part)) {
				return
			}
		}
		for _, part := range chunks(s.text) {
			if !emit(ir.TextEvent(part)) {
				return
			}
		}
		switch {
		case s.truncate:
			return
		case s.fail != nil:
			emit(ir.ErrorEvent(s.fail))
		default:
			emit(ir.Event{
				Type:       ir.EventDone,
				Usage:      s.usage,
				StopReason: ir.StopEndTurn,
				Model:      req.Model,
			})
		}
	}()
	return events, nil
}

// chunks splits text into word-sized deltas, so a streaming client has more
// than one frame to accumulate.
func chunks(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, word := range strings.SplitAfter(text, " ") {
		if word != "" {
			out = append(out, word)
		}
	}
	return out
}
