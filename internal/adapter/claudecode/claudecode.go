// Package claudecode adapts the Claude Code CLI (`claude`) into an inference
// backend.
//
// The CLI is launched in headless print mode with every tool disabled, in an
// empty scratch directory, with a sanitized environment: agent2api uses it as a
// plain LLM, never as an agent that touches the machine.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/agentcli"
	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/prompt"
	"github.com/Dongss/agent2api/internal/runner"
)

// ID is the adapter id used to namespace this backend's models.
const ID = "claude-code"

func init() { adapter.Register(ID, New) }

// envAllowPrefixes lets vendor configuration through the environment
// allowlist — Claude Code reads these to find its account and endpoint.
var envAllowPrefixes = []string{
	"ANTHROPIC_", "CLAUDE_", "AWS_", "GOOGLE_", "VERTEX_", "CLOUD_ML_",
}

// requiredFlags must exist in the installed CLI. `--tools` is on the list
// because it is how tools get disabled: without it, agent2api cannot promise
// the backend behaves as a bare LLM, so it refuses to run at all.
var requiredFlags = []string{"--print", "--output-format", "--verbose", "--tools"}

// hardeningFlags strip everything that makes the CLI more than a model: local
// settings, project skills, MCP servers, and session state on disk. Each is
// applied only if the installed CLI accepts it, so a version bump that renames
// one degrades instead of breaking.
var hardeningFlags = []string{
	"--no-session-persistence",
	"--disable-slash-commands",
	"--strict-mcp-config",
}

// Adapter is the Claude Code backend.
type Adapter struct {
	opts  adapter.Options
	log   *slog.Logger
	probe *agentcli.Probe
}

// New builds the adapter from its resolved configuration.
func New(opts adapter.Options) (adapter.Adapter, error) {
	if opts.Config.Binary == "" {
		return nil, fmt.Errorf("%s: binary must not be empty", ID)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("adapter", ID)

	a := &Adapter{
		opts: opts,
		log:  log,
		probe: &agentcli.Probe{
			Binary:   opts.Config.Binary,
			Env:      runner.Environ(envAllowPrefixes, opts.Config.Env),
			Required: requiredFlags,
			NotFound: fmt.Sprintf("the Claude Code CLI (%s) was not found on PATH; install it from https://claude.com/claude-code",
				opts.Config.Binary),
			Missing: func(missing []string) string {
				return fmt.Sprintf("the installed Claude Code CLI does not support %s; upgrade it, or point adapters.claude-code.binary at a newer install",
					strings.Join(missing, ", "))
			},
			Log: log,
		},
	}
	return a, nil
}

// ID implements adapter.Adapter.
func (a *Adapter) ID() string { return ID }

// EnforcesSchema implements adapter.SchemaEnforcer. The CLI holds the answer to
// a caller's JSON Schema with `--json-schema`; an install too old for the flag
// says so, and the frontend refuses the request rather than returning JSON
// nobody checked.
func (a *Adapter) EnforcesSchema(ctx context.Context) bool {
	caps, err := a.probe.Caps(ctx)
	return err == nil && caps.Has("--json-schema")
}

// Probe checks that the CLI is installed, exposes the flags agent2api needs,
// and is logged in. It deliberately does not send a prompt: `claude auth
// status` answers the same question without spending tokens.
func (a *Adapter) Probe(ctx context.Context) (adapter.Health, error) {
	caps, err := a.probe.Caps(ctx)
	if err != nil {
		return adapter.Health{}, err
	}
	health := adapter.Health{Binary: caps.Path, Version: caps.Version}
	for _, flag := range hardeningFlags {
		if !caps.Has(flag) {
			health.Notes = append(health.Notes, "installed CLI has no "+flag+"; running without it")
		}
	}
	if !caps.Has("--append-system-prompt") {
		health.Notes = append(health.Notes, "installed CLI has no --append-system-prompt; system prompts are folded into the transcript")
	}

	status, err := a.authStatus(ctx)
	switch {
	case err != nil:
		health.Notes = append(health.Notes, "could not read auth status: "+err.Error())
	case !status.LoggedIn:
		return health, &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Claude Code CLI is not logged in; run `" + a.opts.Config.Binary + " auth login`",
		}
	default:
		health.Account = status.describe()
	}
	return health, nil
}

