package ir

// EventType discriminates the events an adapter streams while a CLI runs.
type EventType int

const (
	// EventStart is emitted once, when the backend has accepted the request.
	EventStart EventType = iota
	// EventTextDelta carries a chunk of assistant-visible text.
	EventTextDelta
	// EventThinkingDelta carries a chunk of reasoning text, when the CLI
	// exposes one. Frontends may drop it.
	EventThinkingDelta
	// EventUsage carries token accounting; it may arrive more than once, in
	// which case later events supersede earlier ones.
	EventUsage
	// EventDone is the final event of a successful run.
	EventDone
	// EventError is the final event of a failed run.
	EventError
)

func (t EventType) String() string {
	switch t {
	case EventStart:
		return "start"
	case EventTextDelta:
		return "text_delta"
	case EventThinkingDelta:
		return "thinking_delta"
	case EventUsage:
		return "usage"
	case EventDone:
		return "done"
	case EventError:
		return "error"
	}
	return "unknown"
}

// Usage reports token accounting as the CLI understands it. Fields the backend
// does not report stay zero.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	// ReasoningOutputTokens is the part of OutputTokens the backend spent on
	// reasoning it did not show. Only some CLIs report it; the rest leave it
	// zero, which is indistinguishable from "reasoned about nothing" and is the
	// honest answer either way.
	ReasoningOutputTokens int
}

// Effort is how hard the caller asked the model to think.
//
// The values are the union of what the dialects and the CLIs use, carried
// verbatim rather than normalised: the vocabularies do not line up, and mapping
// one onto another would mean inventing a correspondence. `minimal` has no
// counterpart in Claude Code's scale and `max` has none in OpenAI's, so a
// backend that cannot take a level refuses it by name instead of rounding it to
// a neighbour.
type Effort string

const (
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

// Efforts is every level a frontend will decode. A value outside it is the
// caller's mistake and is refused before any backend is consulted.
var Efforts = []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// ValidEffort reports whether s names a level.
func ValidEffort(s string) bool {
	for _, e := range Efforts {
		if Effort(s) == e {
			return true
		}
	}
	return false
}

// StopReason mirrors the Anthropic vocabulary; frontends map it into their own.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopStopSeq   StopReason = "stop_sequence"
)

// Event is one unit of adapter output. Exactly one of Text, Usage or Err is
// meaningful, depending on Type.
type Event struct {
	Type EventType

	// Text is set for EventTextDelta and EventThinkingDelta.
	Text string

	// Usage is set for EventUsage and may also be set on EventDone.
	Usage *Usage

	// StopReason may be set on EventDone.
	StopReason StopReason

	// Model is the concrete backend model the CLI reported, when it does. Set
	// on EventStart or EventDone.
	Model string

	// Err is set for EventError.
	Err *Error
}

// TextEvent builds an assistant text delta.
func TextEvent(s string) Event { return Event{Type: EventTextDelta, Text: s} }

// ThinkingEvent builds a reasoning delta.
func ThinkingEvent(s string) Event { return Event{Type: EventThinkingDelta, Text: s} }

// ErrorEvent wraps an error as a terminal event.
func ErrorEvent(err *Error) Event { return Event{Type: EventError, Err: err} }
