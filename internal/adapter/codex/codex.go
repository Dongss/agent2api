// Package codex adapts the Codex CLI (`codex exec`) into an inference backend.
//
// The CLI is launched non-interactively with the read-only sandbox, in an empty
// scratch directory, with no session persisted and a sanitized environment:
// agent2api uses it as a plain LLM, never as an agent that touches the machine.
//
// None of that takes the tools away. The read-only sandbox still lets a shell
// command read the whole disk, and an MCP server the user added to config.toml
// is started outside the sandbox altogether — so a caller could ask for a file
// and get it back in the answer, or have a server act on their behalf. Two
// layers keep that from happening:
//
//   - argv switches off every tool codex lets a flag or config key remove (see
//     toolFeatures and mcpOverrides), so the model is not offered them;
//   - the parser refuses any thread item that is not a message, a reasoning
//     summary or a warning, and stops the CLI there.
//
// The second is the guarantee, as `parser.system` is for Qwen Code: what codex
// offers depends on the release, the model and the user's config, and a list
// of switches cannot anticipate the next tool. A tool call that slips past the
// first layer fails the request rather than answering it.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
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
const ID = "codex"

func init() { adapter.Register(ID, New) }

// envAllowPrefixes lets vendor configuration through the environment
// allowlist — Codex reads these to find its account and endpoint.
var envAllowPrefixes = []string{"OPENAI_", "CODEX_", "AZURE_"}

// requiredFlags must exist in the installed CLI. The sandbox and the scratch
// workdir are how the CLI is kept away from the machine, so without them
// agent2api refuses to run rather than promise a containment it cannot deliver.
var requiredFlags = []string{"--json", "--sandbox", "--cd", "--skip-git-repo-check"}

// DefaultSandbox is the sandbox mode used when adapters.codex.sandbox is not
// set. It mirrors the config default rather than restating it, so the two
// cannot drift; the adapter keeps its own fallback because Options can be
// built without going through the config layer at all.
const DefaultSandbox = config.DefaultSandbox

// hardeningFlags are applied when the installed CLI accepts them: no session
// files on disk, and no project rule files loaded from the scratch directory.
var hardeningFlags = []string{"--ephemeral", "--ignore-rules"}

// toolFeatures are the `[features]` that put a tool in front of the model,
// each switched off with `-c features.<name>=false`:
//
//   - shell_tool and unified_exec: exec_command and write_stdin, the shell;
//   - multi_agent: spawn_agent and the rest of multi_agent_v1;
//   - goals: get_goal, create_goal and update_goal;
//   - view_image, which reads an image file off the disk;
//   - plugins, apps and computer_use: plugin MCP servers, cua_repl among them;
//   - sleep_tool: clock.sleep.
//
// The list comes from 0.163.0's own requests, captured against a stub
// provider. With all of them off, a third-party model is offered
// `request_user_input` alone. A GPT-6 model keeps its code-mode `exec`, a V8
// isolate with no file system or network, and the sub-agent tools, which a
// sub-agent inherits these settings for. Inside `exec` it can still reach the
// clock, MCP resource readers with no server left to read, and apply_patch,
// which no switch tried removes: the read-only sandbox refuses the write, and
// the parser refuses the file_change item it would produce.
var toolFeatures = []string{
	"shell_tool", "unified_exec", "multi_agent", "goals", "view_image",
	"plugins", "apps", "computer_use", "sleep_tool",
}

// featurePrefix marks a feature name in the probed flag set. Features are
// config keys rather than flags, and an unknown one is fatal — `--disable
// no_such_feature` fails the run — so only the names the install lists are
// ever passed.
const featurePrefix = "features."

// knownFeatures reads `codex features list` into the probed flag set, one
// "features.<name>" entry per feature the install knows. An install without
// the subcommand reports none, and argv then switches nothing off.
func knownFeatures(_ context.Context, run func(cliArgs ...string) (string, error)) []string {
	out, err := run("features", "list")
	if err != nil {
		return nil
	}
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			names = append(names, featurePrefix+fields[0])
		}
	}
	return names
}

// featureOverrides are the `-c` pairs that switch off every tool feature this
// install knows.
func featureOverrides(caps *agentcli.Caps) []string {
	var args []string
	for _, f := range toolFeatures {
		if caps.Has(featurePrefix + f) {
			args = append(args, "-c", featurePrefix+f+"=false")
		}
	}
	return args
}

