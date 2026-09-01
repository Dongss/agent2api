package config

// Built-in default values. They are named constants rather than literals buried
// in [Defaults] so that the ones worth citing elsewhere — a message, a doc
// comment, a test — have somewhere to be cited from.
const (
	// DefaultHost keeps the listener on this machine. Binding anywhere else
	// requires a bearer key; see [Config.Validate].
	DefaultHost = "127.0.0.1"
	DefaultPort = 8055

	// The deadlines are in seconds; see [Seconds] for why they carry no unit.
	//
	// They are generous because a request here is a whole CLI run: a tool-less
	// turn still takes seconds, and a long answer takes minutes.
	DefaultRequestTimeout = 600 * Second
	DefaultIdleTimeout    = 120 * Second
	DefaultQueueTimeout   = 30 * Second
	// DefaultHeartbeatInterval is the SSE keepalive cadence while the CLI is
	// quiet. Zero would disable keepalives.
	DefaultHeartbeatInterval = 15 * Second
	DefaultShutdownTimeout   = 30 * Second

	// DefaultMaxConcurrency bounds the CLI processes one adapter may run at
	// once. Every request is a whole subprocess, so an unbounded gateway is a
	// fork bomb waiting for a busy client.
	DefaultMaxConcurrency = 4

	DefaultLogLevel  = "info"
	DefaultLogFormat = "text"
)

// Second is the unit of the [Seconds] defaults above, so they read as durations
// rather than as bare magic numbers.
const Second Seconds = 1

// Default binaries, looked up on PATH unless overridden with an absolute path.
const (
	DefaultClaudeCodeBinary = "claude"
	DefaultCodexBinary      = "codex"
	DefaultCursorBinary     = "cursor-agent"
	DefaultQwenBinary       = "qwen"
)

// DefaultSandbox is the codex sandbox mode: the most restrictive one the CLI
// offers, and the only one agent2api recommends. The prompt should be the only
// thing the backend can act on.
const DefaultSandbox = "read-only"

// DefaultCursorMode answers the question and nothing else. Both cursor modes
// are read-only, but "plan" replies with a plan for the work rather than with
// the answer, which is not what a caller asking for a completion wants.
const DefaultCursorMode = "ask"

// DefaultSystemPromptMode sends only the caller's system message, so a backend
// asked for a completion answers as a plain LLM rather than as the agent its
// CLI ships as, and keeps that CLI's multi-thousand-token preamble out of every
// request.
const DefaultSystemPromptMode = SystemPromptReplace

// Defaults is the lowest configuration layer, with the file and then the flags
// merged over it per key.
//
// It lives in Go so the compiler checks every field name and value, and each
// default sits beside the field it belongs to. agent2api.example.yaml documents
// the same keys; TestExampleConfigCoversTheSchema keeps the two in step.
//
// Every adapter is listed; which ones run is discovered by probing at startup.
func Defaults() Config {
	return Config{
		Server: Server{
			Host:              DefaultHost,
			Port:              DefaultPort,
			RequestTimeout:    DefaultRequestTimeout,
			IdleTimeout:       DefaultIdleTimeout,
			QueueTimeout:      DefaultQueueTimeout,
			HeartbeatInterval: DefaultHeartbeatInterval,
			ShutdownTimeout:   DefaultShutdownTimeout,
			MaxConcurrency:    DefaultMaxConcurrency,
			// APIKey and ScratchDir default to empty: no authentication (which
			// Validate only allows on a loopback bind), and per-request
			// workdirs under the OS temp directory.
		},
		Log: Log{
			Level:  DefaultLogLevel,
			Format: DefaultLogFormat,
			// File empty means stderr.
		},
		Adapters: map[string]Adapter{
			"claude-code": {
				Binary:           DefaultClaudeCodeBinary,
				SystemPromptMode: DefaultSystemPromptMode,
			},
			"codex": {
				Binary:  DefaultCodexBinary,
				Sandbox: DefaultSandbox,
			},
			"cursor": {
				Binary: DefaultCursorBinary,
				Mode:   DefaultCursorMode,
			},
			"qwen-code": {
				Binary: DefaultQwenBinary,
			},
		},
	}
}
