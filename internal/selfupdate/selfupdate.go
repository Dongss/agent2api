// Package selfupdate replaces the running agent2api binary with the latest
// published release.
//
// It deliberately mirrors scripts/install.sh — same release, same archive
// names, same checksums.txt — so a machine ends up with exactly the file the
// installer would have written. One rule is stricter: a checksum that is
// missing or does not match is fatal here. The installer is creating a file
// that did not exist and can reasonably warn and continue; update overwrites a
// binary the user already trusts, so it gets no "skipping verification" path.
package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// DefaultRepo is the GitHub repository releases are published to.
	DefaultRepo = "Dongss/agent2api"
	// defaultBaseURL is where release assets live. Tests point this at an
	// httptest server so no test ever reaches the network.
	defaultBaseURL = "https://github.com"
	// binaryName is the executable inside every release archive.
	binaryName = "agent2api"
)

// Options configures one update. The zero value updates the running binary
// from the project's own GitHub releases, which is what the CLI passes.
type Options struct {
	// Current is the version this build reports, e.g. "v0.1.0". An update is
	// skipped when it already equals the latest tag.
	Current string
	// Repo is "owner/name"; defaults to [DefaultRepo].
	Repo string
	// BaseURL overrides the release host. For tests.
	BaseURL string
	// Client overrides the HTTP client. For tests.
	Client *http.Client
	// Dest is the binary to replace. Empty means the running executable.
	Dest string
}

// Result describes what an update did, including the case where it did
// nothing because the binary was already current.
type Result struct {
	// From is the version that was installed, To the latest release. They are
	// equal exactly when Updated is false.
	From, To string
	// Path is the binary that was (or would have been) replaced.
	Path string
	// Updated reports whether the binary on disk changed.
	Updated bool
}

func (o Options) withDefaults() Options {
	if o.Repo == "" {
		o.Repo = DefaultRepo
	}
	if o.BaseURL == "" {
		o.BaseURL = defaultBaseURL
	}
	if o.Client == nil {
		// No overall client timeout: the caller's context bounds the whole
		// update, and a release archive on a slow link must not be cut off
		// by a duration picked here.
		o.Client = &http.Client{Timeout: 0}
	}
	o.BaseURL = strings.TrimSuffix(o.BaseURL, "/")
	return o
}

// Run updates the binary to the latest release.
//
// Nothing on disk is touched until the download is complete and verified, so a
// network failure, a bad checksum or a cancelled context all leave the existing
// binary exactly as it was.
func Run(ctx context.Context, opts Options) (Result, error) {
	opts = opts.withDefaults()

	dest, err := destination(opts.Dest)
	if err != nil {
		return Result{}, err
	}
	res := Result{From: opts.Current, To: opts.Current, Path: dest}

	// Fail on an unwritable or package-managed destination before spending a
	// download on an update that could never be installed.
	if err := checkWritable(dest); err != nil {
		return res, err
	}

	latest, err := latestTag(ctx, opts)
	if err != nil {
		return res, err
	}
	res.To = latest
	if latest == opts.Current {
		return res, nil
	}

	bin, err := download(ctx, opts, latest)
	if err != nil {
		return res, err
	}
	if err := replace(dest, bin); err != nil {
		return res, err
	}
	res.Updated = true
	return res, nil
}

// destination resolves which file to overwrite. Symlinks are followed: an
// install that links ~/.local/bin/agent2api at a versioned path wants the
// binary replaced, not the link turned into a regular file.
func destination(override string) (string, error) {
	path := override
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot tell which binary is running: %w", err)
		}
		path = exe
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	return resolved, nil
}

// checkWritable reports whether the new binary can be installed, before a
// download is spent on an update that could not land.
//
// Writability belongs to the directory, not the file: installing is a rename
// into the directory, not a write through the old inode. A root-owned
// /usr/local/bin holding a user-owned binary would pass the wrong check.
func checkWritable(dest string) error {
	dir := filepath.Dir(dest)
	probe, err := os.CreateTemp(dir, ".agent2api-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w\n"+
			"re-run the install script, or move agent2api to a directory you own (e.g. ~/.local/bin)", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// latestTag reads the tag off the /releases/latest redirect rather than asking
// the REST API, which rate-limits unauthenticated callers to 60 requests an
// hour per address. scripts/install.sh dodges the same limit the same way.
func latestTag(ctx context.Context, opts Options) (string, error) {
	url := opts.BaseURL + "/" + opts.Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}

	// The redirect target is the answer, so stop before following it.
	client := *opts.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s to find the latest release: %w", url, err)
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("cannot find the latest release of %s (HTTP %d)\n"+
			"the network may be down, or the project may have no published release yet", opts.Repo, resp.StatusCode)
	}
	_, tag, ok := strings.Cut(loc, "/releases/tag/")
	if !ok || tag == "" || strings.Contains(tag, "/") {
		return "", fmt.Errorf("cannot tell the latest version from %s", loc)
	}
	return tag, nil
}

// download fetches the release archive for this platform, verifies it, and
// returns the binary inside it.
func download(ctx context.Context, opts Options, tag string) ([]byte, error) {
	base := fmt.Sprintf("%s/%s/releases/download/%s", opts.BaseURL, opts.Repo, tag)
	name := archiveName(tag)

	archive, err := fetch(ctx, opts.Client, base+"/"+name)
	if err != nil {
		return nil, fmt.Errorf("cannot download %s: %w\n"+
			"is %s released for %s/%s?", name, err, tag, runtime.GOOS, runtime.GOARCH)
	}
	sums, err := fetch(ctx, opts.Client, base+"/checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("cannot download the checksums for %s: %w", tag, err)
	}
	if err := verify(archive, sums, name); err != nil {
		return nil, err
	}
	return extract(archive, name)
}

// archiveName is the release asset for this platform. It must stay in step with
// scripts/build.sh and both install scripts.
func archiveName(tag string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", binaryName, tag, runtime.GOOS, runtime.GOARCH, ext)
}

// binaryInArchive is what the extracted executable is called.
func binaryInArchive() string {
	if runtime.GOOS == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}
