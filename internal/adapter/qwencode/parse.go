package qwencode

import (
	"encoding/json"
	"strings"

	"github.com/Dongss/agent2api/internal/ir"
)

// streamLine is the subset of Qwen Code's `--output-format stream-json`
// protocol that agent2api consumes. Unknown line types and fields are ignored
// on purpose: the CLI adds events between releases, and an unrecognized event
// must never break a response.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// type=system,subtype=init.
	Model string `json:"model"`
	// Tools is the tool list the CLI decided to run with. agent2api requires it
	// empty; see [parser.system].
	Tools []string `json:"tools"`

	// type=assistant: a whole message, one line per content block group.
	Message *struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	} `json:"message"`

	// type=result: the terminal line of every run.
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   *usage `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type usage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

func (u *usage) toIR() *ir.Usage {
	if u == nil {
		return nil
	}
	return &ir.Usage{
		InputTokens:          u.InputTokens,
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: u.CacheReadInputTokens,
	}
}

// parser converts stream-json lines into IR events. One parser handles one run.
type parser struct {
	emit func(ir.Event) bool

	// sawText records whether any assistant text reached the client, which
	// decides whether the terminal line's result is a duplicate or a fallback.
	sawText   bool
	finalText string
	usage     *ir.Usage
	model     string
	err       *ir.Error
	done      bool
}

func newParser(emit func(ir.Event) bool) *parser {
	return &parser{emit: emit}
}

// Line consumes one stdout line. A line that is not valid JSON is skipped
// rather than fatal: this CLI prints banners to stdout under some flags, and
// CLIs in general print stray diagnostics there.
func (p *parser) Line(b []byte) error {
	var l streamLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil
	}
	switch l.Type {
	case "system":
		p.system(l)
	case "assistant":
		p.assistant(l)
	case "result":
		p.result(l)
	}
	return nil
}

// system reads the startup line, which is where the containment is checked.
//
// The CLI reports the tools it is running with, and agent2api's settings leave
// it none. Verifying that here rather than trusting the settings is the whole
// guarantee: the deny list those settings carry cannot cover a tool a future
// release adds, and the CLI's own documentation says as much. A run that starts
// with tools available is refused before a word of it reaches the caller.
func (p *parser) system(l streamLine) {
	if l.Subtype != "init" {
		return
	}
	if l.Model != "" {
		p.model = l.Model
	}
	if len(l.Tools) > 0 {
		p.err = &ir.Error{
			Code: ir.CodeUpstreamError,
			Message: "the Qwen Code CLI started with tools available, which agent2api does not allow; " +
				"this build of the CLI needs its tool list re-checked against internal/adapter/qwen",
			Detail: "tools: " + strings.Join(l.Tools, ", "),
		}
	}
}

func (p *parser) assistant(l streamLine) {
	if l.Message == nil {
		return
	}
	for _, c := range l.Message.Content {
		switch c.Type {
		case "text":
			if c.Text == "" {
				continue
			}
			p.sawText = true
			p.emit(ir.TextEvent(c.Text))
		case "thinking":
			if c.Thinking == "" {
				continue
			}
			p.emit(ir.ThinkingEvent(c.Thinking))
		}
	}
}

func (p *parser) result(l streamLine) {
	p.done = true
	if l.Usage != nil {
		p.usage = l.Usage.toIR()
	}
	p.finalText = l.Result

	if !l.IsError {
		// Not the end of it: this CLI reports an upstream API failure as a
		// successful turn whose answer *is* the error. See [apiErrorIn].
		if msg, ok := apiErrorIn(l.Result); ok {
			p.err = classify(msg, l.Subtype)
		}
		return
	}
	msg := ""
	if l.Error != nil {
		msg = l.Error.Message
	}
	p.err = classify(msg, l.Subtype)
}

// apiErrorIn recognises the CLI's own wrapper for an upstream failure.
//
// Asked for an answer while the account was in arrears, the CLI returned
// is_error false, subtype "success", error null, and this as the assistant's
// message:
//
//	[API Error: 400 Access denied, please make sure your account is in good standing…]
//
// There is no machine-readable signal to read instead, and letting it through
// would hand the caller a 200 whose body is an error dressed as a model
// response — the failure this package exists to prevent, arriving by a
// different door.
//
// So the text is matched, conservatively: only when the whole answer is that
// bracketed wrapper. An answer that merely mentions an API error somewhere in
// its prose is a real answer and passes through untouched.
func apiErrorIn(result string) (string, bool) {
	s := strings.TrimSpace(result)
	const prefix = "[API Error:"
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, "]") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, prefix), "]")), true
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
		p.emit(ir.ErrorEvent(ir.AsError(runErr)))
		return
	}
	if !p.done {
		// The CLI stopped without its terminal line: the caller must not read a
		// truncated answer as a complete one.
		p.emit(ir.ErrorEvent(ir.IncompleteRun()))
		return
	}
	if !p.sawText && p.finalText != "" {
		// Nothing was streamed, so the terminal line is the answer rather than
		// a repeat of it.
		p.emit(ir.TextEvent(p.finalText))
	}
	p.emit(ir.Event{
		Type:       ir.EventDone,
		StopReason: ir.StopEndTurn,
		Usage:      p.usage,
		Model:      p.model,
	})
}

// classify turns the CLI's own failure into an ir error with a code a frontend
// can map to a status.
func classify(msg, subtype string) *ir.Error {
	detail := strings.TrimSpace(msg)
	haystack := strings.ToLower(detail + " " + subtype)

	switch {
	case containsAny(haystack, "no auth type", "not logged in", "unauthorized", "401", "api key"):
		return &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Qwen Code CLI is not authenticated; configure an auth type and try again",
			Detail:  detail,
		}
	case containsAny(haystack, "quota", "billing", "insufficient", "out of credits"):
		return &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Qwen Code account cannot serve requests: it is out of credit or quota",
			Detail:  detail,
		}
	case containsAny(haystack, "rate limit", "rate_limit", "overloaded", "429"):
		return &ir.Error{
			Code:       ir.CodeOverloaded,
			Message:    "the Qwen Code CLI is rate limited",
			Detail:     detail,
			RetryAfter: ir.DefaultRateLimitRetry,
		}
	}
	return &ir.Error{Code: ir.CodeUpstreamError, Message: "the Qwen Code CLI failed", Detail: detail}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
