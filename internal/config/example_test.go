package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// With the defaults living in Go, agent2api.example.yaml is the only
// user-facing tour of the key space — which makes it the thing most likely to
// drift out of the schema without anyone noticing.
const examplePath = "../../agent2api.example.yaml"

func exampleBody(t *testing.T) []byte {
	t.Helper()
	// Resolve before any test moves the working directory.
	abs, err := filepath.Abs(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The example must load. It is the first thing a user copies, so a key the
// schema no longer accepts would make it a startup error on arrival.
func TestExampleConfigLoads(t *testing.T) {
	body := exampleBody(t)
	isolate(t)
	path := filepath.Join(t.TempDir(), "agent2api.yaml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(newFlags(t, "--config", path)); err != nil {
		t.Fatalf("agent2api.example.yaml does not load: %v", err)
	}
}

// Every schema field should be documented, and every documented key should
// exist: the first catches a new field nobody wrote down, the second a renamed
// one still documented under its old name.
//
// Adapter paths compare per field, not per adapter: sandbox belongs to codex and
// system_prompt_mode to claude-code, so requiring each under every adapter would
// mislead rather than be thorough.
func TestExampleConfigCoversTheSchema(t *testing.T) {
	schema := koanf.New(Delim)
	if err := schema.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		t.Fatal(err)
	}

	documented := documentedKeys(t, exampleBody(t))
	if len(documented) == 0 {
		t.Fatal("no keys parsed out of the example; this test has stopped testing anything")
	}

	for _, key := range schema.Keys() {
		if !documented[perField(key)] {
			t.Errorf("%s is in the schema but not documented in agent2api.example.yaml", key)
		}
	}
	for key := range documented {
		if !schema.Exists(key) && !anyAdapterHas(schema, key) {
			t.Errorf("agent2api.example.yaml documents %s, which the schema does not define", key)
		}
	}
}

// perField collapses adapters.<id>.<field> to adapters.*.<field>.
func perField(key string) string {
	parts := strings.Split(key, Delim)
	if len(parts) >= 3 && parts[0] == "adapters" {
		return "adapters.*." + strings.Join(parts[2:], Delim)
	}
	return key
}

// anyAdapterHas reports whether a documented adapters.<id>.<field> path names a
// field the Adapter schema has, under any adapter.
func anyAdapterHas(schema *koanf.Koanf, key string) bool {
	parts := strings.Split(key, Delim)
	if len(parts) < 3 || parts[0] != "adapters" {
		return false
	}
	field := strings.Join(parts[2:], Delim)
	for _, known := range schema.Keys() {
		if perField(known) == "adapters.*."+field {
			return true
		}
	}
	return false
}

// commentedKey matches a setting the example shows but leaves switched off.
// Requiring nothing after the colon keeps prose out: a comment like
// "# append:  keep the CLI's own prompt" is documentation, not a key.
var commentedKey = regexp.MustCompile(`^(\s*)#\s*([a-z_]+):\s*(#.*)?$`)

// documentedKeys collects the dotted paths the example mentions, whether active
// or commented out.
func documentedKeys(t *testing.T, body []byte) map[string]bool {
	t.Helper()

	// The active keys come from the YAML parser, so they are exact.
	active, err := parseYAML(body)
	if err != nil {
		t.Fatalf("the example is not valid YAML: %v", err)
	}
	keys := map[string]bool{}
	for key := range flatten(active) {
		keys[perField(key)] = true
	}

	// The commented-out ones need the surrounding section, which the parser has
	// already thrown away, so walk the lines for those.
	var section []string
	for _, line := range strings.Split(string(body), "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		depth := indent / 2

		if m := commentedKey.FindStringSubmatch(line); m != nil {
			if depth <= len(section) {
				keys[perField(strings.Join(append(section[:depth:depth], m[2]), Delim))] = true
			}
			continue
		}
		// An active key line, which sets the section for what follows.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if name, _, ok := strings.Cut(trimmed, ":"); ok && !strings.ContainsAny(name, " \t\"'[]{},") {
			if depth <= len(section) {
				section = append(section[:depth:depth], name)
			}
		}
	}
	return keys
}