// Run executes one request as a fresh CLI process.
func (a *Adapter) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	caps, err := a.probe.Caps(ctx)
	if err != nil {
		return nil, err
	}

	rendered := prompt.Render(req.Messages)
	args, stdin := a.buildArgs(caps, req, rendered)
	if strings.TrimSpace(stdin) == "" {
		return nil, ir.InvalidRequest("messages", "the conversation has no content to send")
	}

	dir, cleanup, err := runner.ScratchDir(runner.ExpandPath(a.opts.ScratchRoot), "agent2api-claude-")
	if err != nil {
		return nil, ir.Errorf(ir.CodeInternal, "cannot create a scratch directory: %v", err)
	}

	return agentcli.Stream(ctx, a.log, agentcli.Run{
		Spec: runner.Spec{
			Adapter:        ID,
			Binary:         caps.Path,
			Args:           args,
			Dir:            dir,
			Env:            runner.Environ(envAllowPrefixes, a.opts.Config.Env),
			Stdin:          stdin,
			RequestTimeout: a.opts.RequestTimeout,
			IdleTimeout:    a.opts.IdleTimeout,
		},
		Model:   req.Model,
		LogArgs: a.redactArgs(args),
		Cleanup: cleanup,
	}, func(emit func(ir.Event) bool) agentcli.Parser {
		return newParser(emit)
	}), nil
}

// buildArgs assembles argv and the prompt that goes to stdin. The prompt is
// never an argument: a replayed conversation can exceed the OS argv limit.
func (a *Adapter) buildArgs(caps *agentcli.Caps, req ir.Request, rendered prompt.Rendered) (args []string, stdin string) {
	args = []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		// The only tool list agent2api ever passes: none.
		"--tools", "",
	}
	if caps.Has("--include-partial-messages") {
		args = append(args, "--include-partial-messages")
	}
	for _, flag := range hardeningFlags {
		if caps.Has(flag) {
			args = append(args, flag)
		}
	}
	if caps.Has("--setting-sources") {
		// Load no user/project/local settings files.
		args = append(args, "--setting-sources", "")
	}
	if req.Variant != "" && caps.Has("--model") {
		args = append(args, "--model", req.Variant)
	}
	// Only ever set once EnforcesSchema said yes, so a missing flag here would
	// mean the caller was promised something the CLI cannot do.
	if req.Schema != "" && caps.Has("--json-schema") {
		args = append(args, "--json-schema", req.Schema)
	}

	stdin = rendered.Prompt
	args = append(args, a.systemPromptArgs(caps, rendered, &stdin)...)

	args = append(args, a.opts.Config.ExtraArgs...)
	return args, stdin
}

// redactArgs makes argv safe to log. The system prompt and the JSON schema are
// caller content, and extra_args is whatever the operator put there, so none of
// them reaches the log at any level.
func (a *Adapter) redactArgs(args []string) []string {
	return agentcli.RedactArgs(args,
		[]string{"--system-prompt", "--append-system-prompt", "--json-schema"},
		agentcli.ExtraArgValues(a.opts.Config.ExtraArgs)...)
}

// systemPromptArgs decides how the caller's system message reaches the CLI.
//
// "replace" (the default) swaps out the CLI's own agent prompt, which is what
// makes the backend answer as a plain LLM and cuts thousands of tokens off every
// request. "append" keeps the CLI's native behaviour and adds the caller's on
// top. With neither flag available the system text folds into the transcript, so
// nothing is silently dropped.
func (a *Adapter) systemPromptArgs(caps *agentcli.Caps, rendered prompt.Rendered, stdin *string) []string {
	replace := a.opts.Config.SystemPromptMode == config.SystemPromptReplace
	if replace && caps.Has("--system-prompt") {
		// An empty value is meaningful here: no system prompt at all.
		return []string{"--system-prompt", rendered.System}
	}
	if rendered.System == "" {
		return nil
	}
	if caps.Has("--append-system-prompt") {
		return []string{"--append-system-prompt", rendered.System}
	}
	*stdin = rendered.PromptWithSystem()
	return nil
}

// authState is the shape of `claude auth status --json`.
type authState struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	Email            string `json:"email"`
	OrgName          string `json:"orgName"`
	SubscriptionType string `json:"subscriptionType"`
}

func (s authState) describe() string {
	parts := make([]string, 0, 3)
	if s.Email != "" {
		parts = append(parts, s.Email)
	}
	if s.SubscriptionType != "" {
		parts = append(parts, s.SubscriptionType)
	} else if s.AuthMethod != "" {
		parts = append(parts, s.AuthMethod)
	}
	if s.OrgName != "" {
		parts = append(parts, s.OrgName)
	}
	return strings.Join(parts, " · ")
}

func (a *Adapter) authStatus(ctx context.Context) (authState, error) {
	out, err := a.probe.Capture(ctx, 60*time.Second, "auth", "status", "--json")
	if err != nil {
		return authState{}, err
	}
	var state authState
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &state); err != nil {
		return authState{}, fmt.Errorf("unexpected `auth status` output: %w", err)
	}
	return state, nil
}
