package agentcli

import (
	"strings"
	"testing"
)

func TestParseFlagNames(t *testing.T) {
	help := `Options:
  --add-dir <directories...>            Additional directories
  --allowedTools, --allowed-tools <tools...>
  -p, --print                           Print response and exit
  --output-format <format>              Output format
  --setting-sources <sources>           Comma-separated list
  -s, --sandbox <SANDBOX_MODE>          [possible values: read-only]`
	flags := ParseFlagNames(help)
	for _, want := range []string{
		"--add-dir", "--allowedTools", "--allowed-tools", "--print",
		"--output-format", "--setting-sources", "--sandbox",
	} {
		if !flags[want] {
			t.Errorf("%s not detected in help output", want)
		}
	}
	if flags["--nonexistent"] {
		t.Error("detected a flag that is not in the help output")
	}
}

func TestRedactArgs(t *testing.T) {
	args := []string{"--print", "--append-system-prompt", "secret instructions", "--model", "opus"}
	got := RedactArgs(args, []string{"--system-prompt", "--append-system-prompt"})
	if got[2] != "<redacted>" {
		t.Errorf("system prompt not redacted: %v", got)
	}
	if args[2] != "secret instructions" {
		t.Error("RedactArgs must not mutate its input")
	}
	if got[4] != "opus" {
		t.Errorf("non-prompt args must survive: %v", got)
	}
}

// A CLI that takes its prompt as a positional argument needs the prompt itself
// redacted, not just flag values.
func TestRedactArgsLiterals(t *testing.T) {
	args := []string{"--print", "how do I hide this?", "--model", "composer"}
	got := RedactArgs(args, nil, "how do I hide this?")
	if got[1] != "<redacted>" {
		t.Errorf("positional prompt not redacted: %v", got)
	}
	if got[3] != "composer" {
		t.Errorf("non-prompt args must survive: %v", got)
	}
}

// A hidden --flag=value token keeps the flag name: which setting was passed is
// the useful half, and not the secret half.
func TestRedactArgsKeepsTheFlagNameOfAnInlineValue(t *testing.T) {
	args := []string{"--token=sk-secret", "--plain", "sk-other"}
	got := RedactArgs(args, nil, "--token=sk-secret", "sk-other")
	if got[0] != "--token=<redacted>" {
		t.Errorf("got[0] = %q, want the flag name kept", got[0])
	}
	if got[1] != "--plain" {
		t.Errorf("got[1] = %q, want the flag untouched", got[1])
	}
	if got[2] != "<redacted>" {
		t.Errorf("got[2] = %q, want a bare value fully hidden", got[2])
	}
}

func TestExtraArgValues(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  []string
	}{
		{"flags only", []string{"--verbose", "-v"}, nil},
		{"flag and value", []string{"--model", "opus"}, []string{"opus"}},
		{"inline value", []string{"--token=sk-x"}, []string{"--token=sk-x"}},
		{"bare value", []string{"secret"}, []string{"secret"}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtraArgValues(tc.extra)
			if len(got) != len(tc.want) {
				t.Fatalf("= %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	if got := FirstLine("\n 2.1.220 (Claude Code)\nextra\n"); got != "2.1.220 (Claude Code)" {
		t.Errorf("FirstLine = %q", got)
	}
	if got := FirstLine(""); got != "" {
		t.Errorf("FirstLine of empty = %q", got)
	}
}

func TestUnsetPassthroughNamesWhatIsMissing(t *testing.T) {
	t.Setenv("AGENTCLI_TEST_SET", "x")
	notes := UnsetPassthrough("codex", []string{"AGENTCLI_TEST_SET", "AGENTCLI_TEST_UNSET_XYZ"})
	if len(notes) != 1 {
		t.Fatalf("want one note, for the unset name; got %q", notes)
	}
	for _, want := range []string{"adapters.codex.env_passthrough", "AGENTCLI_TEST_UNSET_XYZ"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("the note should name %q: %s", want, notes[0])
		}
	}
}