// bareKey is what a `-c` dotted path can address. A server named outside it
// (with a dot, say) cannot be switched off from argv.
var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// mcpOverrides switches off every MCP server the user's config.toml enables.
// A server the user added is started outside the sandbox: on 0.163.0 a stub
// server wrote a file beyond the scratch directory as it started, where
// node_repl, which ships with codex, got EPERM for the same write from the
// code it ran. A remote server is not on this machine at all. The parser's
// refusal arrives when the call starts, which is too late to stop its side
// effects.
//
// `-c mcp_servers={}` does not clear the table (overrides merge), so each
// server is named. The names are read on every request rather than at probe
// time, because config.toml can change under a running gateway. Plugin servers
// are listed with the same features off, which leaves them out: they are
// already gone with `plugins`, and naming one that is not in config.toml is a
// config error that fails the run.
//
// A server that cannot be switched off refuses the request: agent2api does not
// run the CLI with a containment it cannot deliver.
func (a *Adapter) mcpOverrides(ctx context.Context, caps *agentcli.Caps) ([]string, error) {
	listArgs := append([]string{"mcp", "list", "--json"}, featureOverrides(caps)...)
	out, err := a.probe.Capture(ctx, 30*time.Second, listArgs...)
	if err != nil {
		e := ir.AsError(err)
		return nil, &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "cannot list the Codex CLI's MCP servers to switch them off; `" + a.opts.Config.Binary + " mcp list --json` must succeed",
			Detail:  strings.TrimSpace(e.Message + "\n" + e.Detail),
		}
	}
	var servers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(out), &servers); err != nil {
		return nil, &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "cannot read the Codex CLI's MCP server list to switch the servers off",
			Detail:  err.Error(),
		}
	}
	var args []string
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if !bareKey.MatchString(s.Name) {
			return nil, &ir.Error{
				Code: ir.CodeUpstreamUnavailable,
				Message: fmt.Sprintf("the Codex CLI's config enables MCP server %q, whose name a -c override cannot address; "+
					"set enabled = false on it in config.toml, or rename it", s.Name),
			}
		}
		args = append(args, "-c", "mcp_servers."+s.Name+".enabled=false")
	}
	return args, nil
}

// Adapter is the Codex backend.
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
			Binary: opts.Config.Binary,
			Env:    runner.Environ(envAllowPrefixes, opts.Config.EnvPassthrough, opts.Config.Env),
			// The flags that matter live under `codex exec`, not at top level.
			HelpArgs:     []string{"exec", "--help"},
			Required:     requiredFlags,
			Undocumented: knownFeatures,
			NotFound: fmt.Sprintf("the Codex CLI (%s) was not found on PATH; install it from https://developers.openai.com/codex/cli",
				opts.Config.Binary),
			Missing: func(missing []string) string {
				return fmt.Sprintf("the installed Codex CLI does not support %s; upgrade it, or point adapters.codex.binary at a newer install",
					strings.Join(missing, ", "))
			},
			Log: log,
		},
	}
	return a, nil
}

// ID implements adapter.Adapter.
func (a *Adapter) ID() string { return ID }

// effortLevels are the values passed through to `model_reasoning_effort`.
// `max` is absent: it belongs to Claude Code's scale, not this one.
var effortLevels = []ir.Effort{ir.EffortMinimal, ir.EffortLow, ir.EffortMedium, ir.EffortHigh, ir.EffortXHigh}

// EffortLevels implements adapter.EffortSetter.
//
// The knob is a config override rather than a flag, and `-c` is always
// available, so there is nothing to probe for.
//
// Worth knowing before trusting a level end to end: codex turns it into a
// `thinking_budget` for the upstream endpoint, and the endpoint decides whether
// that number is acceptable. Against the third-party provider on the recording
// machine, `low` worked and `high` came back `400 InternalError.Algo.
// InvalidParameter: The thinking_budget parameter must be a positive integer
// and not greater than …`. That surfaces as an honest upstream error rather
// than a silent downgrade, which is the behaviour to keep — but it means a
// level this adapter accepts can still be refused further up.
func (a *Adapter) EffortLevels(context.Context) []ir.Effort { return effortLevels }

// Probe checks that the CLI is installed, exposes the flags agent2api needs,
// and is logged in. It sends no prompt, so it costs nothing.
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
	if len(featureOverrides(caps)) == 0 {
		health.Notes = append(health.Notes, "installed CLI lists none of the features that switch its tools off; "+
			"a request that makes it use one will fail")
	}
	health.Notes = append(health.Notes, a.sandboxNote(caps)...)
	health.Notes = append(health.Notes, agentcli.UnsetPassthrough(ID, a.opts.Config.EnvPassthrough)...)

	account, loggedIn, err := a.loginStatus(ctx)
	switch {
	case err != nil:
		health.Notes = append(health.Notes, "could not read login status: "+err.Error())
	case !loggedIn:
		return health, &ir.Error{
			Code:    ir.CodeUpstreamUnavailable,
			Message: "the Codex CLI is not logged in; run `" + a.opts.Config.Binary + " login`",
		}
	default:
		health.Account = account
	}
	return health, nil
}

