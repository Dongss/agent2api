package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Both vendor SDKs surface a request id to their callers, each under its own
// header name, and both are what a user quotes in a bug report.
func TestRequestIDHeaders(t *testing.T) {
	srv, err := New(testServerConfig(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	id := rec.Header().Get("x-request-id")
	if id == "" {
		t.Fatal("no x-request-id on the response")
	}
	if got := rec.Header().Get("request-id"); got != id {
		t.Errorf("request-id = %q, x-request-id = %q; both must carry the same id", got, id)
	}
	if !strings.HasPrefix(id, "req_") {
		t.Errorf("generated id = %q, want a recognizable prefix", id)
	}

	// A client that already has request tracing keeps its own id.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-request-id", "caller-123")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("x-request-id"); got != "caller-123" {
		t.Errorf("x-request-id = %q, want the caller's own id", got)
	}

	// But not one that would pollute a log line.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-request-id", "ok\tpart status=200 injected")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	got := rec.Header().Get("x-request-id")
	if strings.ContainsAny(got, " \t=") {
		t.Errorf("x-request-id = %q, want the unsafe characters dropped", got)
	}
	if got == "" {
		t.Error("a sanitized id must still be present")
	}
}
