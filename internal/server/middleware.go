package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Dongss/agent2api/internal/frontend/anthropic"
	"github.com/Dongss/agent2api/internal/frontend/openai"
	"github.com/Dongss/agent2api/internal/ir"
)

// writeError renders err in the dialect the requested path belongs to, so an
// Anthropic SDK never has to parse an OpenAI error object, or the reverse.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		anthropic.WriteError(w, err)
		return
	}
	openai.WriteError(w, err)
}

type middleware func(http.Handler) http.Handler

// chain applies middleware so the first listed runs outermost.
func chain(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// statusRecorder remembers what was written, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which the
// streaming frontends will need.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// maxRequestIDBytes bounds an id a client supplied, so a hostile or careless
// header cannot bloat every log line that mentions it.
const maxRequestIDBytes = 128

// requestID stamps every response in both spellings the SDKs surface: OpenAI
// reads x-request-id, Anthropic reads request-id. An id a client reports can
// then be matched against the access log.
//
// An id the client supplied is kept, so existing request tracing still lines up.
func requestID(r *http.Request, w http.ResponseWriter) string {
	id := sanitizeID(r.Header.Get("x-request-id"))
	if id == "" {
		id = newRequestID()
	}
	w.Header().Set("x-request-id", id)
	w.Header().Set("request-id", id)
	return id
}

func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(buf[:])
}

// sanitizeID keeps only characters that are safe in a header and readable in a
// log line, and bounds the length.
func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxRequestIDBytes {
		s = s[:maxRequestIDBytes]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == ':':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// requestLog records one line per request. Prompt content is never logged, at
// any level: this gateway sees whole conversations.
func requestLog(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := requestID(r, w)
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			log.Log(r.Context(), level, "request",
				"id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration", time.Since(start).Round(time.Millisecond).String(),
			)
		})
	}
}

// recovery turns a panic into a 500 instead of a dropped connection.
func recovery(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic serving request",
						"path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
					writeError(w, r, ir.Errorf(ir.CodeInternal, "internal error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// auth enforces the static bearer key, when one is configured. Both the
// OpenAI (Authorization: Bearer) and Anthropic (x-api-key) spellings are
// accepted so either SDK works unmodified.
func auth(key string) middleware {
	return func(next http.Handler) http.Handler {
		if key == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			if !authorized(r, key) {
				openai.WriteError(w, &ir.Error{
					Code:    ir.CodeUnauthorized,
					Message: "missing or invalid API key",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func authorized(r *http.Request, key string) bool {
	presented := strings.TrimSpace(r.Header.Get("x-api-key"))
	if presented == "" {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
			presented = strings.TrimSpace(header[7:])
		}
	}
	if presented == "" {
		return false
	}
	// Constant-time, so a wrong guess cannot be refined byte by byte.
	return subtle.ConstantTimeCompare([]byte(presented), []byte(key)) == 1
}

// notFound answers unknown paths, which is what a misconfigured base_url most
// often hits, in whichever dialect the path suggests.
func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, &ir.Error{
		Code: ir.CodeInvalidRequest,
		Message: "unknown endpoint " + r.Method + " " + r.URL.Path +
			"; agent2api serves POST /v1/chat/completions, POST /v1/messages and GET /v1/models",
	})
}
