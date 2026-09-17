// Package anthropic implements the Anthropic-compatible HTTP surface:
// POST /v1/messages, streaming and non-streaming.
//
// The wire shapes below are the subset of the Messages API that agent2api can
// honour, plus the fields it must explicitly refuse. Everything an agent CLI
// cannot enforce is either passed along as a best-effort hint or rejected with
// a 400 — never silently ignored in a way that changes the answer.
package anthropic

import "encoding/json"

// messagesRequest is POST /v1/messages.
type messagesRequest struct {
	Model string `json:"model"`
	// System is a string or an array of text blocks.
	System   json.RawMessage `json:"system"`
	Messages []inputMessage  `json:"messages"`
	Stream   bool            `json:"stream"`

	// Best effort: no agent CLI exposes a knob for these.
	MaxTokens   *int     `json:"max_tokens"`
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	TopK        *int     `json:"top_k"`

	// `type` selects whether the model thinks; `budget_tokens` is refused,
	// because a token budget is not a level and the backends take levels.
	Thinking json.RawMessage `json:"thinking"`

	// Refused with 400 when present; see validate.
	StopSequences []string        `json:"stop_sequences"`
	Tools         json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`

	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

// inputMessage keeps Content raw: Anthropic allows a string or a block array.
type inputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// inputBlock is one element of the block array form of content.
type inputBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Source   json.RawMessage `json:"source"`
}

// message is the non-streaming response, and the object message_start carries.
type message struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []block `json:"content"`
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        usage   `json:"usage"`
}

// block is one content block: text, or the CLI's thinking when it exposed any.
//
// Only the fields that belong to the block's own type are emitted, but they are
// emitted even when empty, which is why they are pointers. The vendor SDKs
// validate a text block against a required text field and a thinking block
// against required thinking and signature fields, so an omitted empty string is
// a decode error on the client rather than a tidier payload.
type block struct {
	Type      string  `json:"type"`
	Text      *string `json:"text,omitempty"`
	Thinking  *string `json:"thinking,omitempty"`
	Signature *string `json:"signature,omitempty"`
}

// textBlock builds a text block.
func textBlock(text string) block { return block{Type: "text", Text: &text} }

// thinkingBlock builds a thinking block. Its signature is empty: agent2api has
// none to give, because the block did not come from the vendor API.
func thinkingBlock(thinking string) block {
	signature := ""
	return block{Type: "thinking", Thinking: &thinking, Signature: &signature}
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// Streaming frames. Each is sent as an SSE event named after its type field,
// which is what the Anthropic SDKs dispatch on.
type (
	messageStartEvent struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}

	contentBlockStartEvent struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock block  `json:"content_block"`
	}

	contentBlockDeltaEvent struct {
		Type  string     `json:"type"`
		Index int        `json:"index"`
		Delta blockDelta `json:"delta"`
	}

	blockDelta struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		Thinking string `json:"thinking,omitempty"`
	}

	contentBlockStopEvent struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}

	messageDeltaEvent struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason   *string `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
		Usage usage `json:"usage"`
	}

	messageStopEvent struct {
		Type string `json:"type"`
	}

	pingEvent struct {
		Type string `json:"type"`
	}
)

// errorEnvelope is Anthropic's error shape, used for responses and for the
// error event of a stream.
type errorEnvelope struct {
	Type  string    `json:"type"`
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// thinkingRequest is the `thinking` field.
type thinkingRequest struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens"`
}
