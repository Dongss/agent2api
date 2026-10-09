package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

func newFlags(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return fs
}

// isolate runs the test in a scratch working directory, so one that writes a
// file cannot touch the repo.
func isolate(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

func TestLoadDefaults(t *testing.T) {
	isolate(t)
	cfg, prov, err := Load(newFlags(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 8055 {
		t.Errorf("unexpected listener %s:%d", cfg.Server.Host, cfg.Server.Port)
	}
	if cfg.Server.RequestTimeout != 600 || cfg.Server.RequestTimeout.Duration() != 10*time.Minute {
		t.Errorf("request_timeout = %v, want 600 seconds", cfg.Server.RequestTimeout)
	}
	// Every backend is a candidate by default; probing decides which run.
	// Compared against the registry rather than a count, so adding an adapter
	// does not fail here for the wrong reason.
	if got := cfg.AdapterIDs(); len(got) != len(KnownAdapters) {
		t.Errorf("candidate adapters = %v, want all of %v", got, KnownAdapters)
	}
	if cfg.Server.MaxConcurrency != 4 {
		t.Errorf("max_concurrency = %d, want 4", cfg.Server.MaxConcurrency)
	}
	if prov.File != "" {
		t.Errorf("provenance file = %q, want empty", prov.File)
	}
	if got := prov.source("server.port"); got != SourceDefault {
		t.Errorf("server.port source = %s, want default", got)
	}
}

func TestPrecedenceDefaultsFileFlags(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "agent2api.yaml")
	body := `
server:
  port: 9100
  idle_timeout: 45
log:
  level: debug
adapters:
  claude-code:
    binary: /opt/claude/bin/claude
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// The file wins over defaults; the flag wins over the file; keys nobody
	// touched keep their default.
	cfg, prov, err := Load(newFlags(t, "--config", path, "--port", "9200"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9200 {
		t.Errorf("port = %d, want 9200 (flag)", cfg.Server.Port)
	}
	if cfg.Server.IdleTimeout != 45 {
		t.Errorf("idle_timeout = %v, want 45 (file)", cfg.Server.IdleTimeout)
	}
	if cfg.Server.RequestTimeout != 600 {
		t.Errorf("request_timeout = %v, want 600 (default)", cfg.Server.RequestTimeout)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want debug", cfg.Log.Level)
	}
	if got := cfg.Adapters["claude-code"].Binary; got != "/opt/claude/bin/claude" {
		t.Errorf("binary = %q, want the file's value", got)
	}
	if got := prov.source("server.port"); got != SourceFlag {
		t.Errorf("server.port source = %s, want flag", got)
	}
	if got := prov.source("server.idle_timeout"); got != SourceFile {
		t.Errorf("server.idle_timeout source = %s, want file", got)
	}
	if got := prov.source("server.request_timeout"); got != SourceDefault {
		t.Errorf("server.request_timeout source = %s, want default", got)
	}
}

func TestUnsetFlagsDoNotMaskFile(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "agent2api.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 9100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(newFlags(t, "--config", path, "--log-level", "warn"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9100 {
		t.Errorf("port = %d, want 9100: an unset flag masked the file value", cfg.Server.Port)
	}
}

// The flag set is deliberately small; everything else lives in the file. A
// dotted flag would have silently done nothing, so it must be rejected.
func TestDottedFlagsAreGone(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	RegisterFlags(fs)
	if err := fs.Parse([]string{"--adapters.claude-code.binary=/x"}); err == nil {
		t.Fatal("a per-key dotted flag must not be accepted")
	}
}

func TestUnknownKeyIsAnError(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "agent2api.yaml")
	if err := os.WriteFile(path, []byte("server:\n  prot: 9100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(newFlags(t, "--config", path)); err == nil {
		t.Fatal("want an error for the misspelled key 'prot', got nil")
	}
}

func TestUnknownAdapterIsAnError(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "agent2api.yaml")
	if err := os.WriteFile(path, []byte("adapters:\n  gemini:\n    binary: gemini\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(newFlags(t, "--config", path)); err == nil {
		t.Fatal("want an error for the unknown adapter id, got nil")
	}
}

func TestEnvPassthroughLoads(t *testing.T) {
	cfg, _, err := loadWith(t, "adapters:\n  codex:\n    env_passthrough: [PROVIDER_API_KEY]\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Adapters["codex"].EnvPassthrough; len(got) != 1 || got[0] != "PROVIDER_API_KEY" {
		t.Errorf("env_passthrough = %q", got)
	}
}

func TestEnvPassthroughIsValidated(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"not a name": {
			"adapters:\n  codex:\n    env_passthrough: [\"PROVIDER-API-KEY\"]\n",
			`adapters.codex.env_passthrough: "PROVIDER-API-KEY" is not an environment variable name`,
		},
		"a value, not a name": {
			"adapters:\n  codex:\n    env_passthrough: [\"PROVIDER_API_KEY=sk-x\"]\n",
			"is not an environment variable name",
		},
		"in both lists": {
			"adapters:\n  qwen-code:\n    env:\n      PROVIDER_API_KEY: sk-x\n    env_passthrough: [PROVIDER_API_KEY]\n",
			"adapters.qwen-code: PROVIDER_API_KEY is in both env and env_passthrough",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := loadWith(t, c.body)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestMissingExplicitConfigIsAnError(t *testing.T) {
	isolate(t)
	if _, _, err := Load(newFlags(t, "--config", "/nonexistent/agent2api.yaml")); err == nil {
		t.Fatal("want an error for a missing --config path, got nil")
	}
}

func TestNonLoopbackRequiresAPIKey(t *testing.T) {
	isolate(t)
	if _, _, err := Load(newFlags(t, "--host", "0.0.0.0")); err == nil {
		t.Fatal("binding beyond loopback without a bearer key must fail")
	}
	if _, _, err := Load(newFlags(t, "--host", "0.0.0.0", "--api-key", "secret")); err != nil {
		t.Fatalf("binding beyond loopback with a bearer key must work: %v", err)
	}
}

// There are no default locations. A file is read only when -c names it, so one
// sitting in the working directory is not configuration — it is just a file.
func TestConfigFileIsNotDiscovered(t *testing.T) {
	isolate(t)
	if err := os.WriteFile("agent2api.yaml", []byte("server:\n  port: 9300\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := Load(newFlags(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != DefaultPort {
		t.Errorf("port = %d, want the default %d: a file in the cwd must be ignored", cfg.Server.Port, DefaultPort)
	}
	if prov.File != "" {
		t.Errorf("provenance names %q; nothing was asked for", prov.File)
	}

	// Naming the same file explicitly does load it.
	cfg, prov, err = Load(newFlags(t, "--config", "agent2api.yaml"))
	if err != nil {
		t.Fatalf("Load with --config: %v", err)
	}
	if cfg.Server.Port != 9300 {
		t.Errorf("port = %d, want 9300 once the file is named", cfg.Server.Port)
	}
	if prov.File == "" {
		t.Error("provenance should record the file that was loaded")
	}
}

func TestReportRedactsTheAPIKey(t *testing.T) {
	isolate(t)
	cfg, prov, err := Load(newFlags(t, "--api-key", "sk-secret-value"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKey != "sk-secret-value" {
		t.Errorf("api_key = %q, want the flag's value", cfg.Server.APIKey)
	}
	report := prov.Report()
	if strings.Contains(report, "sk-secret-value") {
		t.Error("config print must not reveal the bearer key")
	}
	if !strings.Contains(report, "[redacted]") {
		t.Errorf("the key should be shown as redacted, not omitted:\n%s", report)
	}
	if !strings.Contains(report, "server.port") {
		t.Error("report should list every effective key")
	}
}

// Every flag must land on a key the schema actually defines, or it would parse
// and then quietly do nothing.
func TestEveryFlagMapsToARealKey(t *testing.T) {
	isolate(t)
	// The schema is the defaults struct, loaded exactly as Load loads it.
	schema := koanf.New(Delim)
	if err := schema.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		t.Fatal(err)
	}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	args := []string{
		"--host", "127.0.0.1", "--port", "1", "--api-key", "k",
		"--log-level", "warn", "--log-file", "/tmp/x", "--max-concurrency", "2",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	over, err := flagOverrides(fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(over) == 0 {
		t.Fatal("no overrides collected")
	}
	for key := range over {
		if !schema.Exists(key) {
			t.Errorf("a flag writes %q, which the schema does not define", key)
		}
	}
}
