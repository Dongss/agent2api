// Package responses serves the OpenAI Responses API as a text-only endpoint.
//
// It is the third frontend, alongside openai (Chat Completions) and anthropic
// (Messages), and reads the same [ir] the other two do: no adapter knows this
// package exists.
//
// Two things about the dialect shape what is here. The Responses API is
// stateful by design — a client may send only the new turn and name the prior
// response with previous_response_id — and agent2api keeps no state, so that
// field is refused rather than approximated. And its output is a list of items
// rather than one message, which is where reasoning goes: its own item, ahead
// of the message, instead of a block inside it.
package responses

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Handler serves the Responses endpoint.
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

// Routes registers the Responses endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/responses", h.Responses)
}

// Responses implements POST /v1/responses.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()

	schema, err := req.schema()
	if err != nil {
		WriteError(w, err)
		return
	}
	if schema != "" && !res.EnforcesSchema(ctx) {
		WriteError(w, ir.InvalidRequest("text.format",
			"the %s backend cannot hold an answer to a schema; ask for a model whose CLI can, or drop text.format",
			res.Adapter.ID()))
		return
	}

	want, err := req.reasoningEffort()
	if err != nil {
		WriteError(w, err)
		return
	}
	effort, err := effortFor(ctx, res, want, "reasoning.effort")
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
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		Schema:      schema,
		Effort:      effort,
	}
	if req.User != "" {
		irReq.Metadata = map[string]string{"user": req.User}
	}

	events, err := res.Adapter.Run(ctx, irReq)
	if err != nil {
		WriteError(w, err)
		return
	}

	if req.Stream {
		h.streamResponse(w, r, req, events)
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

	out := req.shell(newID("resp"), h.now().Unix())
	out.Status = "completed"
	out.Output = outputItems(completion.Thinking, completion.Text)
	out.Usage = toUsage(completion.Usage)
	writeJSON(w, http.StatusOK, out)
}

// outputItems renders the response body. Reasoning, where the CLI exposed any,
// is its own item ahead of the message — the Responses arrangement, and not the
// same as the other two frontends, which nest it inside the message.
//
// The message item is always present, even for an empty answer, so a client
// that reaches for the last output item always finds one.
func outputItems(thinking, text string) []item {
	out := make([]item, 0, 2)
	if thinking != "" {
		out = append(out, reasoningItem(newID("rs"), thinking))
	}
	return append(out, messageItem(newID("msg"), text))
}

// shell builds the response object with everything that is known before the
// backend has said anything: the echoed request fields and this gateway's
// fixed answers. The caller fills in status, output and usage.
func (q *responsesRequest) shell(id string, created int64) response {
	return response{
		ID:        id,
		Object:    "response",
		CreatedAt: created,
		Model:     q.Model,
		Output:    []item{},

		Instructions:    q.Instructions,
		MaxOutputTokens: q.MaxOutputTokens,
		Temperature:     q.Temperature,
		TopP:            q.TopP,
		Metadata:        q.Metadata,
		// Refused above if it was set, so it is always absent by this point.
		PreviousResponseID: nil,
		// The caller may have asked for storage. It did not get any, and saying
		// so beats letting it assume a later retrieval will work.
		Store: false,

		ParallelToolCalls: false,
		ToolChoice:        "none",
		Tools:             []any{},
	}
}

func toUsage(u *ir.Usage) *usage {
	if u == nil {
		return nil
	}
	// The Responses input count is the whole prompt, with the cached part
	// called out separately rather than excluded — ir keeps them apart.
	in := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return &usage{
		InputTokens:        in,
		InputTokenDetails:  inputDetails{CachedTokens: u.CacheReadInputTokens},
		OutputTokens:       u.OutputTokens,
		OutputTokenDetails: outDetails{ReasoningTokens: u.ReasoningOutputTokens},
		TotalTokens:        in + u.OutputTokens,
	}
}

// decode reads and validates the request body.
func decode(r *http.Request) (*responsesRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		return nil, ir.InvalidRequest("", "cannot read the request body: %v", err)
	}
	if len(body) > MaxRequestBytes {
		return nil, ir.InvalidRequest("", "the request body exceeds %d bytes", MaxRequestBytes)
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, ir.InvalidRequest("", "the request body is not valid JSON: %v", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, ir.InvalidRequest("model", "a model is required")
	}
	if err := req.reject(); err != nil {
		return nil, err
	}
	return &req, nil
}

