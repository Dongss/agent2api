// Package agentcli holds the plumbing every CLI-backed adapter needs: resolving
// the binary, learning which flags the installed version actually supports, and
// turning one subprocess run into a stream of IR events.
//
// What stays in each adapter is only what is specific to its CLI: the argv to
// build, and how to read the CLI's output. Subprocess lifecycle itself belongs
// to internal/runner.
package agentcli

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/runner"
)

// probeTimeout bounds the `--help` and `--version` calls used to inspect an
// install. They are local and instant; anything slower is a broken CLI.
const probeTimeout = 30 * time.Second

// Caps records what the installed CLI supports, so argv can adapt instead of
// breaking when flags drift between releases.
type Caps struct {
	// Path is the resolved absolute path of the binary.
	Path string
	// Version is the first line of the CLI's --version output.
	Version string

	flags map[string]bool
}

// NewCaps records what one install supports. [Probe.Caps] builds the real thing
// from a --help dump; passing the flag set directly also lets a caller state the
// capabilities it wants to reason about without spawning anything.
func NewCaps(path, version string, flags map[string]bool) *Caps {
	if flags == nil {
		flags = map[string]bool{}
	}
	return &Caps{Path: path, Version: version, flags: flags}
}

// Has reports whether the installed CLI documents the given flag.
func (c *Caps) Has(flag string) bool { return c.flags[flag] }

// Probe resolves one CLI and inspects it once, caching the result. A failure is
// never cached, so a CLI installed after startup is picked up on the next
// request.
type Probe struct {
	// Binary is the configured command name or absolute path.
	Binary string
	// Env is the complete, allowlisted environment for probe calls.
	Env []string
	// HelpArgs and VersionArgs default to --help and --version. A CLI whose
	// flags live under a subcommand passes that subcommand here.
	HelpArgs    []string
	VersionArgs []string
	// Required lists flags the adapter cannot work without.
	Required []string
	// Undocumented finds flags the CLI accepts but leaves out of --help, which
	// a --help scan cannot see. It runs once per successful probe, with a way
	// to call the binary; the names it returns join the flag set, so
	// [Caps.Has] answers for them like any other. A failed call should read as
	// "absent", never fail the probe: the flag is optional by construction.
	Undocumented func(ctx context.Context, run func(cliArgs ...string) (string, error)) []string
	// NotFound is the message for a missing binary; it should say where to get
	// the CLI. Missing renders the message for an install that lacks required
	// flags.
	NotFound string
	Missing  func(missing []string) string
	Log      *slog.Logger

	mu    sync.Mutex
	cache *Caps
}

