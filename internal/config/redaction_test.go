package config

import (
	"strings"
	"testing"
)

// `config print` is meant to be pasted into bug reports, so every field that
// can hold a credential has to survive that. The names stay: "is this set, and
// from which layer?" is what the report is read for.
func TestReportRedactsEverySecretBearingField(t *testing.T) {
	body := `
server:
  api_key: "sk-gateway-secret"
adapters:
  claude-code:
    env:
      ANTHROPIC_AUTH_TOKEN: "sk-env-secret"
      ANTHROPIC_BASE_URL: "https://gateway.internal"
    extra_args: ["--verbose", "--model", "opus", "--token=sk-inline-secret"]
`
	_, prov, err := loadWith(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	report := prov.Report()

	for _, secret := range []string{
		"sk-gateway-secret", // server.api_key
		"sk-env-secret",     // an env value
		"gateway.internal",  // an env value that only looks harmless
		"sk-inline-secret",  // the value half of --token=...
		"opus",              // the value after a flag in extra_args
	} {
		if strings.Contains(report, secret) {
			t.Errorf("%q reached the report:\n%s", secret, report)
		}
	}

	// The names, and the flags, are the part worth keeping.
	for _, kept := range []string{
		"server.api_key",
		"adapters.claude-code.env.ANTHROPIC_AUTH_TOKEN",
		"adapters.claude-code.env.ANTHROPIC_BASE_URL",
		"--verbose",
		"--model",
		"--token=[redacted]",
	} {
		if !strings.Contains(report, kept) {
			t.Errorf("%q should still be visible:\n%s", kept, report)
		}
	}
}

// An unset value is rendered plainly. "Not configured" is not a secret, and
// hiding it would defeat the report's main use.
func TestReportShowsUnsetFieldsPlainly(t *testing.T) {
	_, prov, err := loadWith(t, "server:\n  host: 127.0.0.1\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	report := strings.Join(strings.Fields(prov.Report()), " ")
	for _, want := range []string{
		`server.api_key = "" (default)`,
		"adapters.claude-code.env = {} (default)",
		"adapters.claude-code.extra_args = [] (default)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("want %q in:\n%s", want, prov.Report())
		}
	}
}

func TestFormatExtraArgs(t *testing.T) {
	cases := []struct {
		name string
		args any
		want string
	}{
		{"empty", []any{}, "[]"},
		{"flags only", []any{"--verbose", "--json"}, "[--verbose, --json]"},
		{"flag with a separate value", []any{"--model", "opus"}, "[--model, [redacted]]"},
		{"flag with an inline value", []any{"--model=opus"}, "[--model=[redacted]]"},
		{"short flag", []any{"-v"}, "[-v]"},
		{"bare value", []any{"secret"}, "[[redacted]]"},
		{"typed slice", []string{"--verbose", "x"}, "[--verbose, [redacted]]"},
		{"not a list at all", "oops", "[redacted]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatExtraArgs(tc.args); got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSecretKeyClassification(t *testing.T) {
	secret := []string{
		"server.api_key",
		"adapters.claude-code.env.ANTHROPIC_AUTH_TOKEN",
		"adapters.codex.env.OPENAI_API_KEY",
	}
	plain := []string{
		"server.host",
		"server.port",
		"adapters.claude-code.binary",
		// The empty map itself carries no value to leak.
		"adapters.claude-code.env",
		"adapters.claude-code.extra_args",
	}
	for _, key := range secret {
		if !isSecretKey(key) {
			t.Errorf("%s should be treated as secret-bearing", key)
		}
	}
	for _, key := range plain {
		if isSecretKey(key) {
			t.Errorf("%s should not be redacted", key)
		}
	}
}
