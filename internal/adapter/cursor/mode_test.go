package cursor

import (
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/agentcli"
	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/ir"
)

// capsWith reports an install that documents exactly the given flags.
func capsWith(flags ...string) *agentcli.Caps {
	set := map[string]bool{}
	for _, f := range flags {
		set[f] = true
	}
	return agentcli.NewCaps("/usr/bin/cursor-agent", "test", set)
}

func irRequest() ir.Request { return ir.Request{Model: "cursor", Variant: ""} }

func newAdapter(t *testing.T, mode string) *Adapter {
	t.Helper()
	a, err := New(adapter.Options{
		ID:     ID,
		Config: config.Adapter{Binary: "cursor-agent", Mode: mode},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Adapter)
}

func TestModeDefaultsToAsk(t *testing.T) {
	if got := newAdapter(t, "").mode(); got != "ask" {
		t.Errorf("mode = %q, want ask", got)
	}
	// The adapter's fallback must not drift from the config default.
	if DefaultMode != config.DefaultCursorMode {
		t.Errorf("DefaultMode = %q, config says %q", DefaultMode, config.DefaultCursorMode)
	}
}

func TestConfiguredModeReachesArgv(t *testing.T) {
	for _, mode := range config.CursorModes {
		t.Run(mode, func(t *testing.T) {
			a := newAdapter(t, mode)
			if got := a.mode(); got != mode {
				t.Fatalf("mode = %q, want %q", got, mode)
			}
			args := a.buildArgs(capsWith("--mode"), irRequest(), "prompt text")
			if !containsPair(args, "--mode", mode) {
				t.Errorf("argv = %v, want --mode %s", args, mode)
			}
		})
	}
}

// A CLI without the flag must not be handed it; the answer is then whatever the
// CLI's own default mode does, which Probe reports as a note.
func TestModeIsOmittedWhenTheCLILacksTheFlag(t *testing.T) {
	args := newAdapter(t, "plan").buildArgs(capsWith(), irRequest(), "prompt text")
	for _, arg := range args {
		if arg == "--mode" {
			t.Errorf("argv = %v, want no --mode", args)
		}
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestModeValuesAreValidated(t *testing.T) {
	// The adapter passes the value through; the config layer is what refuses a
	// bad one, so make sure the two agree on the set.
	if strings.Join(config.CursorModes, ",") != "ask,plan" {
		t.Errorf("CursorModes = %v; this test and the CLI's --mode choices are out of step", config.CursorModes)
	}
}
