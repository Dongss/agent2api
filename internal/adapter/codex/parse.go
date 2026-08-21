package codex

import (
	"encoding/json"
	"strings"

	"github.com/Dongss/agent2api/internal/ir"
)

// streamLine is the subset of `codex exec --json` that agent2api consumes.
// Unknown line types and fields are ignored on purpose: the CLI adds events
// between releases, and an unrecognized event must never break a response.
type streamLine struct {
	Type string `json:"type"`

	// type=thread.started
	ThreadID string `json:"thread_id"`

	// type=item.started | item.updated | item.completed
	Item *item `json:"item"`

	// type=turn.completed
	Usage *usage `json:"usage"`

	// type=turn.failed
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`

	// type=error
	Message string `json:"message"`
}

// item is one thread item. Its Type discriminates the payload: "agent_message"
// and "reasoning" carry Text, "error" carries Message, and the tool-shaped
// kinds (command_execution, file_change, mcp_tool_call, web_search, todo_list)
// are ignored — agent2api runs the CLI with nothing to use them on.
type item struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Message string `json:"message"`
}

type usage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// toIR splits the reported input tokens into fresh and cached parts. Codex
// counts cached tokens inside input_tokens; the IR keeps them apart so every
// frontend can add them back up the way its own dialect does.
func (u *usage) toIR() *ir.Usage {
	if u == nil {
		return nil
	}
	fresh := u.InputTokens - u.CachedInputTokens - u.CacheWriteInputTokens
	if fresh < 0 {
		fresh = 0
	}
	return &ir.Usage{
		InputTokens:              fresh,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CachedInputTokens,
		CacheCreationInputTokens: u.CacheWriteInputTokens,
	}
}

// parser converts JSONL lines into IR events. One parser handles one run.
//
// Codex reports a completed item rather than token deltas, so text arrives in
// whole messages: one event per assistant message instead of a token stream.
type parser struct {
	emit func(ir.Event) bool

	// seen guards against emitting the same item twice, since a run may report
	// an item as started, updated and completed.
	seen map[string]bool
	// sawText records whether any assistant text reached the client.
	sawText bool
	// notes collects the non-fatal complaints the CLI reported during the turn
	// (transient stream retries, for instance). They become the detail of an
	// error only if the run then fails.
	notes []string
	// startup holds the same, from before the turn began: config deprecations
	// and model-metadata warnings. They are usually unrelated to a failure, so
	// they are only reported when there is nothing better to say.
	startup []string
	// turn records that the turn started, which is what separates the two.
	turn bool

	usage *ir.Usage
	// err is set when the CLI itself reports a failed turn.
	err *ir.Error
	// done records that the turn completed.
	done bool
}

func newParser(emit func(ir.Event) bool) *parser {
	return &parser{emit: emit, seen: map[string]bool{}}
}

// Line consumes one stdout line. A line that is not valid JSON is skipped
// rather than fatal: CLIs occasionally print stray diagnostics to stdout.
func (p *parser) Line(b []byte) error {
	var l streamLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil
	}

	switch l.Type {
	case "item.started", "item.updated", "item.completed":
		p.item(l.Item)
	case "turn.started":
		p.turn = true
	case "turn.completed":
		p.done = true
		if l.Usage != nil {
			p.usage = l.Usage.toIR()
		}
	case "turn.failed":
		msg := ""
		if l.Error != nil {
			msg = l.Error.Message
		}
		p.err = classify(msg, p.diagnostics())
	case "error":
		// Not necessarily fatal: the CLI reports reconnect attempts this way,
		// and a run that recovers still completes normally.
		p.note(l.Message)
	}
	return nil
}

func (p *parser) item(it *item) {
	if it == nil || p.seen[it.ID] {
		return
	}
	switch it.Type {
	case "agent_message":
		if it.Text == "" {
			return
		}
		p.seen[it.ID] = true
		if p.sawText {
			// Successive messages are separate paragraphs of one answer.
			p.emit(ir.TextEvent("\n\n"))
		}
		p.sawText = true
		p.emit(ir.TextEvent(it.Text))
	case "reasoning":
		if it.Text == "" {
			return
		}
		p.seen[it.ID] = true
		p.emit(ir.ThinkingEvent(it.Text))
	case "error":
		// A warning the CLI raised without failing the turn.
		p.seen[it.ID] = true
		p.note(it.Message)
	}
}

func (p *parser) note(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	if p.turn {
		p.notes = append(p.notes, msg)
		return
	}
	p.startup = append(p.startup, msg)
}

// diagnostics are the notes worth attaching to a failure: what the CLI said
// during the turn, plus the startup warnings when the turn never got far enough
// to say anything itself.
func (p *parser) diagnostics() []string {
	if len(p.notes) > 0 {
		return p.notes
	}
	return append(append([]string{}, p.notes...), p.startup...)
}

// Finish emits the run's terminal event. An error the CLI described itself
// always wins over the runner's "exited with status 1", because it is more
// specific.
func (p *parser) Finish(runErr error) {
	if p.err != nil {
		p.emit(ir.ErrorEvent(p.err))
		return
	}
	if runErr != nil {
		e := ir.AsError(runErr)
		if e.Code == ir.CodeUpstreamError {
			if better := classify(e.Message, p.diagnostics()); better != nil {
				better.Detail = joinDetail(e.Detail, p.diagnostics())
				e = better
			}
		}
		p.emit(ir.ErrorEvent(e))
		return
	}
	if !p.done {
		p.emit(ir.ErrorEvent(&ir.Error{
			Code:    ir.CodeUpstreamError,
			Message: "the Codex CLI exited before completing the turn",
			Detail:  joinDetail("", append(append([]string{}, p.notes...), p.startup...)),
		}))
		return
	}
	p.emit(ir.Event{Type: ir.EventDone, Usage: p.usage, StopReason: ir.StopEndTurn})
}

// classify turns a failure message into an actionable error. The notes carry
// what the CLI complained about earlier in the run, which is often where the
// real cause is.
func classify(msg string, notes []string) *ir.Error {
	detail := joinDetail(msg, notes)
	haystack := strings.ToLower(detail)

	switch {
	case containsAny(haystack, "not logged in", "please run `codex login`", "please log in",
		"unauthorized", "401", "authentication"):
		return &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Codex CLI is not authenticated; run `codex login` and try again",
			Detail:  detail,
		}
	case containsAny(haystack, "out of credits", "insufficient_quota", "quota", "billing", "payment"):
		return &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Codex account cannot serve requests: it is out of credit or quota",
			Detail:  detail,
		}
	case containsAny(haystack, "rate limit", "rate_limit", "overloaded", "429"):
		return &ir.Error{
			Code:       ir.CodeOverloaded,
			Message:    "the Codex CLI is rate limited",
			Detail:     detail,
			RetryAfter: ir.DefaultRateLimitRetry,
		}
	}
	if strings.TrimSpace(msg) == "" && len(notes) == 0 {
		return nil
	}
	return &ir.Error{Code: ir.CodeUpstreamError, Message: "the Codex CLI failed", Detail: detail}
}

func joinDetail(msg string, notes []string) string {
	parts := make([]string, 0, len(notes)+1)
	if m := strings.TrimSpace(msg); m != "" {
		parts = append(parts, m)
	}
	for _, n := range notes {
		if n != "" && n != strings.TrimSpace(msg) {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, "; ")
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
