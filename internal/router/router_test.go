package router

import (
	"strings"
	"testing"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/adapter/fake"
	"github.com/Dongss/agent2api/internal/ir"
)

func testAdapters() []adapter.Adapter {
	return []adapter.Adapter{fake.New("claude-code", "hi")}
}

func TestResolvePassesTheVariantThrough(t *testing.T) {
	r, err := New(testAdapters())
	if err != nil {
		t.Fatal(err)
	}
	// Any variant is accepted: the CLI, not agent2api, owns the model list.
	for _, name := range []string{"opus", "sonnet", "some-model-shipped-tomorrow"} {
		res, err := r.Resolve("claude-code:" + name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if res.Variant != name || res.Adapter.ID() != "claude-code" {
			t.Errorf("resolved %q to %+v", name, res)
		}
	}
}

func TestBareAdapterIDMeansTheCLIsOwnDefault(t *testing.T) {
	r, err := New(testAdapters())
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Variant != "" {
		t.Errorf("variant = %q, want empty so the CLI picks", res.Variant)
	}
	if res.Model != "claude-code" {
		t.Errorf("model = %q", res.Model)
	}
}

func TestUnknownModelIs404(t *testing.T) {
	r, err := New(testAdapters())
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve("gpt-4o")
	if err == nil {
		t.Fatal("want an error for an unknown model")
	}
	e := ir.AsError(err)
	if e.Code != ir.CodeModelNotFound {
		t.Errorf("code = %s, want model_not_found", e.Code)
	}
	// The message must help: name what is being served.
	if !strings.Contains(e.Message, "claude-code") {
		t.Errorf("message should name the available backends, got %q", e.Message)
	}
}

func TestEntriesListOneRowPerBackend(t *testing.T) {
	r, err := New([]adapter.Adapter{
		fake.New("cursor", "hi"),
		fake.New("claude-code", "hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := r.ModelIDs()
	want := []string{"claude-code", "cursor"}
	if len(ids) != len(want) {
		t.Fatalf("models = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("models = %v, want %v", ids, want)
		}
	}
}

func TestEmptyModelName(t *testing.T) {
	r, err := New(testAdapters())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("  "); ir.AsError(err).Code != ir.CodeInvalidRequest {
		t.Errorf("an empty model name should be a 400, got %v", err)
	}
}
