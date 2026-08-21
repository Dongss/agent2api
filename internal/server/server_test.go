package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A stub adapter registered under a known id lets the server be built without
// any real CLI installed. "codex" stays unregistered on purpose, so the
// skipped-adapter path is exercised too.
func init() {
	adapter.Register("test-stub", func(opts adapter.Options) (adapter.Adapter, error) {
		return fake.New("test-stub", "hello"), nil
	})
	// Stands in for an installed-but-broken CLI: it builds, then fails to probe.
	adapter.Register("test-broken", func(opts adapter.Options) (adapter.Adapter, error) {
		a := fake.New("test-broken", "")
		a.ProbeErr = errors.New("the CLI is not logged in")
		return a, nil
	})
	config.RegisterAdapterID("test-broken")
}

func testServerConfig() *config.Config {
	return &config.Config{
		Server: config.Server{
			Host: "127.0.0.1", Port: 0,
			QueueTimeout:   1,
			MaxConcurrency: 2,
		},
		Log:      config.Log{Level: "error", Format: "text"},
		Adapters: map[string]config.Adapter{"test-stub": {Binary: "stub"}},
	}
}

func TestHealthz(t *testing.T) {
	srv, err := New(testServerConfig(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestAuthEnforcement(t *testing.T) {
	cfg := testServerConfig()
	cfg.Server.APIKey = "sk-right"
	srv, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	cases := []struct {
		name   string
		header [2]string
		status int
	}{
		{"no key", [2]string{"", ""}, http.StatusUnauthorized},
		{"wrong key", [2]string{"Authorization", "Bearer sk-wrong"}, http.StatusUnauthorized},
		{"bearer", [2]string{"Authorization", "Bearer sk-right"}, http.StatusOK},
		{"anthropic style", [2]string{"x-api-key", "sk-right"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.header[0] != "" {
				req.Header.Set(tc.header[0], tc.header[1])
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}

	// Health checks stay reachable so a supervisor does not need the key.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want it exempt from auth", rec.Code)
	}
}

func TestUnknownAdapterIsSkippedNotFatal(t *testing.T) {
	cfg := testServerConfig()
	cfg.Adapters["codex"] = config.Adapter{Binary: "codex"}
	srv, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("an unimplemented adapter must not stop startup: %v", err)
	}
	skipped := srv.Skipped()
	if len(skipped) != 1 || skipped[0].ID != "codex" {
		t.Errorf("skipped = %+v, want codex", skipped)
	}
}

func TestNoUsableAdaptersIsFatal(t *testing.T) {
	cfg := testServerConfig()
	cfg.Adapters = map[string]config.Adapter{"codex": {Binary: "codex"}}
	if _, err := New(cfg, discardLogger()); err == nil {
		t.Fatal("a config with nothing servable must fail at startup")
	}
}

// A backend whose CLI is missing is skipped, not fatal — having only some of
// these installed is the normal case.
func TestUnhealthyAdapterIsSkipped(t *testing.T) {
	cfg := testServerConfig()
	cfg.Adapters["test-broken"] = config.Adapter{Binary: "broken"}
	srv, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("a backend that fails to probe must not stop startup: %v", err)
	}
	var found bool
	for _, sk := range srv.Skipped() {
		if sk.ID == "test-broken" {
			found = true
		}
	}
	if !found {
		t.Errorf("skipped = %+v, want test-broken", srv.Skipped())
	}
}

// With no backend usable there is nothing to serve, so startup fails rather
// than standing up a gateway that answers every request with a 404.
func TestNoUsableBackendIsFatal(t *testing.T) {
	cfg := testServerConfig()
	cfg.Adapters = map[string]config.Adapter{"test-broken": {Binary: "broken"}}
	if _, err := New(cfg, discardLogger()); err == nil {
		t.Fatal("a config whose every backend fails to probe must not start")
	}
}

func TestUnknownPathAndAnthropicDialect(t *testing.T) {
	srv, err := New(testServerConfig(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown endpoint") {
		t.Errorf("unknown path: status %d, body %s", rec.Code, rec.Body)
	}

	// The Anthropic endpoint is served, and its errors come back in that
	// dialect's shape rather than OpenAI's.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("/v1/messages with no model: status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["type"] != "error" {
		t.Errorf("the Anthropic endpoint should answer in that dialect's error shape: %s", rec.Body)
	}

	// A live request must reach the same backend the OpenAI frontend uses.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"test-stub:m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/messages status = %d, body = %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["type"] != "message" || body["role"] != "assistant" {
		t.Errorf("unexpected response envelope: %s", rec.Body)
	}

	// An unknown path under /v1/messages must not answer in the other dialect.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["type"] != "error" {
		t.Errorf("unknown Anthropic path answered in the wrong dialect: %s", rec.Body)
	}
}

func TestGracefulShutdown(t *testing.T) {
	srv, err := New(testServerConfig(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// The listener must actually be serving before we ask it to stop.
	url := "http://" + ln.Addr().String() + "/healthz"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context was canceled")
	}
}
