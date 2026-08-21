// Package version reports the build's identity.
package version

import "runtime/debug"

// Version is empty in a plain `go build`, so a working-tree binary describes
// itself by the commit it came from rather than claiming to be a release.
// Release builds stamp it from the git tag; see scripts/build.sh.
var Version = ""

// String returns the release version, falling back to the VCS revision the
// module was built from.
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				revision = s.Value[:12]
			} else {
				revision = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		return "devel"
	}
	return "devel+" + revision + modified
}
