package runner

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// baseAllowlist is the environment every agent CLI gets: enough to find itself,
// its config and the network, and nothing else. agent2api's own environment is
// never passed through wholesale — a CLI launched with the gateway's full env
// can pick up credentials and settings the caller never asked for.
var baseAllowlist = []string{
	"HOME", "PATH", "USER", "LOGNAME", "SHELL",
	"TMPDIR", "TMP", "TEMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	// Windows needs these to resolve the user profile and system directories.
	"APPDATA", "LOCALAPPDATA", "USERPROFILE", "SYSTEMROOT", "SystemRoot", "COMSPEC", "PATHEXT",
}

// Environ builds the child environment: the base allowlist, plus any variable
// matching one of allowPrefixes (adapters use this to let their own vendor
// variables through, e.g. "ANTHROPIC_"), plus explicit overrides from extra.
//
// The result is sorted, so argv/env is reproducible across runs.
func Environ(allowPrefixes []string, extra map[string]string) []string {
	env := make(map[string]string, len(baseAllowlist)+len(extra))
	for _, k := range baseAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if len(allowPrefixes) > 0 {
		for _, kv := range os.Environ() {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			for _, p := range allowPrefixes {
				if strings.HasPrefix(k, p) {
					env[k] = v
					break
				}
			}
		}
	}
	for k, v := range extra {
		env[k] = v
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// ScratchDir creates an empty per-request working directory under root (or the
// OS temp directory when root is empty) and returns it with a cleanup func.
//
// Running the CLI here is defense in depth: even if a tool-disable flag is
// renamed or ignored by a future CLI version, the process starts somewhere with
// nothing in it rather than in the gateway's own directory.
func ScratchDir(root, prefix string) (dir string, cleanup func(), err error) {
	if root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", nil, err
		}
	}
	dir, err = os.MkdirTemp(root, prefix)
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// ExpandPath resolves a leading "~" against the user's home directory.
func ExpandPath(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
