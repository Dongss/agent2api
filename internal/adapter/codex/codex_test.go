package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/agentcli"
	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/ir"
)

// The adapter asks the CLI for its MCP servers on every request. As in the
// agentcli tests, the test binary re-executes itself as that CLI.
const helperEnv = "AGENT2API_CODEX_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		helperMain(mode, os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// helperMain answers `mcp list --json` the way 0.163.0 did on the recording
// machine: node_repl from config.toml enabled, computer-use disabled, and the
// plugin-provided cua_repl listed only while plugins are on.
func helperMain(mode string, args []string) {
	switch mode {
	case "broken":
		fmt.Fprintln(os.Stderr, "Error loading config.toml: invalid transport")
		os.Exit(1)
	case "dotted":
		fmt.Println(`[{"name":"team.repl","enabled":true}]`)
		return
	}
	servers := []string{`{"name":"computer-use","enabled":false}`, `{"name":"node_repl","enabled":true}`}
	if !slices.Contains(args, "features.plugins=false") {
		servers = append(servers, `{"name":"cua_repl","enabled":true}`)
	}
	fmt.Println("[" + strings.Join(servers, ",") + "]")
}

func helperAdapter(t *testing.T, mode string) *Adapter {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to re-execute: %v", err)
	}
	a, err := New(adapter.Options{ID: ID, Config: config.Adapter{
		Binary: exe,
		Env:    map[string]string{helperEnv: mode},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Adapter)
}

// capsWithFeatures is an install whose `features list` names the given
// features and that documents every flag the adapter looks for.
func capsWithFeatures(features ...string) *agentcli.Caps {
	set := map[string]bool{"--ephemeral": true, "--ignore-rules": true, "--color": true, "--model": true}
	for _, f := range features {
		set[featurePrefix+f] = true
	}
	return agentcli.NewCaps("/usr/bin/codex", "test", set)
}

// pairs returns the value following each "-c" in argv.
func pairs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestKnownFeaturesReadsTheList(t *testing.T) {
	list := "apps                                     stable             true\n" +
		"search_tool                              removed            false\n" +
		"shell_tool                               stable             true\n\n"
	got := knownFeatures(context.Background(), func(...string) (string, error) { return list, nil })
	want := []string{"features.apps", "features.search_tool", "features.shell_tool"}
	if !slices.Equal(got, want) {
		t.Errorf("features = %q, want %q", got, want)
	}

	failed := func(...string) (string, error) { return "", errors.New("unrecognized subcommand 'features'") }
	if got := knownFeatures(context.Background(), failed); got != nil {
		t.Errorf("an install without `features list` should report none, got %q", got)
	}
}

// An unknown feature fails the run (`Unknown feature flag: …`), so a tool
// feature this install does not list is left out of argv.
func TestOnlyListedFeaturesAreSwitchedOff(t *testing.T) {
	got := pairs(featureOverrides(capsWithFeatures("goals", "shell_tool", "some_unrelated_feature")))
	want := []string{"features.shell_tool=false", "features.goals=false"}
	if !slices.Equal(got, want) {
		t.Errorf("overrides = %q, want %q", got, want)
	}
}

func TestArgvSwitchesToolsOff(t *testing.T) {
	a := helperAdapter(t, "config")
	mcp := []string{"-c", "mcp_servers.node_repl.enabled=false"}
	args := a.buildArgs(capsWithFeatures(toolFeatures...), ir.Request{}, "/tmp/scratch", mcp)

	got := pairs(args)
	for _, f := range toolFeatures {
		if !slices.Contains(got, "features."+f+"=false") {
			t.Errorf("feature %s is not switched off: %q", f, args)
		}
	}
	for _, want := range []string{`web_search="disabled"`, "mcp_servers.node_repl.enabled=false"} {
		if !slices.Contains(got, want) {
			t.Errorf("argv lacks -c %s: %q", want, args)
		}
	}
	if args[len(args)-1] != "-" {
		t.Errorf("argv must end by reading the prompt from stdin: %q", args)
	}
}

// Plugin servers are listed with plugins off, which leaves them out: naming one
// that is not in config.toml is a config error that would fail every request.
func TestEnabledMCPServersAreSwitchedOff(t *testing.T) {
	a := helperAdapter(t, "config")
	got, err := a.mcpOverrides(context.Background(), capsWithFeatures(toolFeatures...))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", "mcp_servers.node_repl.enabled=false"}
	if !slices.Equal(got, want) {
		t.Errorf("overrides = %q, want %q", got, want)
	}
}

func TestMCPServersThatCannotBeSwitchedOffRefuseTheRequest(t *testing.T) {
	for _, c := range []struct {
		mode, mention string
	}{
		{"dotted", `"team.repl"`},
		{"broken", "mcp list --json"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			_, err := helperAdapter(t, c.mode).mcpOverrides(context.Background(), capsWithFeatures(toolFeatures...))
			if err == nil {
				t.Fatal("the request should be refused")
			}
			e := ir.AsError(err)
			if e.Code != ir.CodeUpstreamUnavailable || !strings.Contains(e.Message, c.mention) {
				t.Errorf("error = %+v, want unavailable naming %s", e, c.mention)
			}
		})
	}
}