// sandboxNote warns when the configured sandbox is not the most restrictive
// one. Loosening it is the user's call, but it must not be a silent one.
func (a *Adapter) sandboxNote(*agentcli.Caps) []string {
	if s := a.sandbox(); s != DefaultSandbox {
		return []string{fmt.Sprintf("sandbox is %q, not %q: the CLI may reach beyond its scratch directory", s, DefaultSandbox)}
	}
	return nil
}

func (a *Adapter) sandbox() string {
	if s := strings.TrimSpace(a.opts.Config.Sandbox); s != "" {
		return s
	}
	return DefaultSandbox
}

// Run executes one request as a fresh CLI process.
func (a *Adapter) Run(ctx context.Context, req ir.Request) (<-chan ir.Event, error) {
	caps, err := a.probe.Caps(ctx)
	if err != nil {
		return nil, err
	}

	// Codex has no system-prompt flag, so the system message is folded into the
	// transcript rather than dropped.
	rendered := prompt.Render(req.Messages)
	stdin := rendered.PromptWithSystem()
	if strings.TrimSpace(stdin) == "" {
		return nil, ir.InvalidRequest("messages", "the conversation has no content to send")
	}

	mcp, err := a.mcpOverrides(ctx, caps)
	if err != nil {
		return nil, err
	}

	dir, cleanup, err := runner.ScratchDir(runner.ExpandPath(a.opts.ScratchRoot), "agent2api-codex-")
	if err != nil {
		return nil, ir.Errorf(ir.CodeInternal, "cannot create a scratch directory: %v", err)
	}

	args := a.buildArgs(caps, req, dir, mcp)
	return agentcli.Stream(ctx, a.log, agentcli.Run{
		Spec: runner.Spec{
			Adapter:        ID,
			Binary:         caps.Path,
			Args:           args,
			Dir:            dir,
			Env:            runner.Environ(envAllowPrefixes, a.opts.Config.EnvPassthrough, a.opts.Config.Env),
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

// buildArgs assembles argv. The trailing "-" tells Codex to read the prompt
// from stdin, which a replayed conversation needs: it can exceed the OS limit
// on the length of a single argument. mcp is what mcpOverrides returned.
func (a *Adapter) buildArgs(caps *agentcli.Caps, req ir.Request, dir string, mcp []string) []string {
	args := []string{
		"exec",
		"--json",
		"--sandbox", a.sandbox(),
		"--skip-git-repo-check",
		"--cd", dir,
	}
	for _, flag := range hardeningFlags {
		if caps.Has(flag) {
			args = append(args, flag)
		}
	}
	if caps.Has("--color") {
		args = append(args, "--color", "never")
	}
	args = append(args, featureOverrides(caps)...)
	// A config key rather than a feature, so there is nothing to probe. An
	// install without the key ignores it, as codex does any unknown `-c` key,
	// and the parser catches the search; one that drops the "disabled" value
	// fails every request with a config error, which is loud enough.
	args = append(args, "-c", `web_search="disabled"`)
	args = append(args, mcp...)
	if req.Variant != "" && caps.Has("--model") {
		args = append(args, "--model", req.Variant)
	}
	if req.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+string(req.Effort))
	}
	args = append(args, a.opts.Config.ExtraArgs...)
	return append(args, "-")
}

// loginStatus reads `codex login status`, which prints a line like
// "Logged in using ChatGPT" and costs nothing.
func (a *Adapter) loginStatus(ctx context.Context) (account string, loggedIn bool, err error) {
	out, err := a.probe.Capture(ctx, 60*time.Second, "login", "status")
	text := agentcli.FirstLine(out)
	if err != nil {
		// The CLI exits non-zero when nobody is logged in, so an error whose
		// output says so is an answer, not a failure.
		if unauthenticated(text + " " + ir.AsError(err).Detail) {
			return "", false, nil
		}
		return "", false, err
	}
	if text == "" || unauthenticated(text) {
		return "", false, nil
	}
	return strings.TrimPrefix(text, "Logged in using "), true, nil
}

func unauthenticated(text string) bool {
	return containsAny(strings.ToLower(text), "not logged in", "please run `codex login`", "please log in")
}
