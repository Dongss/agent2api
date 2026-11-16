// Package cursor adapts the Cursor Agent CLI (`cursor-agent`) into an inference
// backend.
//
// The CLI is launched in headless print mode, in read-only ask mode, in an
// empty scratch directory with a sanitized environment: agent2api uses it as a
// plain LLM, never as an agent that touches the machine.
//
// Read-only is not tool-free. In ask mode the model still reads any file on
// the disk, greps and globs outside the scratch directory, starts sub-agents
// with `Task`, and looks up more tools with `getMcpTools`; asked for the first
// lines of /etc/hosts, it read them and put them in the answer. No flag takes
// the tools away, so two layers stand in for one:
//
//   - a permissions file in the scratch directory denies what cursor's
//     permission tokens can name (see deniedPermissions), which narrows what
//     a call can do in the moment before the CLI is stopped;
//   - the parser fails the run at the first tool call and stops the CLI.
//
// The second is the guarantee, as it is for codex and Qwen Code: `Task` and
// `getMcpTools` ran with the deny list in place, and no list anticipates the
// next tool.
package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
const ID = "cursor"

func init() { adapter.Register(ID, New) }

// envAllowPrefixes lets vendor configuration through the environment
// allowlist — cursor-agent reads these to find its account and endpoint.
var envAllowPrefixes = []string{"CURSOR_", "AWS_"}

// requiredFlags must exist in the installed CLI.
var requiredFlags = []string{"--print", "--output-format"}

// DefaultMode is the execution mode used when adapters.cursor.mode is not set.
// It mirrors the config default rather than restating it, so the two cannot
// drift; the adapter keeps its own fallback because Options can be built
// without going through the config layer at all.
const DefaultMode = config.DefaultCursorMode

// deniedPermissions go in the scratch directory's `.cursor/cli.json`, which
// the CLI reads as the workspace's permissions on top of the user's own
// `~/.cursor/cli-config.json`.
//
// Measured on 2026.11.13-eacade1: `Read(**)` is the entry that changes a run.
// Without it the model read /etc/hosts and grepped it; with it the read came
// back "Permission denied" and the grep found nothing. The shell and the
// fetch were refused with or without the file, so `Shell`, `Write` and
// `WebFetch` matter only if that stops being so. `Mcp` is there for the
// servers in the user's `~/.cursor/mcp.json`; no run reached one, and
// `--approve-mcps` is never passed.
var deniedPermissions = []string{"Read(**)", "Write(**)", "Shell(*)", "WebFetch(*)", "Mcp(*:*)"}

// writePermissions puts the deny list where the CLI about to run in dir will
// find it.
func writePermissions(dir string) error {
	var file struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	file.Permissions.Allow = []string{}
	file.Permissions.Deny = deniedPermissions

	body, err := json.Marshal(file)
	if err != nil {
		return err
	}
	conf := filepath.Join(dir, ".cursor")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(conf, "cli.json"), body, 0o600)
}

// MaxPromptBytes caps the rendered transcript. Unlike the other CLIs,
// cursor-agent takes its prompt as a command-line argument, and Linux refuses a
// single argument longer than 128 KiB. Failing with an explanation beats an
// opaque "argument list too long" from the kernel.
const MaxPromptBytes = 96 << 10

// Adapter is the Cursor backend.
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
			Env:      runner.Environ(envAllowPrefixes, opts.Config.EnvPassthrough, opts.Config.Env),
			Required: requiredFlags,
			NotFound: fmt.Sprintf("the Cursor Agent CLI (%s) was not found on PATH; install it from https://cursor.com/cli",
				opts.Config.Binary),
			Missing: func(missing []string) string {
				return fmt.Sprintf("the installed Cursor Agent CLI does not support %s; upgrade it, or point adapters.cursor.binary at a newer install",
					strings.Join(missing, ", "))
			},
			Log: log,
		},
	}
	return a, nil
}

// ID implements adapter.Adapter.
func (a *Adapter) ID() string { return ID }

