package openai

import (
	"net/http"
	"testing"

	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

// A 429 has to say when to come back: both SDKs read Retry-After to size their
// backoff, and guessing is worse than being told.
func TestRetryAfterHeader(t *testing.T) {
	cases := []struct {
		name string
		fail *ir.Error
		want string
	}{
		{"rate limited", &ir.Error{Code: ir.CodeOverloaded, Message: "busy", RetryAfter: 10 * 1e9}, "10"},
		{"rounded up", &ir.Error{Code: ir.CodeOverloaded, Message: "busy", RetryAfter: 1500 * 1e6}, "2"},
		{"no hint", &ir.Error{Code: ir.CodeUpstreamError, Message: "broken"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := fake.New("fakecli", "", "small")
			backend.Fail = tc.fail
			rec := post(t, testHandler(t, backend),
				`{"model":"fakecli:small","messages":[{"role":"user","content":"hi"}]}`)
			if got := rec.Header().Get("Retry-After"); got != tc.want {
				t.Errorf("Retry-After = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every error class must reach the caller as the status and the error type an
// OpenAI SDK knows how to act on. A wrong status here turns a retryable
// condition into a hard failure, or the reverse.
func TestErrorMappingTable(t *testing.T) {
	cases := []struct {
		code      ir.Code
		status    int
		errorType string
		errorCode string
	}{
		{ir.CodeInvalidRequest, http.StatusBadRequest, "invalid_request_error", ""},
		{ir.CodeModelNotFound, http.StatusNotFound, "invalid_request_error", "model_not_found"},
		{ir.CodeUnauthorized, http.StatusUnauthorized, "authentication_error", "invalid_api_key"},
		{ir.CodeOverloaded, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"},
		{ir.CodeUpstreamUnavailable, http.StatusServiceUnavailable, "api_error", "service_unavailable"},
		{ir.CodeUpstreamError, http.StatusBadGateway, "api_error", "upstream_error"},
		{ir.CodeTimeout, http.StatusGatewayTimeout, "api_error", "timeout"},
		{ir.CodeInternal, http.StatusInternalServerError, "api_error", ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			backend := fake.New("fakecli", "", "small")
			backend.Fail = &ir.Error{Code: tc.code, Message: "backend says no"}
			rec := post(t, testHandler(t, backend),
				`{"model":"fakecli:small","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			body := decodeError(t, rec)
			if body.Type != tc.errorType {
				t.Errorf("type = %q, want %q", body.Type, tc.errorType)
			}
			if body.Code != tc.errorCode {
				t.Errorf("code = %q, want %q", body.Code, tc.errorCode)
			}
		})
	}
}
