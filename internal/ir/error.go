package ir

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DefaultRateLimitRetry is the retry hint attached to an upstream rate limit.
// The CLIs do not say how long to wait, and both vendor SDKs back off on their
// own, but a hint keeps a client from hammering a limit it has just hit.
const DefaultRateLimitRetry = 10 * time.Second

// Code is a protocol-neutral error class. Frontends translate it into their own
// dialect's error shape; the HTTP status comes from [Code.HTTPStatus].
type Code string

const (
	// CodeInvalidRequest — the caller sent something we cannot serve (400).
	CodeInvalidRequest Code = "invalid_request"
	// CodeModelNotFound — no adapter serves the requested model (404).
	CodeModelNotFound Code = "model_not_found"
	// CodeUnauthorized — the gateway's own bearer key check failed (401).
	CodeUnauthorized Code = "unauthorized"
	// CodeOverloaded — no concurrency slot became free in time (429).
	CodeOverloaded Code = "overloaded"
	// CodeUpstreamUnavailable — the CLI is missing or not logged in (503).
	CodeUpstreamUnavailable Code = "upstream_unavailable"
	// CodeUpstreamError — the CLI ran but failed (502).
	CodeUpstreamError Code = "upstream_error"
	// CodeTimeout — the CLI exceeded a request or idle deadline (504).
	CodeTimeout Code = "timeout"
	// CodeCanceled — the client went away before the response was complete.
	CodeCanceled Code = "canceled"
	// CodeInternal — a bug in agent2api (500).
	CodeInternal Code = "internal"
)

// HTTPStatus maps a code to the status frontends should return.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeModelNotFound:
		return http.StatusNotFound
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeOverloaded:
		return http.StatusTooManyRequests
	case CodeUpstreamUnavailable:
		return http.StatusServiceUnavailable
	case CodeUpstreamError:
		return http.StatusBadGateway
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeCanceled:
		return 499 // nginx's "client closed request"; never actually written
	}
	return http.StatusInternalServerError
}

// Error is an agent2api error carrying enough context for a frontend to render
// an actionable message. Detail holds operator-facing context (a stderr tail,
// for instance) and is included in the response body: this is a local,
// single-user gateway, and hiding the CLI's own complaint helps nobody.
type Error struct {
	Code    Code
	Message string
	// Param names the offending request field, when there is one.
	Param string
	// Detail is extra diagnostic context, e.g. the tail of the CLI's stderr.
	Detail string
	// RetryAfter, when set, is rendered as the Retry-After header. Both vendor
	// SDKs honour it when deciding how long to back off.
	RetryAfter time.Duration
	// Err is the wrapped cause, if any.
	Err error
}

func (e *Error) Error() string {
	msg := string(e.Code) + ": " + e.Message
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an [Error] with a formatted message.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// InvalidRequest builds a 400 tied to a specific request field.
func InvalidRequest(param, format string, args ...any) *Error {
	return &Error{Code: CodeInvalidRequest, Param: param, Message: fmt.Sprintf(format, args...)}
}

// AsError extracts an [*Error] from err, wrapping anything unrecognized as an
// internal error so callers always have something to render.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error(), Err: err}
}
