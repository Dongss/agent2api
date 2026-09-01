// Package qwen adapts the Qwen Code CLI (`qwen`) into an inference backend.
//
// The CLI is launched non-interactively in an empty scratch directory with a
// sanitized environment and a generated settings file that leaves it no tools:
// agent2api uses it as a plain LLM, never as an agent that touches the machine.
//
// # How the tools get turned off, and why it takes this much
//
// The other three adapters disable tools with a flag. Qwen Code has none, and
// the settings that do the job come with two traps its own documentation names.
//
// An allowlist looks like the answer — `tools.core` is fail-closed over the
// core tool set — but an *empty* one "is treated as unset and disables
// nothing", so `tools.core: []` would leave every tool live. A non-empty list
// of a name that matches nothing is what actually closes it.
//
// That alone is not enough. The tools it removes stay reachable through
// `tool_search`, which is not a core tool and so survives the allowlist: asked
// to list a directory under `tools.core` alone, the CLI found `glob` and
// `run_shell_command` again and read the directory. The non-core tools have to
// be denied by name as well.
//
// A denylist has the failure mode the project cannot accept — the docs warn it
// "must be re-audited per release", because a built-in added later registers
// until explicitly denied. So the deny list is not the guarantee. The guarantee
// is [parser], which reads the tool list the CLI reports on startup and fails
// the run if it is not empty. A future release that adds a tool breaks loudly
// instead of quietly handing the model a shell.
//
// `--safe-mode` is deliberately not passed. It disables customisations, and the
// settings file below is one: with it the CLI came back with all 23 tools live.
package qwencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/agentcli"
	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/prompt"
	"github.com/Dongss/agent2api/internal/runner"
)

// ID is the adapter id used to namespace this backend's models.
const ID = "qwen-code"

func init() { adapter.Register(ID, New) }

// envAllowPrefixes lets vendor configuration through the environment
// allowlist — Qwen Code reads these to find its account and endpoint.
var envAllowPrefixes = []string{"QWEN_", "DASHSCOPE_", "OPENAI_", "ALIBABA_"}

// requiredFlags must exist in the installed CLI.
//
// Unlike the other adapters there is no tool flag to require, because there is
// no tool flag; see the package comment for what stands in for one.
var requiredFlags = []string{"--output-format", "--prompt"}

// noSuchTool is the single entry in the core-tool allowlist. Its value only has
// to match no real tool: a non-empty list is what makes the allowlist
// fail-closed, and an empty one would disable nothing.
const noSuchTool = "__agent2api_no_tools__"

// deniedTools are the tools the core-tool allowlist does not cover, denied by
// name and by category. `tool_search` is the one that matters most: without it
// the model reaches the tools the allowlist removed.
//
// This list is belt-and-braces, not the guarantee — see the package comment.
var deniedTools = []string{
	// Categories, which cover the built-ins and their shell equivalents.
	"Bash", "Read", "Edit", "WebFetch",
	// Non-core tools observed on qwen 0.22.3.
	"tool_search", "agent", "list_agents", "skill", "send_message",
	"get_goal", "update_goal", "task_stop", "report_findings",
	"enter_worktree", "exit_worktree", "record_artifact", "read_mcp_resource",
}

// settings is the per-request `.qwen/settings.json` written into the scratch
// directory, where it takes precedence over the user's own.
type settings struct {
	Tools struct {
		Core []string `json:"core"`
	} `json:"tools"`
	Permissions struct {
		Deny []string `json:"deny"`
	} `json:"permissions"`
}

// writeSettings puts the tool-disabling settings in the scratch directory the
// CLI is about to run in.
func writeSettings(dir string) error {
	var s settings
	s.Tools.Core = []string{noSuchTool}
	s.Permissions.Deny = deniedTools

	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	conf := filepath.Join(dir, ".qwen")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(conf, "settings.json"), body, 0o600)
}

