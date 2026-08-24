package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Dongss/agent2api/internal/frontend/sse"
	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/router"
)

// MaxRequestBytes caps the request body. A replayed conversation is the payload
// here, so the limit is generous, but not unbounded.
const MaxRequestBytes = 32 << 20 // 32 MiB

// Handler serves the OpenAI-compatible endpoints.
type Handler struct {
	Router *router.Router
	Log    *slog.Logger
	// Heartbeat is how often a silent stream emits a keepalive comment. Zero
	// disables keepalives; [New] starts it at [sse.DefaultHeartbeat].
	Heartbeat time.Duration
	// Now is injectable so tests get stable timestamps.
	Now func() time.Time
}

// New builds a handler.
func New(r *router.Router, log *slog.Logger) *Handler {
	return &Handler{Router: r, Log: log, Heartbeat: sse.DefaultHeartbeat, Now: time.Now}
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Routes registers the OpenAI endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.ChatCompletions)
	mux.HandleFunc("GET /v1/models", h.Models)
	mux.HandleFunc("GET /v1/models/{model}", h.Model)
}

// Models implements GET /v1/models: one row per backend being served.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	created := h.now().Unix()
	list := modelList{Object: "list", Data: []modelItem{}}
	for _, e := range h.Router.Entries() {
		list.Data = append(list.Data, modelItem{
			ID:      e.ID,
			Object:  "model",
			Created: created,
			OwnedBy: e.Adapter,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// Model implements GET /v1/models/{model}.
func (h *Handler) Model(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("model")
	res, err := h.Router.Resolve(name)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelItem{
		ID:      name,
		Object:  "model",
		Created: h.now().Unix(),
		OwnedBy: res.Adapter.ID(),
	})
}

// ChatCompletions implements POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	req, err := decode(r)
	if err != nil {
		WriteError(w, err)
		return
	}

	res, err := h.Router.Resolve(req.Model)
	if err != nil {
		WriteError(w, err)
		return
	}

	messages, err := req.toMessages()
	if err != nil {
		WriteError(w, err)
		return
	}

	irReq := ir.Request{
		Model:       res.Model,
		Adapter:     res.Adapter.ID(),
		Variant:     res.Variant,
		Messages:    messages,
		Stream:      req.Stream,
		MaxTokens:   req.maxTokens(),
		Temperature: req.Temperature,
	}
	if req.User != "" {
		irReq.Metadata = map[string]string{"user": req.User}
	}

	ctx := r.Context()
	events, err := res.Adapter.Run(ctx, irReq)
	if err != nil {
		WriteError(w, err)
		return
	}

	if req.Stream {
		h.streamCompletion(w, r, req, events)
		return
	}

	completion, err := ir.Collect(ctx, events)
	if err != nil {
		if e := ir.AsError(err); e.Code == ir.CodeCanceled {
			// The client hung up; there is nobody left to answer.
			h.Log.Debug("client disconnected", "model", req.Model)
			return
		}
		WriteError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, chatCompletion{
		ID:      newID(),
		Object:  "chat.completion",
		Created: h.now().Unix(),
		Model:   req.Model,
		Choices: []choice{{
			Index: 0,
			Message: replyMessage{
				Role:             "assistant",
				Content:          completion.Text,
				ReasoningContent: completion.Thinking,
			},
			FinishReason: finishReason(completion.StopReason),
		}},
		Usage: toUsage(completion.Usage),
	})
}

// decode reads and validates the request body.
func decode(r *http.Request) (*chatRequest, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil, ir.InvalidRequest("", "Content-Type must be application/json, got %q", ct)
	}
	body := http.MaxBytesReader(nil, r.Body, MaxRequestBytes)
	dec := json.NewDecoder(body)
	var req chatRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, ir.InvalidRequest("", "request body exceeds %d bytes", MaxRequestBytes)
		}
		return nil, ir.InvalidRequest("", "request body is not valid JSON: %v", err)
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

// validate refuses what agent2api cannot honestly serve. Silently ignoring
// these would give callers wrong answers with no way to tell.
func (c *chatRequest) validate() error {
	if strings.TrimSpace(c.Model) == "" {
		return ir.InvalidRequest("model", "a model name is required")
	}
	if len(c.Messages) == 0 {
		return ir.InvalidRequest("messages", "at least one message is required")
	}
	if c.StreamOptions != nil && !c.Stream {
		return ir.InvalidRequest("stream_options", "stream_options is only meaningful with \"stream\": true")
	}
	if !isEmptyJSON(c.Stop) && strings.TrimSpace(string(c.Stop)) != "[]" {
		return ir.InvalidRequest("stop",
			"stop sequences are not supported: an agent CLI cannot enforce them, and returning text the caller asked to have cut would be worse than refusing")
	}
	if c.N != nil && *c.N != 1 {
		return ir.InvalidRequest("n", "only n=1 is supported; an agent CLI produces one completion per run")
	}
	if c.Logprobs != nil && *c.Logprobs {
		return ir.InvalidRequest("logprobs", "log probabilities are not available from an agent CLI")
	}
	if c.TopLogprobs != nil {
		return ir.InvalidRequest("top_logprobs", "log probabilities are not available from an agent CLI")
	}
	if !isEmptyJSON(c.Tools) || !isEmptyJSON(c.Functions) {
		return ir.InvalidRequest("tools",
			"client tool calling is not supported; agent2api runs agent CLIs with their own tools disabled")
	}
	if !isEmptyJSON(c.ToolChoice) || !isEmptyJSON(c.FunctionCall) {
		return ir.InvalidRequest("tool_choice", "client tool calling is not supported")
	}
	return nil
}

