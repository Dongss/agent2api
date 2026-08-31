package responses

import "encoding/json"

// --- request -----------------------------------------------------------

// responsesRequest is POST /v1/responses as agent2api reads it. Fields this
// gateway cannot serve are decoded anyway so they can be refused by name
// instead of ignored; see [responsesRequest.reject].
type responsesRequest struct {
	Model string `json:"model"`
	// Input is either a bare string or an array of items.
	Input json.RawMessage `json:"input"`
	// Instructions is the system prompt under its Responses name.
	Instructions *string `json:"instructions"`

	Stream          bool     `json:"stream"`
	MaxOutputTokens *int     `json:"max_output_tokens"`
	Temperature     *float64 `json:"temperature"`
	TopP            *float64 `json:"top_p"`

	// Store is accepted and ignored: agent2api keeps nothing. The response
	// echoes false so the caller is told rather than left to assume.
	Store *bool `json:"store"`

	Metadata map[string]string `json:"metadata"`
	User     string            `json:"user"`

	// Reasoning selects how much reasoning the model does and whether it is
	// summarised. Only the summary side is meaningful here, and only as a
	// request to be passed nowhere: no CLI takes a knob for it.
	Reasoning json.RawMessage `json:"reasoning"`

	// Everything below is refused.
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  json.RawMessage `json:"parallel_tool_calls"`
	PreviousResponseID *string         `json:"previous_response_id"`
	Include            json.RawMessage `json:"include"`
	Text               json.RawMessage `json:"text"`
	Conversation       json.RawMessage `json:"conversation"`
	Background         json.RawMessage `json:"background"`
}

// inputItem is one entry of an `input` array. The Responses API keys items on
// Type, but a bare message may omit it, so Role decides when Type is empty.
type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// Set on function_call / function_call_output items, which are refused.
	Name   string `json:"name"`
	CallID string `json:"call_id"`
}

// contentPart is one entry of an item's content array.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// textFormat is the `text` field, which selects structured output. Responses
// puts the schema inline in the format object, where chat completions nests it
// one level down under json_schema.
type textFormat struct {
	Format struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	} `json:"format"`
}

// --- response ----------------------------------------------------------

// response is the object returned by a non-streaming call and embedded whole in
// the terminal streaming event.
type response struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	CreatedAt int64    `json:"created_at"`
	Status    string   `json:"status"`
	Model     string   `json:"model"`
	Output    []item   `json:"output"`
	Usage     *usage   `json:"usage"`
	Error     *errBody `json:"error"`

	IncompleteDetails *incompleteDetails `json:"incomplete_details"`

	// Echoed back so the response describes the call that produced it. Store is
	// always false: the caller may have asked for storage, and did not get it.
	Instructions       *string           `json:"instructions"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	PreviousResponseID *string           `json:"previous_response_id"`
	Store              bool              `json:"store"`
	Metadata           map[string]string `json:"metadata"`

	// Constant for this gateway, but clients read them.
	ParallelToolCalls bool   `json:"parallel_tool_calls"`
	ToolChoice        string `json:"tool_choice"`
	Tools             []any  `json:"tools"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

// item is one entry of `output`: either a reasoning item or a message.
type item struct {
	Type    string
	ID      string
	Status  string
	Role    string
	Content []outputPart
	Summary []summaryPart
}

// MarshalJSON writes the fields that belong to this item's kind, and writes
// them even when empty.
//
// `omitempty` cannot do this job: it drops an empty slice as readily as a nil
// one, so a freshly opened message item would ship without `content`. The
// OpenAI SDK's stream accumulator reads that as null and crashes on the next
// content_part.added, which is how this was found — an empty list and a missing
// key are different statements, and only one of them is true here.
func (i item) MarshalJSON() ([]byte, error) {
	switch i.Type {
	case "reasoning":
		summary := i.Summary
		if summary == nil {
			summary = []summaryPart{}
		}
		return json.Marshal(struct {
			Type    string        `json:"type"`
			ID      string        `json:"id"`
			Summary []summaryPart `json:"summary"`
		}{i.Type, i.ID, summary})
	default:
		content := i.Content
		if content == nil {
			content = []outputPart{}
		}
		return json.Marshal(struct {
			Type    string       `json:"type"`
			ID      string       `json:"id"`
			Status  string       `json:"status"`
			Role    string       `json:"role"`
			Content []outputPart `json:"content"`
		}{i.Type, i.ID, i.Status, i.Role, content})
	}
}

type outputPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func messageItem(id, text string) item {
	return item{
		Type:    "message",
		ID:      id,
		Status:  "completed",
		Role:    "assistant",
		Content: []outputPart{{Type: "output_text", Text: text, Annotations: []any{}}},
	}
}

func reasoningItem(id, text string) item {
	it := item{Type: "reasoning", ID: id, Status: "completed", Summary: []summaryPart{}}
	if text != "" {
		it.Summary = append(it.Summary, summaryPart{Type: "summary_text", Text: text})
	}
	return it
}

// usage is the Responses token accounting, which nests the cached and reasoning
// counts one level down.
type usage struct {
	InputTokens        int          `json:"input_tokens"`
	InputTokenDetails  inputDetails `json:"input_tokens_details"`
	OutputTokens       int          `json:"output_tokens"`
	OutputTokenDetails outDetails   `json:"output_tokens_details"`
	TotalTokens        int          `json:"total_tokens"`
}

type inputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type outDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// --- streaming events --------------------------------------------------

// Every streamed event carries the same two fields; the rest varies by type.
// They are separate structs rather than one with omitempty because an absent
// index and an index of 0 mean different things to a client reassembling the
// output.

type createdEvent struct {
	Type     string   `json:"type"`
	Sequence int      `json:"sequence_number"`
	Response response `json:"response"`
}

type outputItemEvent struct {
	Type        string `json:"type"`
	Sequence    int    `json:"sequence_number"`
	OutputIndex int    `json:"output_index"`
	Item        item   `json:"item"`
}

type contentPartEvent struct {
	Type         string     `json:"type"`
	Sequence     int        `json:"sequence_number"`
	ItemID       string     `json:"item_id"`
	OutputIndex  int        `json:"output_index"`
	ContentIndex int        `json:"content_index"`
	Part         outputPart `json:"part"`
}

type textDeltaEvent struct {
	Type         string `json:"type"`
	Sequence     int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type textDoneEvent struct {
	Type         string `json:"type"`
	Sequence     int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

type summaryPartEvent struct {
	Type         string      `json:"type"`
	Sequence     int         `json:"sequence_number"`
	ItemID       string      `json:"item_id"`
	OutputIndex  int         `json:"output_index"`
	SummaryIndex int         `json:"summary_index"`
	Part         summaryPart `json:"part"`
}

type summaryDeltaEvent struct {
	Type         string `json:"type"`
	Sequence     int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	SummaryIndex int    `json:"summary_index"`
	Delta        string `json:"delta"`
}

type summaryDoneEvent struct {
	Type         string `json:"type"`
	Sequence     int    `json:"sequence_number"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	SummaryIndex int    `json:"summary_index"`
	Text         string `json:"text"`
}

// errorEvent is the standalone `error` event, used when the failure has no
// response object to hang off.
type errorEvent struct {
	Type     string `json:"type"`
	Sequence int    `json:"sequence_number"`
	errBody
}

// --- errors ------------------------------------------------------------

type errorEnvelope struct {
	Error errBody `json:"error"`
}

type errBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}