// reject refuses what this gateway cannot serve honestly, naming the field so
// the caller knows what to remove. Silently ignoring any of these would change
// the answer without saying so.
func (q *responsesRequest) reject() error {
	if q.PreviousResponseID != nil && *q.PreviousResponseID != "" {
		return ir.InvalidRequest("previous_response_id",
			"agent2api keeps no server-side state, so a prior response cannot be continued; send the whole conversation in `input` every time")
	}
	if !isEmptyJSON(q.Conversation) {
		return ir.InvalidRequest("conversation",
			"server-side conversations are not supported; send the whole conversation in `input` every time")
	}
	if !isEmptyJSON(q.Tools) {
		return ir.InvalidRequest("tools",
			"client tool calling is not supported; agent2api runs agent CLIs with their own tools disabled")
	}
	if !isEmptyJSON(q.ToolChoice) {
		return ir.InvalidRequest("tool_choice", "client tool calling is not supported")
	}
	if !isEmptyJSON(q.ParallelToolCalls) {
		return ir.InvalidRequest("parallel_tool_calls", "client tool calling is not supported")
	}
	if !isEmptyJSON(q.Background) {
		return ir.InvalidRequest("background",
			"background responses need server-side state, which agent2api does not keep")
	}
	return q.rejectInclude()
}

func (q *responsesRequest) rejectInclude() error {
	if isEmptyJSON(q.Include) {
		return nil
	}
	var include []string
	if err := json.Unmarshal(q.Include, &include); err != nil {
		return ir.InvalidRequest("include", "include must be an array of strings")
	}
	if len(include) == 0 {
		return nil
	}
	return ir.InvalidRequest("include",
		"none of the include options can be served by an agent CLI; drop %q", strings.Join(include, ", "))
}

// effortFor decodes a caller's reasoning-effort request against what the
// resolved backend takes.
//
// Two different refusals, deliberately: a level outside the vocabulary is the
// caller's mistake and needs no backend, while a level the backend has no knob
// for names that backend and lists what it does take. Neither rounds the
// request to a neighbouring level — `minimal` is not `low`, and a caller who
// asked for one did not ask for the other.
func effortFor(ctx context.Context, res router.Resolution, want, param string) (ir.Effort, error) {
	if want == "" {
		return "", nil
	}
	if !ir.ValidEffort(want) {
		return "", ir.InvalidRequest(param, "unknown reasoning effort %q; it must be one of %s",
			want, effortList(ir.Efforts))
	}
	levels := res.EffortLevels(ctx)
	if len(levels) == 0 {
		return "", ir.InvalidRequest(param,
			"the %s backend has no reasoning-effort setting; ask for a model whose CLI does, or drop %s",
			res.Adapter.ID(), param)
	}
	for _, l := range levels {
		if l == ir.Effort(want) {
			return l, nil
		}
	}
	return "", ir.InvalidRequest(param,
		"the %s backend does not take reasoning effort %q; it takes %s",
		res.Adapter.ID(), want, effortList(levels))
}

func effortList(levels []ir.Effort) string {
	out := make([]string, len(levels))
	for i, l := range levels {
		out[i] = string(l)
	}
	return strings.Join(out, ", ")
}

// reasoningEffort reads the effort out of the `reasoning` object.
func (q *responsesRequest) reasoningEffort() (string, error) {
	if isEmptyJSON(q.Reasoning) {
		return "", nil
	}
	var r reasoningRequest
	if err := json.Unmarshal(q.Reasoning, &r); err != nil {
		return "", ir.InvalidRequest("reasoning", "reasoning must be an object")
	}
	return r.Effort, nil
}

// schema reads `text.format`, returning the JSON Schema the answer must conform
// to, or "" when the caller asked for ordinary text. `text` also carries
// unrelated knobs like verbosity, so an absent format is not an error.
//
// Whether a schema can actually be served depends on the backend, so this only
// decodes; the caller checks the capability once the model has resolved.
func (q *responsesRequest) schema() (string, error) {
	if isEmptyJSON(q.Text) {
		return "", nil
	}
	var t textFormat
	if err := json.Unmarshal(q.Text, &t); err != nil {
		return "", ir.InvalidRequest("text", "text must be an object")
	}
	switch t.Format.Type {
	case "", "text":
		return "", nil
	case "json_object":
		// See the same case in the openai frontend: an open object schema is a
		// different request, and the model answers it by inventing a wrapper.
		return "", ir.InvalidRequest("text.format.type", "`json_object` asks for JSON of no particular shape, which the backend cannot be held to: its schema flag needs a shape, and an open schema makes the model invent a wrapper key. Send `json_schema` with the shape you want")
	case "json_schema":
		// Responses puts the schema inline in the format object, where chat
		// completions nests it under json_schema.
		if isEmptyJSON(t.Format.Schema) {
			return "", ir.InvalidRequest("text.format.schema", "a schema is required")
		}
		return string(t.Format.Schema), nil
	default:
		return "", ir.InvalidRequest("text.format.type", "unsupported output format %q", t.Format.Type)
	}
}

