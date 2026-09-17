package anthropic

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

func testHandler(t *testing.T, backend *fake.Adapter) http.Handler {
	t.Helper()
	return testHandlerTuned(t, backend, nil)
}

// testHandlerTuned builds the handler and lets a test adjust it before it
// serves, e.g. to shorten the streaming keepalive cadence.
func testHandlerTuned(t *testing.T, backend *fake.Adapter, tune func(*Handler)) http.Handler {
	t.Helper()
	rt, err := router.New([]adapter.Adapter{backend})
	if err != nil {
		t.Fatal(err)
	}
	h := New(rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if tune != nil {
		tune(h)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeMessage(t *testing.T, rec *httptest.ResponseRecorder) message {
	t.Helper()
	var got message
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	return got
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error response is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if env.Type != "error" {
		t.Errorf("error envelope type = %q, want error", env.Type)
	}
	return env
}

func TestMessage(t *testing.T) {
	backend := fake.New("fakecli", "Hello there.", "small", "large")
	backend.Thinking = "let me think"
	backend.Usage = &ir.Usage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 5}
	h := testHandler(t, backend)

	rec := post(t, h, `{"model":"fakecli:large","max_tokens":64,"system":"be nice",
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := decodeMessage(t, rec)

	if got.Type != "message" || got.Role != "assistant" || !strings.HasPrefix(got.ID, "msg_") {
		t.Errorf("envelope = %+v", got)
	}
	if got.Model != "fakecli:large" {
		t.Errorf("model = %q, want the name the caller asked for", got.Model)
	}
	if got.StopReason == nil || *got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", got.StopReason)
	}
	if got.Usage.InputTokens != 11 || got.Usage.OutputTokens != 3 || got.Usage.CacheReadInputTokens != 5 {
		t.Errorf("usage = %+v", got.Usage)
	}

	// Thinking is its own block and comes first, as in the vendor API.
	if len(got.Content) != 2 {
		t.Fatalf("content = %+v, want a thinking block and a text block", got.Content)
	}
	if got.Content[0].Type != "thinking" || deref(got.Content[0].Thinking) != "let me think" {
		t.Errorf("content[0] = %+v", got.Content[0])
	}
	if got.Content[0].Signature == nil {
		t.Error("a thinking block must carry a signature field, even an empty one")
	}
	if got.Content[1].Type != "text" || deref(got.Content[1].Text) != "Hello there." {
		t.Errorf("content[1] = %+v", got.Content[1])
	}

	// The system field must reach the adapter as a system message, and the
	// history must arrive in full.
	req := backend.Requests[0]
	if req.Variant != "large" || req.Adapter != "fakecli" {
		t.Errorf("routed to %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != ir.RoleSystem || req.Messages[0].Content != "be nice" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[1].Role != ir.RoleUser || req.Messages[1].Content != "hi" {
		t.Errorf("messages[1] = %+v", req.Messages[1])
	}
	if req.MaxTokens == nil || *req.MaxTokens != 64 {
		t.Errorf("max_tokens not carried into the request: %v", req.MaxTokens)
	}
	if req.Stream {
		t.Error("a non-streaming request must not ask the adapter to stream")
	}
}

// A reply with no thinking still yields exactly one text block, so clients that
// index into content[0] keep working.
func TestMessageWithoutThinking(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := decodeMessage(t, rec)
	if len(got.Content) != 1 || got.Content[0].Type != "text" {
		t.Fatalf("content = %+v", got.Content)
	}
	// The field must be present, not omitted, even for an empty answer.
	if got.Content[0].Text == nil || *got.Content[0].Text != "" {
		t.Errorf("content[0] = %+v, want an explicit empty text field", got.Content[0])
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestSystemAndContentBlocks(t *testing.T) {
	backend := fake.New("fakecli", "ok", "small")
	rec := post(t, testHandler(t, backend), `{"model":"fakecli:small","max_tokens":8,
		"system":[{"type":"text","text":"first rule"},{"type":"text","text":"second rule"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"ignored"},{"type":"text","text":"earlier reply"}]},
			{"role":"user","content":"and now?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	msgs := backend.Requests[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("messages = %+v", msgs)
	}
	if msgs[0].Content != "first rule\nsecond rule" {
		t.Errorf("system blocks joined as %q", msgs[0].Content)
	}
	if msgs[1].Content != "one\ntwo" {
		t.Errorf("text blocks joined as %q", msgs[1].Content)
	}
	// A replayed thinking block is the model's scratch space, not conversation.
	if msgs[2].Content != "earlier reply" {
		t.Errorf("assistant turn = %q, want the thinking block dropped", msgs[2].Content)
	}
}

func TestRejections(t *testing.T) {
	h := testHandler(t, fake.New("fakecli", "ok", "small"))
	base := `"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]`

	cases := []struct {
		name     string
		body     string
		status   int
		wantType string
		wantIn   string
	}{
		{"no model", `{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 400, "invalid_request_error", "model name is required"},
		{"no messages", `{"model":"fakecli:small","max_tokens":8,"messages":[]}`, 400, "invalid_request_error", "at least one message"},
		{"stop sequences", `{` + base + `,"stop_sequences":["END"]}`, 400, "invalid_request_error", "stop sequences"},
		{"tools", `{` + base + `,"tools":[{"name":"x"}]}`, 400, "invalid_request_error", "tool calling"},
		{"tool_choice", `{` + base + `,"tool_choice":{"type":"any"}}`, 400, "invalid_request_error", "tool calling"},
		{"system role in messages", `{"model":"fakecli:small","max_tokens":8,
			"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`, 400, "invalid_request_error", `"system" field`},
		{"unknown role", `{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"tool","content":"x"}]}`, 400, "invalid_request_error", "unknown role"},
		{"image block", `{"model":"fakecli:small","max_tokens":8,
			"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`, 400, "invalid_request_error", "image"},
		{"tool_result block", `{"model":"fakecli:small","max_tokens":8,
			"messages":[{"role":"user","content":[{"type":"tool_result","text":"x"}]}]}`, 400, "invalid_request_error", "tool blocks"},
		{"unknown model", `{"model":"nope","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 404, "not_found_error", "not available"},
		{"broken json", `{"model":`, 400, "invalid_request_error", "valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			env := decodeError(t, rec)
			if env.Error.Type != tc.wantType {
				t.Errorf("error type = %q, want %q", env.Error.Type, tc.wantType)
			}
			if !strings.Contains(env.Error.Message, tc.wantIn) {
				t.Errorf("message %q should mention %q", env.Error.Message, tc.wantIn)
			}
		})
	}
}

// max_tokens is required by the vendor API but cannot be enforced by any agent
// CLI, so agent2api takes the lenient route: a request without it still works.
func TestMaxTokensIsOptional(t *testing.T) {
	rec := post(t, testHandler(t, fake.New("fakecli", "ok", "small")),
		`{"model":"fakecli:small","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// A backend that stops mid-answer must not look like a short answer.
func TestTruncatedRunIsReported(t *testing.T) {
	backend := fake.New("fakecli", "half an answer", "small")
	backend.Silent = true
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(decodeError(t, rec).Error.Message, "without completing") {
		t.Errorf("error = %s", rec.Body)
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	cases := []struct {
		code      ir.Code
		status    int
		errorType string
	}{
		{ir.CodeUpstreamUnavailable, http.StatusServiceUnavailable, "api_error"},
		{ir.CodeUpstreamError, http.StatusBadGateway, "api_error"},
		{ir.CodeTimeout, http.StatusGatewayTimeout, "api_error"},
		{ir.CodeOverloaded, http.StatusTooManyRequests, "rate_limit_error"},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			backend := fake.New("fakecli", "", "small")
			backend.Fail = &ir.Error{Code: tc.code, Message: "backend says no", Detail: "stderr tail"}
			rec := post(t, testHandler(t, backend),
				`{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			env := decodeError(t, rec)
			if env.Error.Type != tc.errorType {
				t.Errorf("error type = %q, want %q", env.Error.Type, tc.errorType)
			}
			if !strings.Contains(env.Error.Message, "backend says no") ||
				!strings.Contains(env.Error.Message, "stderr tail") {
				t.Errorf("the CLI's own diagnostics should reach the caller, got %q", env.Error.Message)
			}
		})
	}
}

// This dialect measures thinking in tokens and the backends measure it in
// levels. Refusing the budget is the decision recorded in #20: any threshold
// turning a number into a level would be invented here.
func TestThinkingBudgetIsRefused(t *testing.T) {
	b := fake.New("fake", "reply")
	b.Efforts = []ir.Effort{ir.EffortLow, ir.EffortHigh}
	rec := post(t, testHandler(t, b),
		`{"model":"fake","max_tokens":64,"messages":[{"role":"user","content":"hi"}],
		  "thinking":{"type":"enabled","budget_tokens":2048}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Error.Message, "not a token budget") {
		t.Errorf("message = %q, want it to explain why", env.Error.Message)
	}
}

// The on/off switch is the one thing the two vocabularies share.
func TestThinkingSwitch(t *testing.T) {
	tests := []struct {
		name     string
		thinking string
		want     ir.Effort
	}{
		{"enabled asks for the top level", `,"thinking":{"type":"enabled"}`, ir.EffortHigh},
		{"disabled asks for none", `,"thinking":{"type":"disabled"}`, ""},
		{"absent asks for none", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.New("fake", "reply")
			b.Efforts = []ir.Effort{ir.EffortLow, ir.EffortHigh}
			rec := post(t, testHandler(t, b),
				`{"model":"fake","max_tokens":64,"messages":[{"role":"user","content":"hi"}]`+tc.thinking+`}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if got := b.Requests[0].Effort; got != tc.want {
				t.Errorf("Effort = %q, want %q", got, tc.want)
			}
		})
	}
}
