package anthropic

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

// Handler serves the Anthropic-compatible endpoints.
type Handler struct {
	Router *router.Router
	Log    *slog.Logger
	// Heartbeat is how often a silent stream emits a ping. Zero disables
	// pings; [New] starts it at [sse.DefaultHeartbeat].
	Heartbeat time.Duration
	// Now is injectable so tests get stable timestamps.
	Now func() time.Time
}

// New builds a handler.
func New(r *router.Router, log *slog.Logger) *Handler {
	return &Handler{Router: r, Log: log, Heartbeat: sse.DefaultHeartbeat, Now: time.Now}
}

// Routes registers the Anthropic endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", h.Messages)
}

// Messages implements POST /v1/messages.
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
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
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		irReq.Metadata = map[string]string{"user": req.Metadata.UserID}
	}

	ctx := r.Context()
	events, err := res.Adapter.Run(ctx, irReq)
	if err != nil {
		WriteError(w, err)
		return
	}

	if req.Stream {
		h.streamMessage(w, r, req, events)
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

	stop := completion.StopReason
	if stop == "" {
		stop = ir.StopEndTurn
	}
	writeJSON(w, http.StatusOK, message{
		ID:           newID(),
		Type:         "message",
		Role:         "assistant",
		Model:        req.Model,
		Content:      blocks(completion.Thinking, completion.Text),
		StopReason:   stopReason(stop),
		StopSequence: nil,
		Usage:        toUsage(completion.Usage),
	})
}

// blocks renders the response content. Thinking comes first, as it does in the
// vendor API, and an empty answer still yields one empty text block so clients
// that index into content[0] keep working.
func blocks(thinking, text string) []block {
	var out []block
	if thinking != "" {
		out = append(out, thinkingBlock(thinking))
	}
	return append(out, textBlock(text))
}

// decode reads and validates the request body.
func decode(r *http.Request) (*messagesRequest, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil, ir.InvalidRequest("", "Content-Type must be application/json, got %q", ct)
	}
	body := http.MaxBytesReader(nil, r.Body, MaxRequestBytes)
	var req messagesRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
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

// validate refuses what agent2api cannot honestly serve.
func (m *messagesRequest) validate() error {
	if strings.TrimSpace(m.Model) == "" {
		return ir.InvalidRequest("model", "a model name is required")
	}
	if len(m.Messages) == 0 {
		return ir.InvalidRequest("messages", "at least one message is required")
	}
	if len(m.StopSequences) > 0 {
		return ir.InvalidRequest("stop_sequences",
			"stop sequences are not supported: an agent CLI cannot enforce them, and returning text the caller asked to have cut would be worse than refusing")
	}
	if !isEmptyJSON(m.Tools) {
		return ir.InvalidRequest("tools",
			"client tool calling is not supported; agent2api runs agent CLIs with their own tools disabled")
	}
	if !isEmptyJSON(m.ToolChoice) {
		return ir.InvalidRequest("tool_choice", "client tool calling is not supported")
	}
	return nil
}

// toMessages converts the wire request into IR. The system field becomes a
// leading system message: the prompt renderer routes it to the CLI's own
// system-prompt flag where one exists.
func (m *messagesRequest) toMessages() ([]ir.Message, error) {
	var out []ir.Message

	system, err := decodeSystem(m.System)
	if err != nil {
		return nil, err
	}
	if system != "" {
		out = append(out, ir.Message{Role: ir.RoleSystem, Content: system})
	}

	for i, msg := range m.Messages {
		param := fmt.Sprintf("messages[%d]", i)
		var role ir.Role
		switch msg.Role {
		case "user":
			role = ir.RoleUser
		case "assistant":
			role = ir.RoleAssistant
		case "system":
			return nil, ir.InvalidRequest(param+".role",
				`the Messages API takes the system prompt in the top-level "system" field, not as a message`)
		default:
			return nil, ir.InvalidRequest(param+".role", "unknown role %q", msg.Role)
		}
		text, err := decodeContent(msg.Content, param)
		if err != nil {
			return nil, err
		}
		out = append(out, ir.Message{Role: role, Content: text})
	}

	return out, nil
}

// decodeSystem accepts both spellings of the system field: a plain string, or
// an array of text blocks (which is how prompt caching is expressed).
func decodeSystem(raw json.RawMessage) (string, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []inputBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", ir.InvalidRequest("system", "system must be a string or an array of text blocks")
	}
	texts := make([]string, 0, len(parts))
	for i, p := range parts {
		if p.Type != "text" && p.Type != "" {
			return "", ir.InvalidRequest(fmt.Sprintf("system[%d].type", i),
				"only text blocks are supported in system, got %q", p.Type)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}

// decodeContent flattens a message's content to text, refusing anything that
// cannot survive the trip to a text-only CLI. Thinking blocks replayed from an
// earlier turn are dropped: they are the model's scratch space, not part of the
// conversation the next turn needs.
func decodeContent(raw json.RawMessage, param string) (string, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []inputBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", ir.InvalidRequest(param+".content", "content must be a string or an array of content blocks")
	}
	var b strings.Builder
	for i, p := range parts {
		switch p.Type {
		case "text", "":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		case "thinking", "redacted_thinking":
			// Deliberately dropped, see above.
		case "image", "document":
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d]", param, i),
				"%s input is not supported yet; agent CLIs are driven as text-only models here", p.Type)
		case "tool_use", "tool_result", "server_tool_use":
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d]", param, i),
				"tool blocks cannot be replayed: agent2api does not support client tool calling")
		default:
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d].type", param, i),
				"unsupported content block type %q", p.Type)
		}
	}
	return b.String(), nil
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// stopReason maps the IR vocabulary onto Anthropic's, which is where it came
// from; an unfinished run reports no reason at all, like the vendor API.
func stopReason(stop ir.StopReason) *string {
	if stop == "" {
		return nil
	}
	s := string(stop)
	return &s
}

func toUsage(u *ir.Usage) usage {
	if u == nil {
		return usage{}
	}
	return usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
}

func newID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "msg_agent2api"
	}
	return "msg_" + hex.EncodeToString(buf[:])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError renders err in Anthropic's error shape with the matching status.
func WriteError(w http.ResponseWriter, err error) {
	e := ir.AsError(err)
	// The retry hint is what the SDKs read to decide how long to back off.
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	writeJSON(w, e.Code.HTTPStatus(), envelopeOf(e))
}

// envelopeOf renders an error as Anthropic's error object, for a response body
// or for the error event of a stream.
func envelopeOf(e *ir.Error) errorEnvelope {
	return errorEnvelope{Type: "error", Error: errorBody{
		Type:    errorType(e.Code),
		Message: describe(e),
	}}
}

// describe folds the operator-facing detail into the message: this is a local
// gateway, and hiding the CLI's own complaint helps nobody.
func describe(e *ir.Error) string {
	msg := e.Message
	if e.Param != "" {
		msg = e.Param + ": " + msg
	}
	if e.Detail == "" {
		return msg
	}
	detail := e.Detail
	const maxDetail = 2000
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	return msg + ": " + detail
}

func errorType(code ir.Code) string {
	switch code {
	case ir.CodeInvalidRequest:
		return "invalid_request_error"
	case ir.CodeModelNotFound:
		return "not_found_error"
	case ir.CodeUnauthorized:
		return "authentication_error"
	case ir.CodeOverloaded:
		return "rate_limit_error"
	default:
		// Anthropic has no type for a failing backend; api_error is what its
		// own 5xx responses use.
		return "api_error"
	}
}