// Probe checks that the CLI is installed, exposes the flags agent2api needs,
// and is logged in. It sends no prompt, so it costs nothing.
func (a *Adapter) Probe(ctx context.Context) (adapter.Health, error) {
	caps, err := a.probe.Caps(ctx)
	if err != nil {
		return adapter.Health{}, err
	}
	health := adapter.Health{Binary: caps.Path, Version: caps.Version}
	if !caps.Has("--mode") {
		health.Notes = append(health.Notes,
			"installed CLI has no --mode; it will answer in its own default (writable) mode rather than the read-only "+
				a.mode()+" mode")
	}
	health.Notes = append(health.Notes, agentcli.UnsetPassthrough(ID, a.opts.Config.EnvPassthrough)...)
	if !caps.Has("--stream-partial-output") {
		health.Notes = append(health.Notes,
			"installed CLI has no --stream-partial-output; streamed answers arrive in chunks rather than token by token")
	}

	status, err := a.status(ctx)
	switch {
	case err != nil:
		health.Notes = append(health.Notes, "could not read auth status: "+err.Error())
	case !status.IsAuthenticated:
		return health, &ir.Error{
			Code: ir.CodeUpstreamUnavailable,
			Message: "the Cursor Agent CLI is not logged in; run `" + a.opts.Config.Binary + " login`" +
				orDetail(status.Message),
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

	// cursor-agent has no system-prompt flag, so the system message is folded
	// into the transcript rather than dropped.
	rendered := prompt.Render(req.Messages)
	text := rendered.PromptWithSystem()
	if strings.TrimSpace(text) == "" {
		return nil, ir.InvalidRequest("messages", "the conversation has no content to send")
	}
	if len(text) > MaxPromptBytes {
		return nil, ir.InvalidRequest("messages",
			"the conversation renders to %d bytes, more than the %d this backend can take: cursor-agent receives its prompt as a command-line argument",
			len(text), MaxPromptBytes)
	}

	dir, cleanup, err := runner.ScratchDir(runner.ExpandPath(a.opts.ScratchRoot), "agent2api-cursor-")
	if err != nil {
		return nil, ir.Errorf(ir.CodeInternal, "cannot create a scratch directory: %v", err)
	}
	if err := writePermissions(dir); err != nil {
		cleanup()
		return nil, ir.Errorf(ir.CodeInternal, "cannot write the permissions that deny the CLI's tools: %v", err)
	}

	args := a.buildArgs(caps, req, text)
	return agentcli.Stream(ctx, a.log, agentcli.Run{
		Spec: runner.Spec{
			Adapter:        ID,
			Binary:         caps.Path,
			Args:           args,
			Dir:            dir,
			Env:            runner.Environ(envAllowPrefixes, a.opts.Config.EnvPassthrough, a.opts.Config.Env),
			RequestTimeout: a.opts.RequestTimeout,
			IdleTimeout:    a.opts.IdleTimeout,
		},
		Model: req.Model,
		// The prompt is caller content, and extra_args is whatever the operator
		// put there; neither reaches the log.
		LogArgs: agentcli.RedactArgs(args, nil,
			append([]string{text}, agentcli.ExtraArgValues(a.opts.Config.ExtraArgs)...)...),
		Cleanup: cleanup,
	}, func(emit func(ir.Event) bool) agentcli.Parser {
		return newParser(emit)
	}), nil
}

// buildArgs assembles argv, with the prompt as the trailing positional
// argument, which is the only way this CLI accepts one.
func (a *Adapter) buildArgs(caps *agentcli.Caps, req ir.Request, text string) []string {
	args := []string{"--print", "--output-format", "stream-json"}
	if caps.Has("--stream-partial-output") {
		args = append(args, "--stream-partial-output")
	}
	if caps.Has("--mode") {
		// Both modes are read-only; "ask" is the closest this CLI comes to
		// being a plain model.
		args = append(args, "--mode", a.mode())
	}
	if caps.Has("--sandbox") {
		args = append(args, "--sandbox", "enabled")
	}
	if caps.Has("--trust") {
		// The scratch directory is empty and ours; trusting it up front keeps
		// the CLI from stopping to ask about a workspace nobody can see.
		args = append(args, "--trust")
	}
	if req.Variant != "" && caps.Has("--model") {
		args = append(args, "--model", req.Variant)
	}
	args = append(args, a.opts.Config.ExtraArgs...)
	return append(args, text)
}

// mode is the configured execution mode, or the default.
func (a *Adapter) mode() string {
	if m := strings.TrimSpace(a.opts.Config.Mode); m != "" {
		return m
	}
	return DefaultMode
}

// authState is the shape of `cursor-agent status --format json`.
type authState struct {
	Status          string `json:"status"`
	IsAuthenticated bool   `json:"isAuthenticated"`
	Message         string `json:"message"`
	UserInfo        *struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"userInfo"`
}

func (s authState) describe() string {
	parts := make([]string, 0, 2)
	if s.UserInfo != nil {
		if s.UserInfo.Email != "" {
			parts = append(parts, s.UserInfo.Email)
		} else if s.UserInfo.Name != "" {
			parts = append(parts, s.UserInfo.Name)
		}
	}
	if len(parts) == 0 && s.Message != "" {
		parts = append(parts, s.Message)
	}
	if s.Status != "" && s.Status != "authenticated" {
		parts = append(parts, s.Status)
	}
	return strings.Join(parts, " · ")
}

func (a *Adapter) status(ctx context.Context) (authState, error) {
	out, err := a.probe.Capture(ctx, 60*time.Second, "status", "--format", "json")
	if err != nil {
		return authState{}, err
	}
	var state authState
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &state); err != nil {
		return authState{}, fmt.Errorf("unexpected `status --format json` output: %w", err)
	}
	return state, nil
}

func orDetail(msg string) string {
	if msg = strings.TrimSpace(msg); msg == "" {
		return ""
	}
	return " (" + msg + ")"
}
