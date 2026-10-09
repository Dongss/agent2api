package claudecode

import (
	"context"
	"slices"
	"testing"

	"github.com/Dongss/agent2api/internal/adapter/agentcli"
	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/prompt"
)

// The three answers `claude --thinking-display __agent2api_probe__ --version`
// can give, the first two as 2.1.295 and an older release gave them.
func TestThinkingDisplayProbe(t *testing.T) {
	refused := func(choices string) func(...string) (string, error) {
		return func(...string) (string, error) {
			return "", &ir.Error{Code: ir.CodeUpstreamUnavailable, Message: "`claude …` failed (exit 1)",
				Detail: "error: option '--thinking-display <display>' argument '__agent2api_probe__' is invalid. Allowed choices are " + choices + "."}
		}
	}
	cases := []struct {
		name string
		run  func(...string) (string, error)
		want bool
	}{
		{"has the flag", refused("summarized, omitted, highlights"), true},
		{"ignores an unknown flag", func(...string) (string, error) { return "2.1.278 (Claude Code)\n", nil }, false},
		// Passing a value the CLI no longer takes would fail every request.
		{"dropped the value we pass", refused("omitted, highlights"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := slices.Contains(undocumentedFlags(context.Background(), c.run), "--thinking-display")
			if got != c.want {
				t.Errorf("found --thinking-display = %v, want %v", got, c.want)
			}
		})
	}
}

func TestThinkingDisplayIsPassedWhenAvailable(t *testing.T) {
	a := &Adapter{}
	msgs := []ir.Message{{Role: ir.RoleUser, Content: "hi"}}
	rendered := prompt.Render(msgs)
	for _, has := range []bool{true, false} {
		caps := agentcli.NewCaps("/bin/claude", "x", map[string]bool{"--thinking-display": has})
		args, _ := a.buildArgs(caps, ir.Request{Messages: msgs}, rendered)
		i := slices.Index(args, "--thinking-display")
		switch {
		case has && (i < 0 || args[i+1] != "summarized"):
			t.Errorf("want --thinking-display summarized, got %q", args)
		case !has && i >= 0:
			t.Errorf("an install without the flag must not be passed it: %q", args)
		}
	}
}