// toMessages converts the request into IR.
//
// instructions leads, because it is the system prompt and the Responses API
// puts it outside the input list. system and developer items inside the list
// keep their position among the turns, as they do in the other frontends.
func (q *responsesRequest) toMessages() ([]ir.Message, error) {
	var out []ir.Message
	if q.Instructions != nil && *q.Instructions != "" {
		out = append(out, ir.Message{Role: ir.RoleSystem, Content: *q.Instructions})
	}

	items, err := q.inputItems()
	if err != nil {
		return nil, err
	}
	for i, it := range items {
		param := fmt.Sprintf("input[%d]", i)
		msg, err := itemToMessage(it, param)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}

	if !hasRole(out, ir.RoleUser) && !hasRole(out, ir.RoleAssistant) {
		return nil, ir.InvalidRequest("input", "the conversation needs at least one user message")
	}
	return out, nil
}

// inputItems normalises the two shapes `input` takes: a bare string, which is
// the whole user turn, or a list of items.
func (q *responsesRequest) inputItems() ([]inputItem, error) {
	if isEmptyJSON(q.Input) {
		return nil, ir.InvalidRequest("input", "input is required")
	}
	var s string
	if err := json.Unmarshal(q.Input, &s); err == nil {
		return []inputItem{{Type: "message", Role: "user", Content: mustJSON(s)}}, nil
	}
	var items []inputItem
	if err := json.Unmarshal(q.Input, &items); err != nil {
		return nil, ir.InvalidRequest("input", "input must be a string or an array of items")
	}
	return items, nil
}

func itemToMessage(it inputItem, param string) (ir.Message, error) {
	switch it.Type {
	case "", "message":
	case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
		return ir.Message{}, ir.InvalidRequest(param+".type",
			"tool calls cannot be replayed: agent2api does not support client tool calling")
	case "reasoning":
		// Clients echo prior reasoning items back so the model can pick up its
		// own chain of thought. There is nothing to hand a fresh CLI process.
		return ir.Message{}, ir.InvalidRequest(param+".type",
			"reasoning items cannot be replayed: every request starts a fresh CLI process with no prior reasoning to resume")
	default:
		return ir.Message{}, ir.InvalidRequest(param+".type", "unsupported input item type %q", it.Type)
	}

	var role ir.Role
	switch it.Role {
	case "system", "developer":
		role = ir.RoleSystem
	case "user":
		role = ir.RoleUser
	case "assistant":
		role = ir.RoleAssistant
	case "":
		return ir.Message{}, ir.InvalidRequest(param+".role", "a role is required")
	default:
		return ir.Message{}, ir.InvalidRequest(param+".role", "unknown role %q", it.Role)
	}

	text, err := decodeContent(it.Content, param)
	if err != nil {
		return ir.Message{}, err
	}
	return ir.Message{Role: role, Content: text}, nil
}

// decodeContent accepts both content shapes: a plain string, or an array of
// typed parts.
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
		case "input_text", "output_text", "text", "":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		case "input_image", "input_file", "image", "file":
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d]", param, i),
				"image and file input are not supported; agent CLIs are driven as text-only models here")
		default:
			return "", ir.InvalidRequest(fmt.Sprintf("%s.content[%d].type", param, i),
				"unsupported content part type %q", p.Type)
		}
	}
	return b.String(), nil
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
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// v is always a string here; a failure would be a programming error.
		panic(err)
	}
	return b
}

// newID builds an identifier with the Responses prefix for its kind.
func newID(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return prefix + "_agent2api"
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

// WriteError renders err in OpenAI's error shape with the matching status.
//
// The mapping is a copy of the openai frontend's rather than a shared helper:
// the two dialects agree today, and a frontend that imports another frontend to
// save twenty lines is a worse trade than the duplication. TestErrorShape
// pins them together.
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

func errorBodyOf(e *ir.Error) errBody {
	return errBody{
		Message: errorMessage(e),
		Type:    errorType(e.Code),
		Param:   e.Param,
		Code:    errorCode(e.Code),
	}
}

func errorMessage(e *ir.Error) string {
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

// errAborted stops the relay after the failure has already been reported as an
// HTTP status, so nothing more is written to the response.
var errAborted = errors.New("responses: stream aborted before it started")
