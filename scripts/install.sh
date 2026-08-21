#!/bin/sh
#
# Install agent2api on macOS or Linux:
#
#   curl -fsSL https://raw.githubusercontent.com/Dongss/agent2api/main/scripts/install.sh | sh
#
# Environment:
#   AGENT2API_VERSION       install this tag instead of the latest (e.g. v0.1.0)
#   AGENT2API_INSTALL_DIR   install here instead of ~/.local/bin
#
# The default directory needs no sudo. Nothing outside it is touched: if it is
# not on your PATH the script says so rather than editing your shell profile.
set -eu

REPO="Dongss/agent2api"
BIN="agent2api"
INSTALL_DIR="${AGENT2API_INSTALL_DIR:-$HOME/.local/bin}"

die() {
	echo "install.sh: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but was not found"
}

need curl
need tar

# --- what to download ------------------------------------------------------

os="$(uname -s)"
case "$os" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported operating system: $os (this script covers macOS and Linux; on Windows use scripts/install.ps1)" ;;
esac

arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture: $arch (releases cover amd64 and arm64)" ;;
esac

version="${AGENT2API_VERSION:-}"
if [ -z "$version" ]; then
	# Read the tag off the redirect rather than asking the API, which rate-limits
	# unauthenticated callers to 60 requests an hour per address — a limit an
	# install script should never be able to hit.
	latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")" ||
		die "cannot find the latest release of $REPO — the network may be down, or the project may have none published yet; set AGENT2API_VERSION to install a specific tag"
	version="${latest##*/tag/}"
	case "$version" in
	"" | *"/"*) die "cannot tell the latest version from $latest; set AGENT2API_VERSION to pick one" ;;
	esac
fi

archive="${BIN}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

# --- download and verify ---------------------------------------------------

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "installing $BIN $version ($os/$arch)"

curl -fsSL "$base/$archive" -o "$tmp/$archive" ||
	die "cannot download $base/$archive — is $version a released version for $os/$arch?"

# Checksums guard against a truncated download and against a single asset having
# been swapped; they are cheap enough to be worth doing every time.
if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
	expected="$(awk -v want="$archive" '$2 == want || $2 == "*" want { print $1 }' "$tmp/checksums.txt")"
	if [ -z "$expected" ]; then
		echo "install.sh: warning: checksums.txt does not list $archive; skipping verification" >&2
	else
		if command -v sha256sum >/dev/null 2>&1; then
			actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
		elif command -v shasum >/dev/null 2>&1; then
			actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
		elif command -v openssl >/dev/null 2>&1; then
			actual="$(openssl dgst -sha256 "$tmp/$archive" | awk '{print $NF}')"
		else
			actual=""
			echo "install.sh: warning: no sha256 tool found; skipping verification" >&2
		fi
		if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
			die "checksum mismatch for $archive: expected $expected, got $actual"
		fi
	fi
else
	echo "install.sh: warning: no checksums.txt in $version; skipping verification" >&2
fi

# --- install ---------------------------------------------------------------

tar -xzf "$tmp/$archive" -C "$tmp" || die "cannot unpack $archive"
[ -f "$tmp/$BIN" ] || die "$archive did not contain $BIN"

mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
install -m 0755 "$tmp/$BIN" "$INSTALL_DIR/$BIN" || die "cannot write to $INSTALL_DIR"

echo "installed $INSTALL_DIR/$BIN"
"$INSTALL_DIR/$BIN" version

case ":${PATH}:" in
*":$INSTALL_DIR:"*) ;;
*)
	echo
	echo "$INSTALL_DIR is not on your PATH. Add it to your shell profile:"
	echo "  export PATH=\"\$PATH:$INSTALL_DIR\""
	;;
esac

echo
echo "next: $BIN doctor    # which agent CLIs this machine can serve"