// Caps resolves the binary and its supported flags.
func (p *Probe) Caps(ctx context.Context) (*Caps, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache != nil {
		return p.cache, nil
	}

	path, err := exec.LookPath(runner.ExpandPath(p.Binary))
	if err != nil {
		return nil, &ir.Error{Code: ir.CodeUpstreamUnavailable, Message: p.NotFound, Err: err}
	}

	help, err := p.capture(ctx, path, args(p.HelpArgs, "--help"))
	if err != nil {
		return nil, err
	}
	flags := ParseFlagNames(help)
	var missing []string
	for _, flag := range p.Required {
		if !flags[flag] {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		return nil, &ir.Error{Code: ir.CodeUpstreamUnavailable, Message: p.Missing(missing)}
	}
	if p.Undocumented != nil {
		for _, flag := range p.Undocumented(ctx, func(cliArgs ...string) (string, error) {
			return p.capture(ctx, path, cliArgs)
		}) {
			flags[flag] = true
		}
	}

	version, err := p.capture(ctx, path, args(p.VersionArgs, "--version"))
	if err != nil {
		return nil, err
	}

	p.cache = NewCaps(path, FirstLine(version), flags)
	if p.Log != nil {
		p.Log.Debug("probed agent cli", "path", path, "version", p.cache.Version)
	}
	return p.cache, nil
}

// Capture runs the CLI with the given arguments and returns its stdout. It is
// how adapters ask a CLI about itself, e.g. for login state.
func (p *Probe) Capture(ctx context.Context, timeout time.Duration, cliArgs ...string) (string, error) {
	binary := p.Binary
	p.mu.Lock()
	if p.cache != nil {
		binary = p.cache.Path
	}
	p.mu.Unlock()
	return p.run(ctx, binary, timeout, cliArgs)
}

func (p *Probe) capture(ctx context.Context, path string, cliArgs []string) (string, error) {
	return p.run(ctx, path, probeTimeout, cliArgs)
}

// run executes a self-describing call and returns what it printed.
//
// Some CLIs answer these on stderr rather than stdout (`codex login status`
// does), so stderr is the fallback, with a tail large enough to hold a --help
// dump for a CLI that prints one there.
func (p *Probe) run(ctx context.Context, binary string, timeout time.Duration, cliArgs []string) (string, error) {
	var out strings.Builder
	res, err := runner.Run(ctx, runner.Spec{
		Binary:          binary,
		Args:            cliArgs,
		Env:             p.Env,
		RequestTimeout:  timeout,
		StderrTailBytes: 256 << 10,
	}, func(line []byte) error {
		out.Write(line)
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		e := ir.AsError(err)
		if e.Code == ir.CodeUpstreamError {
			// A non-zero exit from a self-describing call means the install is
			// not usable, not that the request was bad.
			e = &ir.Error{
				Code:    ir.CodeUpstreamUnavailable,
				Message: fmt.Sprintf("`%s %s` failed (exit %d)", binary, strings.Join(cliArgs, " "), res.ExitCode),
				Detail:  strings.TrimSpace(res.Stderr + "\n" + out.String()),
			}
		}
		return out.String(), e
	}
	if strings.TrimSpace(out.String()) == "" {
		return res.Stderr, nil
	}
	return out.String(), nil
}

func args(given []string, fallback ...string) []string {
	if len(given) > 0 {
		return given
	}
	return fallback
}

// ParseFlagNames extracts every "--flag" token from a --help dump, which is how
// agent2api tells what an installed version supports.
func ParseFlagNames(help string) map[string]bool {
	flags := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(help))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		for _, field := range strings.FieldsFunc(scanner.Text(), func(r rune) bool {
			return r == ' ' || r == ',' || r == '\t' || r == '<' || r == '>' || r == '[' || r == ']' || r == '='
		}) {
			if strings.HasPrefix(field, "--") && len(field) > 2 {
				flags[field] = true
			}
		}
	}
	return flags
}

// ExtraArgValues picks out the tokens in a user-supplied argv fragment that
// could be values rather than flag names, for [RedactArgs] to keep out of the
// log: extra_args may carry a credential, a flag name never does.
//
// A token appearing elsewhere in argv is redacted there too. Losing a line of
// log detail is the right side to err on.
func ExtraArgValues(extra []string) []string {
	var out []string
	for _, arg := range extra {
		if !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
			out = append(out, arg)
		}
	}
	return out
}

// FirstLine is the first non-empty line of s, trimmed.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// RedactArgs makes argv safe to log by replacing the value that follows any of
// secretFlags, and any argument listed in literals. Caller content never
// reaches the log, at any level.
//
// A hidden "--flag=value" token keeps its flag name: knowing which setting was
// passed is the useful half, and it is not the secret half.
func RedactArgs(cliArgs []string, secretFlags []string, literals ...string) []string {
	secret := map[string]bool{}
	for _, f := range secretFlags {
		secret[f] = true
	}
	hide := map[string]bool{}
	for _, l := range literals {
		if l != "" {
			hide[l] = true
		}
	}
	out := make([]string, len(cliArgs))
	copy(out, cliArgs)
	for i := range out {
		switch {
		case hide[out[i]]:
			if name, _, found := strings.Cut(out[i], "="); found && strings.HasPrefix(out[i], "-") {
				out[i] = name + "=<redacted>"
			} else {
				out[i] = "<redacted>"
			}
		case i > 0 && secret[out[i-1]]:
			out[i] = "<redacted>"
		}
	}
	return out
}