// toMessages converts the wire messages into IR, flattening content parts and
// refusing anything that cannot survive the trip to a text-only CLI.
func (c *chatRequest) toMessages() ([]ir.Message, error) {
	out := make([]ir.Message, 0, len(c.Messages))
	for i, m := range c.Messages {
		param := fmt.Sprintf("messages[%d]", i)

		var role ir.Role
		switch m.Role {
		case "system", "developer":
			role = ir.RoleSystem
		case "user":
			role = ir.RoleUser
		case "assistant":
			role = ir.RoleAssistant
		case "tool", "function":
			return nil, ir.InvalidRequest(param+".role",
				"tool results cannot be replayed: agent2api does not support client tool calling")
		default:
			return nil, ir.InvalidRequest(param+".role", "unknown role %q", m.Role)
		}
		if !isEmptyJSON(m.ToolCalls) {
			return nil, ir.InvalidRequest(param+".tool_calls", "client tool calling is not supported")
		}

		text, err := decodeContent(m.Content, param)
		if err != nil {
			return nil, err
		}
		out = append(out, ir.Message{Role: role, Content: text})
	}

	if !hasRole(out, ir.RoleUser) && !hasRole(out, ir.RoleAssistant) {
		return nil, ir.InvalidRequest("messages", "the conversation needs at least one user message")
	}
	return out, nil
}

// decodeContent accepts both content shapes OpenAI defines: a plain string, or
// an array of typed parts.
func decodeContent(raw json.RawMessage, param string) (string, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", ir.InvalidRequest(param+".content", "content must be a string or an array of content parts")
	}
	var b strings.Builder
	for i, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text", "":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		case "image_url", "input_image", "image":
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d]", param, i),
				"image input is not supported yet; agent CLIs are driven as text-only models here")
		default:
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d].type", param, i),
				"unsupported content part type %q", p.Type)
		}
	}
	return b.String(), nil
}

// maxTokens folds OpenAI's two spellings into one best-effort hint.
func (c *chatRequest) maxTokens() *int {
	if c.MaxCompletionTokens != nil {
		return c.MaxCompletionTokens
	}
	return c.MaxTokens
}

// includeUsage reports whether the caller asked for a usage frame at the end of
// the stream. OpenAI omits usage from streams unless this is set.
func (c *chatRequest) includeUsage() bool {
	return c.StreamOptions != nil && c.StreamOptions.IncludeUsage
}

func hasRole(msgs []ir.Message, role ir.Role) bool {
	for _, m := range msgs {
		if m.Role == role {
			return true
		}
	}
	return false
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func finishReason(stop ir.StopReason) string {
	switch stop {
	case ir.StopMaxTokens:
		return "length"
	default:
		return "stop"
	}
}

func toUsage(u *ir.Usage) *usage {
	if u == nil {
		return nil
	}
	// Cached input tokens are input tokens the model still had to be given;
	// counting them keeps prompt_tokens comparable with the vendor API.
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return &usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
}

func newID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "chatcmpl-agent2api"
	}
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

// WriteError renders err in OpenAI's error shape with the matching status.
func WriteError(w http.ResponseWriter, err error) {
	e := ir.AsError(err)
	retryAfter(w, e)
	writeJSON(w, e.Code.HTTPStatus(), errorEnvelope{Error: errorBodyOf(e)})
}

// retryAfter passes the backend's retry hint on to the client, which is what
// the SDKs read to decide how long to back off.
func retryAfter(w http.ResponseWriter, e *ir.Error) {
	if e.RetryAfter <= 0 {
		return
	}
	seconds := int(math.Ceil(e.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

// errorBodyOf renders an error as OpenAI's error object, for a response body or
// for an in-stream error frame.
func errorBodyOf(e *ir.Error) errorBody {
	return errorBody{
		Message: message(e),
		Type:    errorType(e.Code),
		Param:   e.Param,
		Code:    errorCode(e.Code),
	}
}

func message(e *ir.Error) string {
	if e.Detail == "" {
		return e.Message
	}
	detail := e.Detail
	const maxDetail = 2000
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	return e.Message + ": " + detail
}

func errorType(code ir.Code) string {
	switch code {
	case ir.CodeInvalidRequest, ir.CodeModelNotFound:
		return "invalid_request_error"
	case ir.CodeUnauthorized:
		return "authentication_error"
	case ir.CodeOverloaded:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

func errorCode(code ir.Code) string {
	switch code {
	case ir.CodeModelNotFound:
		return "model_not_found"
	case ir.CodeUnauthorized:
		return "invalid_api_key"
	case ir.CodeOverloaded:
		return "rate_limit_exceeded"
	case ir.CodeUpstreamUnavailable:
		return "service_unavailable"
	case ir.CodeTimeout:
		return "timeout"
	case ir.CodeUpstreamError:
		return "upstream_error"
	default:
		return ""
	}
}
