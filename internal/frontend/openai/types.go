// Package openai implements the OpenAI-compatible HTTP surface:
// POST /v1/chat/completions and GET /v1/models, streaming and non-streaming.
package openai

import "encoding/json"

// chatRequest mirrors the fields of OpenAI's chat completion request that
// agent2api either honours or must explicitly refuse. Everything else is
// accepted and ignored, exactly as an OpenAI-compatible server may do.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`

	// Best effort: most agent CLIs cannot enforce these.
	Temperature         *float64 `json:"temperature"`
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`

	// Refused with 400 when present; see validate.
	Stop           json.RawMessage `json:"stop"`
	N              *int            `json:"n"`
	Logprobs       *bool           `json:"logprobs"`
	TopLogprobs    *int            `json:"top_logprobs"`
	ResponseFormat json.RawMessage `json:"response_format"`
	Tools          json.RawMessage `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	Functions      json.RawMessage `json:"functions"`
	FunctionCall   json.RawMessage `json:"function_call"`

	User string `json:"user"`
}

// chatMessage keeps Content raw because OpenAI allows either a string or an
// array of typed parts.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// contentPart is one element of the array form of message content.
type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// chatCompletion is the non-streaming response body.
type chatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
	// SystemFingerprint is absent on purpose: there is no deterministic
	// backend fingerprint to report for a local CLI.
}

type choice struct {
	Index        int          `json:"index"`
	Message      replyMessage `json:"message"`
	FinishReason string       `json:"finish_reason"`
	Logprobs     *struct{}    `json:"logprobs"`
}

type replyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ReasoningContent carries the CLI's thinking output when it exposes any.
	// It is not part of the OpenAI schema; clients that do not know it ignore
	// the field, and clients that do (many local-LLM UIs) render it.
	ReasoningContent string  `json:"reasoning_content,omitempty"`
	Refusal          *string `json:"refusal"`
}

// chatChunk is one streaming frame. The final frame of a run carries an empty
// Choices list and the usage totals, which is how OpenAI reports usage on a
// stream, and only when the caller asked for it.
type chatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index int        `json:"index"`
	Delta chunkDelta `json:"delta"`
	// FinishReason is null on every frame but the last one of a choice.
	FinishReason *string `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// ReasoningContent mirrors the non-standard field the non-streaming
	// response uses for the CLI's thinking output.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// streamError is the frame a stream sends when the run fails after the
// response has already been committed as 200 OK. OpenAI has no spec for this;
// an error object in the data frame is what its own API and the common clients
// do, so a caller sees a cause instead of a truncated answer.
type streamError struct {
	Error errorBody `json:"error"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// modelList is the GET /v1/models response.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// errorEnvelope is OpenAI's error shape.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// responseFormat is the `response_format` object. The schema sits one level
// down, under json_schema, which is where this dialect differs from Responses.
type responseFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	} `json:"json_schema"`
}
