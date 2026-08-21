package anthropic

import (
	"net/http"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

func TestRetryAfterHeader(t *testing.T) {
	backend := fake.New("fakecli", "", "small")
	backend.Fail = &ir.Error{Code: ir.CodeOverloaded, Message: "busy", RetryAfter: 10 * time.Second}
	rec := post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if got := rec.Header().Get("Retry-After"); got != "10" {
		t.Errorf("Retry-After = %q, want 10", got)
	}

	backend = fake.New("fakecli", "", "small")
	backend.Fail = &ir.Error{Code: ir.CodeUpstreamError, Message: "broken"}
	rec = post(t, testHandler(t, backend),
		`{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want none when the backend gave no hint", got)
	}
}

// Every error class must reach the caller as the status and the error type an
// Anthropic SDK knows how to act on.
func TestErrorMappingTable(t *testing.T) {
	cases := []struct {
		code      ir.Code
		status    int
		errorType string
	}{
		{ir.CodeInvalidRequest, http.StatusBadRequest, "invalid_request_error"},
		{ir.CodeModelNotFound, http.StatusNotFound, "not_found_error"},
		{ir.CodeUnauthorized, http.StatusUnauthorized, "authentication_error"},
		{ir.CodeOverloaded, http.StatusTooManyRequests, "rate_limit_error"},
		{ir.CodeUpstreamUnavailable, http.StatusServiceUnavailable, "api_error"},
		{ir.CodeUpstreamError, http.StatusBadGateway, "api_error"},
		{ir.CodeTimeout, http.StatusGatewayTimeout, "api_error"},
		{ir.CodeInternal, http.StatusInternalServerError, "api_error"},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			backend := fake.New("fakecli", "", "small")
			backend.Fail = &ir.Error{Code: tc.code, Message: "backend says no"}
			rec := post(t, testHandler(t, backend),
				`{"model":"fakecli:small","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if got := decodeError(t, rec).Error.Type; got != tc.errorType {
				t.Errorf("type = %q, want %q", got, tc.errorType)
			}
		})
	}
}
