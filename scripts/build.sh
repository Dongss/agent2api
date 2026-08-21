#!/usr/bin/env bash
#
# Build the agent2api binary, stamped with the version it is.
#
#   scripts/build.sh                 # -> ./agent2api
#   scripts/build.sh dist/agent2api  # -> that path
#   VERSION=v0.1.0 scripts/build.sh  # override what gets stamped
#
# The version comes from the git tag (`git describe`), so the tag is the single
# source of truth: `git tag v0.1.0` is what makes a build call itself v0.1.0.
# A build from an untagged or dirty tree is left unstamped on purpose — the
# binary then reports "devel+<commit>" from Go's build info, which is the honest
# answer and cannot be mistaken for a release.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$root/agent2api}"
cd "$root"

version="${VERSION:-}"
if [[ -z "$version" ]] && git rev-parse --git-dir >/dev/null 2>&1; then
	# --dirty and no --always: an uncommitted tree, or one with no tag reachable,
	# yields nothing and stays unstamped.
	version="$(git describe --tags --exact-match --dirty 2>/dev/null || true)"
	[[ "$version" == *-dirty ]] && version=""
fi

ldflags=""
if [[ -n "$version" ]]; then
	ldflags="-X github.com/Dongss/agent2api/internal/version.Version=$version"
	echo "building $version -> $out"
else
	echo "building unstamped (no exact tag on a clean tree) -> $out"
fi

go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/agent2api
"$out" version
