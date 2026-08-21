// Package ir defines the protocol-neutral internal representation that sits
// between API frontends (OpenAI, Anthropic) and agent CLI adapters.
//
// Frontends decode wire requests into an [Request]; adapters translate CLI
// output into a stream of [Event]s; frontends encode those events back into
// their own dialect. Neither side knows about the other.
package ir

// Role identifies the author of a [Message].
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn of the conversation. Content is plain text; the
// frontends flatten richer wire formats (content part arrays) down to text and
// reject anything that cannot be represented, such as images.
type Message struct {
	Role    Role
	Content string
}

// Request is a protocol-neutral completion request. It carries the entire
// conversation: agent2api keeps no server-side state, so every request must be
// self-contained.
type Request struct {
	// Model is the resolved, namespaced backend model, e.g. "claude-code:opus".
	Model string
	// Adapter is the adapter id the request was routed to, e.g. "claude-code".
	Adapter string
	// Variant is the adapter-specific model name, e.g. "opus".
	Variant string

	Messages []Message
	Stream   bool

	// MaxTokens and Temperature are best-effort: most CLIs cannot enforce them
	// and adapters are free to ignore them.
	MaxTokens   *int
	Temperature *float64

	// Metadata carries non-essential request annotations (e.g. the caller's
	// "user" field). Never used for routing.
	Metadata map[string]string
}
