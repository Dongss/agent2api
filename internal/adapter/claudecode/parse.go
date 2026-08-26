package claudecode

import (
	"encoding/json"
	"strings"

	"github.com/Dongss/agent2api/internal/ir"
)

// streamLine is the subset of Claude Code's `--output-format stream-json`
// protocol that agent2api consumes. Unknown line types and fields are ignored
// on purpose: the CLI adds events between releases, and an unrecognized event
// must never break a response.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// type=stream_event: a native Anthropic streaming event.
	Event *streamEvent `json:"event"`

	// type=system,subtype=init and type=result.
	Model string `json:"model"`

	// type=result: the terminal line of every run.
	IsError        bool            `json:"is_error"`
	Result         string          `json:"result"`
	StopReason     string          `json:"stop_reason"`
	Usage          *usage          `json:"usage"`
	APIErrorStatus json.RawMessage `json:"api_error_status"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string `json:"model"`
		Usage *usage `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Thinking   string `json:"thinking"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// The CLI reports thinking tokens in a nested object, under a name of its
	// own; ir calls the same quantity reasoning tokens.
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (u *usage) toIR() *ir.Usage {
	if u == nil {
		return nil
	}
	return &ir.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		ReasoningOutputTokens:    u.OutputTokensDetails.ThinkingTokens,
	}
}

// parser converts stream-json lines into IR events. One parser handles one run.
type parser struct {
	emit func(ir.Event) bool

	// sawText records whether any text delta reached the client, which decides
	// whether the terminal line's full result is a duplicate or a fallback.
	sawText bool
	// finalText is the complete assistant message reported on the result line.
	finalText  string
	usage      *ir.Usage
	stopReason ir.StopReason
	model      string
	// err is set when the CLI itself reports a failed run.
	err *ir.Error
	// done records that the terminal result line arrived.
	done bool
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
	case "stream_event":
		p.streamEvent(l.Event)
	case "result":
		p.result(l)
	}
	return nil
}

func (p *parser) streamEvent(ev *streamEvent) {
	if ev == nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Model != "" {
			p.model = ev.Message.Model
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return
			}
			p.sawText = true
			p.emit(ir.TextEvent(ev.Delta.Text))
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				return
			}
			p.emit(ir.ThinkingEvent(ev.Delta.Thinking))
		}
	case "message_delta":
		if ev.Usage != nil {
			p.usage = ev.Usage.toIR()
		}
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			p.stopReason = ir.StopReason(ev.Delta.StopReason)
		}
	}
}

func (p *parser) result(l streamLine) {
	p.done = true
	if l.Usage != nil {
		p.usage = l.Usage.toIR()
	}
	if l.StopReason != "" {
		p.stopReason = ir.StopReason(l.StopReason)
	}
	if l.Model != "" {
		p.model = l.Model
	}
	p.finalText = l.Result

	if !l.IsError && (l.Subtype == "success" || l.Subtype == "") {
		return
	}
	p.err = classifyResult(l)
}

// Finish emits the run's terminal event. runErr is whatever the runner reported;
// an error the CLI described itself always wins, because it is more specific
// than "exited with status 1".
func (p *parser) Finish(runErr error) {
	if p.err != nil {
		p.emit(ir.ErrorEvent(p.err))
		return
	}
	if runErr != nil {
		e := ir.AsError(runErr)
		if e.Code == ir.CodeUpstreamError {
			if auth := authHint(e.Detail); auth != nil {
				e = auth
			}
		}
		p.emit(ir.ErrorEvent(e))
		return
	}
	if !p.done {
		p.emit(ir.ErrorEvent(&ir.Error{
			Code:    ir.CodeUpstreamError,
			Message: "the Claude Code CLI exited before reporting a result",
		}))
		return
	}
	// Without token-level deltas (older CLIs, or a run that produced its answer
	// in one shot) the terminal line is the only place the text appears.
	if !p.sawText && p.finalText != "" {
		p.emit(ir.TextEvent(p.finalText))
	}
	stop := p.stopReason
	if stop == "" {
		stop = ir.StopEndTurn
	}
	p.emit(ir.Event{Type: ir.EventDone, Usage: p.usage, StopReason: stop, Model: p.model})
}

// classifyResult turns a failed result line into an actionable error.
func classifyResult(l streamLine) *ir.Error {
	msg := strings.TrimSpace(l.Result)
	if msg == "" {
		msg = "the Claude Code CLI reported " + orDefault(l.Subtype, "an error")
	}
	if auth := authHint(msg); auth != nil {
		return auth
	}
	if isRateLimit(msg) {
		return &ir.Error{
			Code:    ir.CodeOverloaded,
			Message: "the Claude Code CLI is rate limited",
			Detail:  msg,
		}
	}
	switch l.Subtype {
	case "error_max_turns":
		return &ir.Error{Code: ir.CodeUpstreamError, Message: "the Claude Code CLI hit its turn limit", Detail: msg}
	}
	return &ir.Error{Code: ir.CodeUpstreamError, Message: "the Claude Code CLI failed", Detail: msg}
}

// authHint recognizes the ways the CLI says "you are not logged in", so the
// caller gets a 503 telling them what to run instead of an opaque 502.
func authHint(text string) *ir.Error {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	needles := []string{
		"not logged in", "log in", "login", "unauthorized", "authentication_error",
		"invalid api key", "oauth", "credit balance", "please run /login",
	}
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return &ir.Error{
				Code:    ir.CodeUpstreamUnavailable,
				Message: "the Claude Code CLI is not authenticated; run `claude auth login` and try again",
				Detail:  strings.TrimSpace(text),
			}
		}
	}
	return nil
}

func isRateLimit(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "overloaded")
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
