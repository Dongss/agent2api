package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// release is a fake GitHub releases host: the latest-tag redirect, one archive
// per platform, and a checksums.txt. Tests mutate the fields to model a broken
// release without needing a second server.
type release struct {
	tag     string
	archive []byte
	// sums, when non-nil, replaces the generated checksums.txt.
	sums []byte
	// missingAsset serves a 404 for the platform archive.
	missingAsset bool
}

func (r *release) start(t *testing.T) string {
	t.Helper()
	name := archiveName(r.tag)

	mux := http.NewServeMux()
	mux.HandleFunc("/o/r/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/o/r/releases/tag/"+r.tag)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/o/r/releases/download/"+r.tag+"/"+name, func(w http.ResponseWriter, _ *http.Request) {
		if r.missingAsset {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(r.archive)
	})
	mux.HandleFunc("/o/r/releases/download/"+r.tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		if r.sums != nil {
			_, _ = w.Write(r.sums)
			return
		}
		sum := sha256.Sum256(r.archive)
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// tarGz packs content as the release binary, the way scripts/build.sh output is
// shipped.
func tarGz(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: binaryInArchive(), Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installed writes a stand-in for an already-installed binary and returns its
// path. Tests point Options.Dest at it so nothing replaces the test binary.
func installed(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent2api")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunUpdates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the zip path is exercised by fromZip; the swap dance needs a real running exe")
	}
	rel := &release{tag: "v9.9.9", archive: tarGz(t, "new binary")}
	dest := installed(t, "old binary")

	res, err := Run(context.Background(), Options{
		Current: "v0.1.0", Repo: "o/r", BaseURL: rel.start(t), Dest: dest,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Updated || res.From != "v0.1.0" || res.To != "v9.9.9" {
		t.Errorf("got %+v, want an update v0.1.0 -> v9.9.9", res)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Errorf("binary content = %q, want %q", got, "new binary")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: mode %v", info.Mode())
	}
	// The staging file must not survive a successful update.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the binary", len(entries))
	}
}

func TestRunAlreadyCurrent(t *testing.T) {
	rel := &release{tag: "v9.9.9", archive: tarGz(t, "new binary")}
	dest := installed(t, "old binary")

	res, err := Run(context.Background(), Options{
		Current: "v9.9.9", Repo: "o/r", BaseURL: rel.start(t), Dest: dest,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Updated {
		t.Error("Updated = true, want false when already on the latest tag")
	}
	if got, _ := os.ReadFile(dest); string(got) != "old binary" {
		t.Errorf("binary was rewritten: %q", got)
	}
}

// A bad or absent checksum must leave the old binary in place: this is the rule
// that differs from the install scripts, so it is the one worth pinning.
func TestRunRefusesUnverifiedDownload(t *testing.T) {
	archive := tarGz(t, "new binary")
	tests := []struct {
		name string
		sums []byte
		want string
	}{
		{"mismatch", []byte(strings.Repeat("a", 64) + "  " + archiveName("v9.9.9") + "\n"), "checksum mismatch"},
		{"not listed", []byte(strings.Repeat("a", 64) + "  something-else.tar.gz\n"), "does not list"},
		{"empty", []byte(""), "does not list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rel := &release{tag: "v9.9.9", archive: archive, sums: tc.sums}
			dest := installed(t, "old binary")

			_, err := Run(context.Background(), Options{
				Current: "v0.1.0", Repo: "o/r", BaseURL: rel.start(t), Dest: dest,
			})
			if err == nil {
				t.Fatal("Run succeeded, want a verification failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if got, _ := os.ReadFile(dest); string(got) != "old binary" {
				t.Errorf("binary was replaced despite a failed check: %q", got)
			}
		})
	}
}

func TestRunMissingAssetForPlatform(t *testing.T) {
	rel := &release{tag: "v9.9.9", archive: tarGz(t, "new"), missingAsset: true}
	dest := installed(t, "old binary")

	_, err := Run(context.Background(), Options{
		Current: "v0.1.0", Repo: "o/r", BaseURL: rel.start(t), Dest: dest,
	})
	if err == nil {
		t.Fatal("Run succeeded, want a download failure")
	}
	// The message has to say which platform came up empty; that is the whole
	// diagnostic when a release skips an architecture.
	if !strings.Contains(err.Error(), runtime.GOARCH) {
		t.Errorf("error = %v, want it to name %s", err, runtime.GOARCH)
	}
	if got, _ := os.ReadFile(dest); string(got) != "old binary" {
		t.Errorf("binary was replaced: %q", got)
	}
}

func TestLatestTagNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := latestTag(context.Background(), Options{Repo: "o/r", BaseURL: srv.URL}.withDefaults())
	if err == nil {
		t.Fatal("latestTag succeeded against a repo with no releases")
	}
	if !strings.Contains(err.Error(), "no published release") {
		t.Errorf("error = %v, want it to explain there may be no release yet", err)
	}
}

func TestExtractRejectsArchiveWithoutBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = gz.Close()

	if _, err := extract(buf.Bytes(), archiveName("v1.0.0")); err == nil {
		t.Fatal("extract succeeded on an archive with no binary in it")
	}
}
