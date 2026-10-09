package agentcli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/runner"
)

// The probe needs a CLI to inspect. As in the runner tests, the test binary
// re-executes itself as one rather than relying on a shell script.
const helperEnv = "AGENT2API_AGENTCLI_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helperMain(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// helperMain is a CLI with one documented flag and one it leaves out of
// --help, which refuses a value it does not take the way commander does.
func helperMain(args []string) {
	switch {
	case slices.Contains(args, "--hidden"):
		fmt.Fprintln(os.Stderr, "error: option '--hidden <v>' argument is invalid. Allowed choices are good.")
		os.Exit(1)
	case slices.Contains(args, "--help"):
		fmt.Println("  --documented   a flag --help shows")
	case slices.Contains(args, "--version"):
		fmt.Println("fake 1.0")
	}
}

func helperProbe(t *testing.T) *Probe {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to re-execute: %v", err)
	}
	return &Probe{
		Binary: exe,
		Env:    runner.Environ(nil, map[string]string{helperEnv: "1"}),
	}
}

func TestUndocumentedFlagsJoinTheFlagSet(t *testing.T) {
	p := helperProbe(t)
	p.Undocumented = func(_ context.Context, run func(...string) (string, error)) []string {
		var found []string
		for _, flag := range []string{"--hidden", "--absent"} {
			// A refusal that lists choices is the evidence; a clean exit means
			// the unknown flag was ignored.
			if _, err := run(flag, "__probe__", "--version"); err != nil {
				found = append(found, flag)
			}
		}
		return found
	}
	caps, err := p.Caps(context.Background())
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	for flag, want := range map[string]bool{"--documented": true, "--hidden": true, "--absent": false} {
		if caps.Has(flag) != want {
			t.Errorf("Has(%s) = %v, want %v", flag, caps.Has(flag), want)
		}
	}
	if caps.Version != "fake 1.0" {
		t.Errorf("version = %q", caps.Version)
	}
}

// The refusal the hook relies on must reach it: commander prints it on stderr
// and exits non-zero, and both halves have to survive the probe's run.
func TestUndocumentedSeesTheRefusal(t *testing.T) {
	p := helperProbe(t)
	var detail string
	p.Undocumented = func(_ context.Context, run func(...string) (string, error)) []string {
		_, err := run("--hidden", "__probe__")
		if err != nil {
			detail = err.Error()
		}
		return nil
	}
	if _, err := p.Caps(context.Background()); err != nil {
		t.Fatalf("a failed undocumented check must not fail the probe: %v", err)
	}
	if !strings.Contains(detail, "Allowed choices are good") {
		t.Errorf("the refusal did not reach the hook: %q", detail)
	}
}
