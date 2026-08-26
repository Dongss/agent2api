package responses

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/router"
)

func newTestHandler(t *testing.T, backend *fake.Adapter) *Handler {
	t.Helper()
	rt, err := router.New([]adapter.Adapter{backend})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := New(rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Heartbeat = 0
	return h
}

// plain is a backend that answers with text and nothing else.
func plain(t *testing.T) *Handler {
	t.Helper()
	b := fake.New("fake", "Mock reply.")
	b.Usage = &ir.Usage{InputTokens: 11, OutputTokens: 5, CacheReadInputTokens: 3}
	return newTestHandler(t, b)
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Responses(rec, req)
	return rec
}

// decodeResponse reads a non-streaming body, failing the test if it is not the
// response object.
func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	return got
}

func TestResponseShape(t *testing.T) {
	h := plain(t)
	rec := post(t, h, `{"model":"fake","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeResponse(t, rec)

	if got["object"] != "response" || got["status"] != "completed" {
		t.Errorf("object/status = %v/%v", got["object"], got["status"])
	}
	if id, _ := got["id"].(string); !strings.HasPrefix(id, "resp_") {
		t.Errorf("id = %q, want a resp_ prefix", id)
	}
	// The caller did not ask to store, and is told so either way.
	if got["store"] != false {
		t.Errorf("store = %v, want false", got["store"])
	}
	// A gateway that serves no tools still has to say so in the fields clients
	// read before deciding what to do next.
	if got["tool_choice"] != "none" || got["parallel_tool_calls"] != false {
		t.Errorf("tool fields = %v / %v", got["tool_choice"], got["parallel_tool_calls"])
	}

	output, _ := got["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("no output items: %s", rec.Body.String())
	}
	last, _ := output[len(output)-1].(map[string]any)
	if last["type"] != "message" || last["role"] != "assistant" {
		t.Fatalf("last item = %v, want an assistant message", last)
	}
	content, _ := last["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one part", content)
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] == "" {
		t.Errorf("part = %v", part)
	}
}

// Asking for storage must not be silently ignored: the answer says it was not
// stored, which is the one thing the caller could not otherwise find out.
func TestStoreIsAcceptedAndReportedFalse(t *testing.T) {
	h := plain(t)
	rec := post(t, h, `{"model":"fake","input":"hello","store":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeResponse(t, rec); got["store"] != false {
		t.Errorf("store = %v, want false even though the caller asked for true", got["store"])
	}
}

// An empty slice and a missing key say different things. A message item without
// `content` breaks the OpenAI SDK's stream accumulator, so the encoding is
// pinned here rather than left to `omitempty`.
func TestEmptyItemFieldsAreEncodedAsEmptyLists(t *testing.T) {
	msg, err := json.Marshal(item{Type: "message", ID: "msg_1", Status: "in_progress", Role: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), `"content":[]`) {
		t.Errorf("message item = %s, want an empty content list", msg)
	}
	if strings.Contains(string(msg), "summary") {
		t.Errorf("message item = %s, want no summary field", msg)
	}

	rs, err := json.Marshal(item{Type: "reasoning", ID: "rs_1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rs), `"summary":[]`) {
		t.Errorf("reasoning item = %s, want an empty summary list", rs)
	}
	if strings.Contains(string(rs), "content") {
		t.Errorf("reasoning item = %s, want no content field", rs)
	}
}

func TestReasoningLeadsTheOutput(t *testing.T) {
	b := fake.New("fake", "Mock reply.")
	b.Thinking = "Considering the question carefully."
	h := newTestHandler(t, b)
	rec := post(t, h, `{"model":"fake","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	output, _ := decodeResponse(t, rec)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output has %d items, want reasoning then message", len(output))
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "reasoning" {
		t.Fatalf("first item = %v, want reasoning", first["type"])
	}
	summary, _ := first["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary = %v, want one part", summary)
	}
	if part, _ := summary[0].(map[string]any); part["type"] != "summary_text" || part["text"] == "" {
		t.Errorf("summary part = %v", summary[0])
	}
}

func TestUsageSplitsCachedAndReasoningTokens(t *testing.T) {
	u := toUsage(&ir.Usage{
		InputTokens: 10, CacheReadInputTokens: 5, CacheCreationInputTokens: 2,
		OutputTokens: 20, ReasoningOutputTokens: 8,
	})
	// Responses counts the whole prompt in input_tokens and calls the cached
	// part out separately, where ir keeps the two apart.
	if u.InputTokens != 17 || u.InputTokenDetails.CachedTokens != 5 {
		t.Errorf("input = %d (cached %d), want 17 (5)", u.InputTokens, u.InputTokenDetails.CachedTokens)
	}
	if u.OutputTokens != 20 || u.OutputTokenDetails.ReasoningTokens != 8 {
		t.Errorf("output = %d (reasoning %d), want 20 (8)", u.OutputTokens, u.OutputTokenDetails.ReasoningTokens)
	}
	if u.TotalTokens != 37 {
		t.Errorf("total = %d, want 37", u.TotalTokens)
	}
}

func TestInputShapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []ir.Message
	}{
		{
			"bare string is the user turn",
			`{"model":"fake","input":"hi"}`,
			[]ir.Message{{Role: ir.RoleUser, Content: "hi"}},
		},
		{
			"instructions lead as the system prompt",
			`{"model":"fake","instructions":"be terse","input":"hi"}`,
			[]ir.Message{{Role: ir.RoleSystem, Content: "be terse"}, {Role: ir.RoleUser, Content: "hi"}},
		},
		{
			"developer is a system turn and keeps its place",
			`{"model":"fake","input":[{"role":"user","content":"a"},{"role":"developer","content":"b"},{"role":"user","content":"c"}]}`,
			[]ir.Message{
				{Role: ir.RoleUser, Content: "a"},
				{Role: ir.RoleSystem, Content: "b"},
				{Role: ir.RoleUser, Content: "c"},
			},
		},
		{
			"content parts are joined",
			`{"model":"fake","input":[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}]}`,
			[]ir.Message{{Role: ir.RoleUser, Content: "a\nb"}},
		},
		{
			"an assistant turn replays as one",
			`{"model":"fake","input":[{"role":"user","content":"a"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]}`,
			[]ir.Message{{Role: ir.RoleUser, Content: "a"}, {Role: ir.RoleAssistant, Content: "b"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req responsesRequest
			if err := json.Unmarshal([]byte(tc.input), &req); err != nil {
				t.Fatal(err)
			}
			got, err := req.toMessages()
			if err != nil {
				t.Fatalf("toMessages: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d messages, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("message %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Everything this gateway cannot serve honestly must come back as a 400 that
// names the field, rather than being dropped on the floor.
func TestRefusals(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{"tools", `{"model":"fake","input":"hi","tools":[{"type":"function","name":"f"}]}`, "tools"},
		{"tool_choice", `{"model":"fake","input":"hi","tool_choice":"auto"}`, "tool_choice"},
		{"parallel_tool_calls", `{"model":"fake","input":"hi","parallel_tool_calls":true}`, "parallel_tool_calls"},
		{"previous_response_id", `{"model":"fake","input":"hi","previous_response_id":"resp_1"}`, "previous_response_id"},
		{"conversation", `{"model":"fake","input":"hi","conversation":"conv_1"}`, "conversation"},
		{"background", `{"model":"fake","input":"hi","background":true}`, "background"},
		{"include", `{"model":"fake","input":"hi","include":["reasoning.encrypted_content"]}`, "include"},
		{"structured output", `{"model":"fake","input":"hi","text":{"format":{"type":"json_schema"}}}`, "text.format.type"},
		{"image part", `{"model":"fake","input":[{"role":"user","content":[{"type":"input_image"}]}]}`, "input[0].content[0]"},
		{"file part", `{"model":"fake","input":[{"role":"user","content":[{"type":"input_file"}]}]}`, "input[0].content[0]"},
		{"function_call item", `{"model":"fake","input":[{"type":"function_call","name":"f"}]}`, "input[0].type"},
		{"reasoning item", `{"model":"fake","input":[{"type":"reasoning","id":"rs_1"}]}`, "input[0].type"},
		{"unknown item type", `{"model":"fake","input":[{"type":"nonsense"}]}`, "input[0].type"},
		{"unknown role", `{"model":"fake","input":[{"role":"robot","content":"hi"}]}`, "input[0].role"},
		{"missing model", `{"input":"hi"}`, "model"},
		{"missing input", `{"model":"fake"}`, "input"},
		{"system only", `{"model":"fake","input":[{"role":"system","content":"hi"}]}`, "input"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := plain(t)
			rec := post(t, h, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not JSON: %v", err)
			}
			if env.Error.Param != tc.param {
				t.Errorf("param = %q, want %q (message: %s)", env.Error.Param, tc.param, env.Error.Message)
			}
			if env.Error.Type != "invalid_request_error" {
				t.Errorf("type = %q", env.Error.Type)
			}
			if env.Error.Message == "" {
				t.Error("a refusal with no explanation is not one")
			}
		})
	}
}

// The tools refusal must survive OpenAI adding tool types: a real client was
// seen sending `namespace` and `web_search` alongside `function`.
func TestToolsRefusalDoesNotAssumeFunctionType(t *testing.T) {
	h := plain(t)
	rec := post(t, h, `{"model":"fake","input":"hi","tools":[{"type":"web_search"},{"type":"namespace","name":"x"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// `text` carries knobs other than the output format, so only a non-text format
// is an error.
func TestPlainTextFormatIsAccepted(t *testing.T) {
	h := plain(t)
	for _, body := range []string{
		`{"model":"fake","input":"hi","text":{"format":{"type":"text"}}}`,
		`{"model":"fake","input":"hi","text":{"verbosity":"low"}}`,
	} {
		if rec := post(t, h, body); rec.Code != http.StatusOK {
			t.Errorf("status = %d for %s: %s", rec.Code, body, rec.Body.String())
		}
	}
}

func TestUnknownModelIs404(t *testing.T) {
	h := plain(t)
	rec := post(t, h, `{"model":"nope","input":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestBackendFailureKeepsItsStatus(t *testing.T) {
	b := fake.New("fake", "")
	b.Fail = &ir.Error{
		Code:    ir.CodeUpstreamUnavailable,
		Message: "the fake CLI is not logged in",
		Detail:  "fake: no credentials found",
	}
	h := newTestHandler(t, b)
	rec := post(t, h, `{"model":"fake","input":"hi"}`)
	if rec.Code < 500 {
		t.Fatalf("status = %d, want an upstream failure: %s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if env.Error.Message == "" {
		t.Error("no explanation on the failure")
	}
}
