package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The file has to sit where the CLI looks for workspace permissions, and hold
// the read denial: that is the entry measured to keep a run's reads off the
// disk (see deniedPermissions).
func TestPermissionsFileDeniesReads(t *testing.T) {
	dir := t.TempDir()
	if err := writePermissions(dir); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".cursor", "cli.json"))
	if err != nil {
		t.Fatalf("the CLI reads workspace permissions from .cursor/cli.json: %v", err)
	}
	var file struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatalf("cli.json is not JSON: %v\n%s", err, body)
	}
	if len(file.Permissions.Allow) != 0 {
		t.Errorf("allow = %v, want nothing allowed", file.Permissions.Allow)
	}
	for _, want := range []string{"Read(**)", "Write(**)", "Shell(*)", "WebFetch(*)", "Mcp(*:*)"} {
		if !slices.Contains(file.Permissions.Deny, want) {
			t.Errorf("deny = %v, missing %s", file.Permissions.Deny, want)
		}
	}
}