// Adapter is the Qwen Code backend.
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
			NotFound: fmt.Sprintf("the Qwen Code CLI (%s) was not found on PATH; install it from https://github.com/QwenLM/qwen-code",
				opts.Config.Binary),
			Missing: func(missing []string) string {
				return fmt.Sprintf("the installed Qwen Code CLI does not support %s; upgrade it, or point adapters.qwen.binary at a newer install",
					strings.Join(missing, ", "))
			},
			Log: log,
		},
	}
	return a, nil
}

// ID implements adapter.Adapter.
func (a *Adapter) ID() string { return ID }

// Probe checks that the CLI is installed and exposes the flags agent2api needs.
//
// It reports no account, and says nothing about why. `qwen auth` describes
// itself as removed and the CLI offers no equivalent of `claude auth status
// --json`, so the only way to learn whether it is logged in is to send a prompt
// — which costs tokens, and probing is meant to be free. An unauthenticated
// install fails its first real request with the CLI's own explanation instead.
//
// That could be a note, and was one. But Health.Notes is for remarks about a
// particular install — a flag this version lacks, a sandbox loosened in config
// — and a note that fires on every healthy run is not a remark, it is a
// permanent footnote under a table that is otherwise all `ok`. The empty
// account column carries the same information for anyone who looks.
func (a *Adapter) Probe(ctx context.Context) (adapter.Health, error) {
	caps, err := a.probe.Caps(ctx)
	if err != nil {
		return adapter.Health{}, err
	}
	return adapter.Health{Binary: caps.Path, Version: caps.Version}, nil
}

// Run executes one request as a fresh CLI process.
func (a *Adapter) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	caps, err := a.probe.Caps(ctx)
	if err != nil {
		return nil, err
	}

	// Qwen Code has no system-prompt flag, so the system message is folded into
	// the transcript rather than dropped.
	rendered := prompt.Render(req.Messages)
	stdin := rendered.PromptWithSystem()
	if strings.TrimSpace(stdin) == "" {
		return nil, ir.InvalidRequest("messages", "the conversation has no content to send")
	}

	dir, cleanup, err := runner.ScratchDir(runner.ExpandPath(a.opts.ScratchRoot), "agent2api-qwencode-")
	if err != nil {
		return nil, ir.Errorf(ir.CodeInternal, "cannot create a scratch directory: %v", err)
	}
	if err := writeSettings(dir); err != nil {
		cleanup()
		return nil, ir.Errorf(ir.CodeInternal, "cannot write the settings that disable the CLI's tools: %v", err)
	}

	args := a.buildArgs(caps, req)
	return agentcli.Stream(ctx, a.log, agentcli.Run{
		Spec: runner.Spec{
			Adapter: ID,
			Binary:  caps.Path,
			Args:    args,
			// The CLI takes no working-directory flag; it uses the process's,
			// which is also where it looks for the settings written above.
			Dir:            dir,
			Env:            runner.Environ(envAllowPrefixes, a.opts.Config.Env),
			Stdin:          stdin,
			RequestTimeout: a.opts.RequestTimeout,
			IdleTimeout:    a.opts.IdleTimeout,
		},
		Model: req.Model,
		// The prompt goes on stdin, so argv carries no caller content — but
		// extra_args is whatever the operator put there.
		LogArgs: agentcli.RedactArgs(args, nil, agentcli.ExtraArgValues(a.opts.Config.ExtraArgs)...),
		Cleanup: cleanup,
	}, func(emit func(ir.Event) bool) agentcli.Parser {
		return newParser(emit)
	}), nil
}

// buildArgs assembles argv. The prompt is never an argument: `--prompt` appends
// to stdin, and a replayed conversation can exceed the OS argv limit, so it
// goes on stdin alone.
func (a *Adapter) buildArgs(caps *agentcli.Caps, req ir.Request) []string {
	args := []string{"--output-format", "stream-json"}
	if req.Variant != "" && caps.Has("--model") {
		args = append(args, "--model", req.Variant)
	}
	return append(args, a.opts.Config.ExtraArgs...)
}
