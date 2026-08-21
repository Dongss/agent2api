package cursor

import (
	"encoding/json"
	"strings"

	"github.com/Dongss/agent2api/internal/ir"
)

// streamLine is the subset of `cursor-agent --output-format stream-json` that
// agent2api consumes. Unknown line types and fields are ignored on purpose: the
// CLI adds events between releases, and an unrecognized event must never break
// a response.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// type=system,subtype=init
	Model string `json:"model"`

	// type=assistant: an object. Kept raw because other line types spell
	// "message" as a plain string, and one unreadable field must not throw
	// away the rest of the line.
	Message json.RawMessage `json:"message"`

	// type=thinking,subtype=delta
	Text string `json:"text"`

	// type=result
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   *usage `json:"usage"`
}

// assistantMessage is the object an "assistant" line carries.
type assistantMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

func (u *usage) toIR() *ir.Usage {
	if u == nil {
		return nil
	}
	return &ir.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
}

// assistantText returns the text an "assistant" line carries.
func (l *streamLine) assistantText() string {
	var msg assistantMessage
	if err := json.Unmarshal(l.Message, &msg); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range msg.Content {
		if part.Type == "text" || part.Type == "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// note returns whatever explanation a line carries, for a diagnostic message.
// The CLI is not documented to emit a top-level error event, so this stays
// tolerant of either spelling rather than assuming one.
func (l *streamLine) note() string {
	var s string
	if err := json.Unmarshal(l.Message, &s); err == nil && s != "" {
		return s
	}
	if l.Result != "" {
		return l.Result
	}
	return l.Text
}

// parser converts stream-json lines into IR events. One parser per run.
//
// Beyond translation it has one job: cursor-agent emits its answer twice. Under
// --stream-partial-output each token is an "assistant" line, and the CLI also
// flushes everything since the last boundary (tool call, retry, result) as one
// more such line just before it. Concatenating blindly would duplicate the
// answer.
//
// The flush is identified, not guessed: it repeats the accumulated text and
// always precedes a boundary. So a line matching the accumulation is held for
// one line — dropped if a boundary follows, released otherwise. A run without
// the flag is unaffected, since there every line is a fresh chunk.
type parser struct {
	emit func(ir.Event) bool

	// pending is the text emitted since the last boundary, which is exactly
	// what a flush line would repeat.
	pending string
	// held is a line that might be a flush, waiting for the next line to tell.
	held    string
	hasHeld bool

	// sawText records whether any assistant text reached the client, which
	// decides whether the terminal line's full result is a duplicate or the
	// only copy.
	sawText   bool
	finalText string
	usage     *ir.Usage
	model     string
	notes     []string
	err       *ir.Error
	done      bool
}

func newParser(emit func(ir.Event) bool) *parser {
	return &parser{emit: emit}
}

// Line consumes one stdout line. A line that is not valid JSON is skipped
// rather than fatal: CLIs occasionally print stray diagnostics to stdout.
func (p *parser) Line(b []byte) error {
	var l streamLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil
	}

	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.Model != "" {
			p.model = l.Model
		}
	case "thinking":
		if l.Subtype == "delta" && l.Text != "" {
			p.emit(ir.ThinkingEvent(l.Text))
		}
	case "assistant":
		p.assistant(l.assistantText())
	case "tool_call", "retry", "connection", "interaction_query":
		// A boundary: the CLI flushed right before it, so anything held back
		// was that flush.
		p.boundary()
	case "result":
		p.boundary()
		p.result(l)
	case "error":
		p.note(l.note())
	}
	return nil
}

// assistant handles one assistant line: release whatever was held, then decide
// whether this line is text or the flush that repeats it.
func (p *parser) assistant(text string) {
	if text == "" {
		return
	}
	p.release()
	if text == p.pending {
		p.held, p.hasHeld = text, true
		return
	}
	p.write(text)
}

// release emits a held line, which the next line proved to be real text.
func (p *parser) release() {
	if !p.hasHeld {
		return
	}
	text := p.held
	p.held, p.hasHeld = "", false
	p.write(text)
}

// boundary discards a held flush and starts a fresh accumulation window: the
// CLI resets its own buffer at exactly these points.
func (p *parser) boundary() {
	p.held, p.hasHeld = "", false
	p.pending = ""
}

func (p *parser) write(text string) {
	p.sawText = true
	p.pending += text
	p.emit(ir.TextEvent(text))
}

func (p *parser) note(parts ...string) {
	for _, s := range parts {
		if s = strings.TrimSpace(s); s != "" {
			p.notes = append(p.notes, s)
		}
	}
}

func (p *parser) result(l streamLine) {
	p.done = true
	if l.Usage != nil {
		p.usage = l.Usage.toIR()
	}
	p.finalText = l.Result

	if !l.IsError && (l.Subtype == "success" || l.Subtype == "") {
		return
	}
	p.note(l.Result)
	p.err = classify("the Cursor CLI reported "+orDefault(l.Subtype, "an error"), p.notes)
}

// Finish emits the run's terminal event. An error the CLI described itself
// always wins over the runner's "exited with status 1", because it is more
// specific.
func (p *parser) Finish(runErr error) {
	// Nothing more is coming, so a line still held back was real text after all.
	p.release()

	if p.err != nil {
		p.emit(ir.ErrorEvent(p.err))
		return
	}
	if runErr != nil {
		e := ir.AsError(runErr)
		if e.Code == ir.CodeUpstreamError {
			e = classify(e.Message, append(p.notes, e.Detail))
		}
		p.emit(ir.ErrorEvent(e))
		return
	}
	if !p.done {
		p.emit(ir.ErrorEvent(&ir.Error{
			Code:    ir.CodeUpstreamError,
			Message: "the Cursor CLI exited before reporting a result",
			Detail:  strings.Join(p.notes, "; "),
		}))
		return
	}
	// A CLI that streamed nothing still reports the whole answer on the
	// terminal line, which is then the only copy.
	if !p.sawText && p.finalText != "" {
		p.emit(ir.TextEvent(p.finalText))
	}
	p.emit(ir.Event{Type: ir.EventDone, Usage: p.usage, StopReason: ir.StopEndTurn, Model: p.model})
}

// classify turns a failure into an actionable error.
func classify(message string, notes []string) *ir.Error {
	detail := strings.TrimSpace(strings.Join(compact(notes), "; "))
	haystack := strings.ToLower(message + " " + detail)

	switch {
	case containsAny(haystack, "authentication required", "not logged in", "log in", "login",
		"unauthorized", "api key is invalid", "401"):
		return &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Cursor CLI is not authenticated; run `cursor-agent login` and try again",
			Detail:  detail,
		}
	case containsAny(haystack, "rate limit", "rate_limit", "overloaded", "too many requests", "429"):
		return &ir.Error{
			Code:       ir.CodeOverloaded,
			Message:    "the Cursor CLI is rate limited",
			Detail:     detail,
			RetryAfter: ir.DefaultRateLimitRetry,
		}
	case containsAny(haystack, "usage limit", "out of credits", "quota", "billing", "subscription"):
		return &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Cursor account cannot serve requests: it is out of quota or has no active plan",
			Detail:  detail,
		}
	}
	return &ir.Error{Code: ir.CodeUpstreamError, Message: message, Detail: detail}
}

func compact(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
