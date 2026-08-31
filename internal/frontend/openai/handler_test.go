package openai

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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeCompletion(t *testing.T, rec *httptest.ResponseRecorder) chatCompletion {
	t.Helper()
	var got chatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	return got
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error response is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	return env.Error
}

func TestChatCompletion(t *testing.T) {
	backend := fake.New("fakecli", "Hello there.", "small", "large")
	backend.Thinking = "let me think"
	backend.Usage = &ir.Usage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 5}
	h := testHandler(t, backend)

	rec := post(t, h, `{"model":"fakecli:large","messages":[
		{"role":"system","content":"be nice"},
		{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := decodeCompletion(t, rec)

	if got.Object != "chat.completion" || !strings.HasPrefix(got.ID, "chatcmpl-") {
		t.Errorf("envelope = %+v", got)
	}
	if got.Model != "fakecli:large" {
		t.Errorf("model = %q, want the name the caller asked for", got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Message.Content != "Hello there." {
		t.Errorf("content = %q", c.Message.Content)
	}
	if c.Message.Role != "assistant" || c.FinishReason != "stop" {
		t.Errorf("choice = %+v", c)
	}
	if c.Message.ReasoningContent != "let me think" {
		t.Errorf("reasoning_content = %q", c.Message.ReasoningContent)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 16 || got.Usage.CompletionTokens != 3 || got.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v, want cached input counted in prompt_tokens", got.Usage)
	}

	// The adapter must have received the routed variant and the full history.
	if len(backend.Requests) != 1 {
		t.Fatalf("adapter ran %d times", len(backend.Requests))
	}
	req := backend.Requests[0]
	if req.Variant != "large" || req.Adapter != "fakecli" {
		t.Errorf("routed to %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != ir.RoleSystem || req.Messages[1].Content != "hi" {
		t.Errorf("messages = %+v", req.Messages)
	}
	if req.Stream {
		t.Error("a non-streaming request must not ask the adapter to stream")
	}
}

func TestContentPartsAreFlattened(t *testing.T) {
	backend := fake.New("fakecli", "ok", "small")
	rec := post(t, testHandler(t, backend), `{"model":"fakecli:small","messages":[
		{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := backend.Requests[0].Messages[0].Content; got != "first\nsecond" {
		t.Errorf("flattened content = %q", got)
	}
}

func TestRejections(t *testing.T) {
	h := testHandler(t, fake.New("fakecli", "ok", "small"))
	base := `"model":"fakecli:small","messages":[{"role":"user","content":"hi"}]`

	cases := []struct {
		name      string
		body      string
		status    int
		wantParam string
		wantIn    string
	}{
		{"stop", `{` + base + `,"stop":["END"]}`, 400, "stop", "stop sequences"},
		{"stream_options without stream", `{` + base + `,"stream_options":{"include_usage":true}}`, 400, "stream_options", "stream"},
		{"n>1", `{` + base + `,"n":2}`, 400, "n", "n=1"},
		{"logprobs", `{` + base + `,"logprobs":true}`, 400, "logprobs", "log probabilities"},
		{"tools", `{` + base + `,"tools":[{"type":"function"}]}`, 400, "tools", "tool calling"},
		{"tool_choice", `{` + base + `,"tool_choice":"auto"}`, 400, "tool_choice", "tool calling"},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, 400, "model", "required"},
		{"no messages", `{"model":"fakecli:small","messages":[]}`, 400, "messages", "at least one"},
		{"unknown model", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, 404, "model", "not available"},
		{"tool role", `{"model":"fakecli:small","messages":[{"role":"tool","content":"x"}]}`, 400, "messages[0].role", "tool"},
		{"image", `{"model":"fakecli:small","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, 400, "messages[0].content[0]", "image"},
		{"broken json", `{"model":`, 400, "", "valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			body := decodeError(t, rec)
			if tc.wantParam != "" && body.Param != tc.wantParam {
				t.Errorf("param = %q, want %q", body.Param, tc.wantParam)
			}
			if !strings.Contains(body.Message, tc.wantIn) {
				t.Errorf("message %q should mention %q", body.Message, tc.wantIn)
			}
			if body.Type == "" {
				t.Error("error type must be set for OpenAI SDK compatibility")
			}
		})
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	cases := []struct {
		code   ir.Code
		status int
	}{
		{ir.CodeUpstreamUnavailable, http.StatusServiceUnavailable},
		{ir.CodeUpstreamError, http.StatusBadGateway},
		{ir.CodeTimeout, http.StatusGatewayTimeout},
		{ir.CodeOverloaded, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			backend := fake.New("fakecli", "", "small")
			backend.Fail = &ir.Error{Code: tc.code, Message: "backend says no", Detail: "stderr tail"}
			rec := post(t, testHandler(t, backend),
				`{"model":"fakecli:small","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			body := decodeError(t, rec)
			if !strings.Contains(body.Message, "backend says no") {
				t.Errorf("message = %q", body.Message)
			}
			if !strings.Contains(body.Message, "stderr tail") {
				t.Errorf("the CLI's own diagnostics should reach the caller, got %q", body.Message)
			}
		})
	}
}

// A backend that stops mid-answer must not look like a short answer: the caller
// has no way to tell the difference, so it has to be an error.
func TestTruncatedRunIsReported(t *testing.T) {
	backend := fake.New("fakecli", "half an answer", "small")
	backend.Silent = true
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(decodeError(t, rec).Message, "without completing") {
		t.Errorf("error = %s", rec.Body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := testHandler(t, fake.New("fakecli", "ok", "small", "large"))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q", list.Object)
	}
	found := map[string]bool{}
	for _, m := range list.Data {
		found[m.ID] = true
		if m.Object != "model" || m.OwnedBy != "fakecli" {
			t.Errorf("model entry = %+v", m)
		}
	}
	// One row per backend; callers append ":<model>" to pick a model.
	if !found["fakecli"] {
		t.Errorf("fakecli missing from /v1/models: %v", found)
	}
	if len(found) != 1 {
		t.Errorf("/v1/models should list one row per backend, got %v", found)
	}
}

func TestSingleModelEndpoint(t *testing.T) {
	h := testHandler(t, fake.New("fakecli", "ok", "small"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models/fakecli:small", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown model status = %d, want 404", rec.Code)
	}
}

func TestMaxCompletionTokensAlias(t *testing.T) {
	backend := fake.New("fakecli", "ok", "small")
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":42}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := backend.Requests[0].MaxTokens
	if got == nil || *got != 42 {
		t.Errorf("max_completion_tokens not carried into the request: %v", got)
	}
}

// response_format used to be neither decoded nor refused, so a caller that
// asked for JSON got prose and a 200 with no sign the constraint was dropped.
func TestResponseFormatIsNotSilentlyIgnored(t *testing.T) {
	body := `{"model":"fake","messages":[{"role":"user","content":"hi"}],
	          "response_format":{"type":"json_schema","json_schema":{"name":"p","schema":{"type":"object"}}}}`
	rec := post(t, testHandler(t, fake.New("fake", "prose")), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Param != "response_format" {
		t.Errorf("param = %q, want response_format", env.Error.Param)
	}
	// The refusal names the backend: another model would serve this.
	if !strings.Contains(env.Error.Message, "fake") {
		t.Errorf("message = %q, want it to name the backend", env.Error.Message)
	}
}

func TestResponseFormatShapes(t *testing.T) {
	tests := []struct {
		name   string
		format string
		want   string
		status int
	}{
		{"absent", ``, "", http.StatusOK},
		{"text", `,"response_format":{"type":"text"}`, "", http.StatusOK},
		// json_object cannot be served honestly; see the handler for why.
		{"json_object", `,"response_format":{"type":"json_object"}`, "", http.StatusBadRequest},
		{
			"json_schema",
			`,"response_format":{"type":"json_schema","json_schema":{"name":"p","schema":{"type":"object","required":["a"]}}}`,
			`{"type":"object","required":["a"]}`, http.StatusOK,
		},
		{"json_schema with no schema", `,"response_format":{"type":"json_schema","json_schema":{"name":"p"}}`, "", http.StatusBadRequest},
		{"unknown type", `,"response_format":{"type":"nonsense"}`, "", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.New("fake", "reply")
			b.Schema = true
			rec := post(t, testHandler(t, b),
				`{"model":"fake","messages":[{"role":"user","content":"hi"}]`+tc.format+`}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status != http.StatusOK {
				return
			}
			if len(b.Requests) != 1 {
				t.Fatalf("adapter saw %d requests", len(b.Requests))
			}
			if got := b.Requests[0].Schema; got != tc.want {
				t.Errorf("Schema = %q, want %q", got, tc.want)
			}
		})
	}
}
