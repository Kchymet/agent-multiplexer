#!/bin/sh
# amux bootstrap installer: installs the latest release.
#
#   curl -fsSL https://raw.githubusercontent.com/Kchymet/agent-multiplexer/master/scripts/install.sh | sh
#
# It downloads the release binary for this OS/arch, verifies it against the
# release's SHA256SUMS, atomically installs it to ~/.local/bin/amux (override
# with AMUX_BINDIR), and installs the shell shim to ~/.config/amux/amux.sh.
# After that, amux keeps itself current: the daemon installs new releases on
# its own (opt out with `amux config set update.auto off` or
# AMUX_NO_AUTO_UPDATE=1), and `amux update` installs one on demand.
set -eu

REPO_URL=${AMUX_UPDATE_REPO_URL:-https://github.com/Kchymet/agent-multiplexer}
REPO_URL=${REPO_URL%/}
BINDIR=${AMUX_BINDIR:-$HOME/.local/bin}
CONFDIR=${XDG_CONFIG_HOME:-$HOME/.config}/amux

say() { printf '%s\n' "$*"; }
die() { printf 'amux install: %s\n' "$*" >&2; exit 1; }

case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS $(uname -s) (amux supports macOS and Linux)" ;;
esac
case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m) (amux supports amd64 and arm64)" ;;
esac
asset=amux-$os-$arch

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "need sha256sum or shasum to verify the download"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/amux-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

# Resolve the latest version first, then fetch every asset from that exact
# tag, so a release published mid-install cannot mix assets.
fetch "$REPO_URL/releases/latest/download/amux-release.json" "$tmp/amux-release.json" ||
	die "no published release found at $REPO_URL"
version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([0-9][0-9.]*\)".*/\1/p' "$tmp/amux-release.json" | head -n 1)
[ -n "$version" ] || die "could not read the release version"
base="$REPO_URL/releases/download/v$version"

say "Installing amux $version ($os/$arch)…"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download SHA256SUMS failed"
fetch "$base/$asset" "$tmp/$asset" || die "download $asset failed"
fetch "$base/amux.sh" "$tmp/amux.sh" || die "download amux.sh failed"

verify() {
	want=$(awk -v n="$1" '$2 == n || $2 == "*" n { print $1 }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "$1 is not listed in SHA256SUMS"
	got=$(sha256 "$tmp/$1")
	[ "$got" = "$want" ] || die "$1 checksum mismatch (got $got, want $want)"
}
verify "$asset"
verify amux.sh
chmod 0755 "$tmp/$asset"
got_version=$("$tmp/$asset" version --short) || die "the downloaded binary does not run on this host"
[ "$got_version" = "$version" ] || die "downloaded binary reports $got_version, expected $version"

# Atomic install: stage beside the target, then rename over it. A running
# daemon keeps its open binary until it restarts.
mkdir -p "$BINDIR" "$CONFDIR"
cp "$tmp/$asset" "$BINDIR/.amux.install.$$"
chmod 0755 "$BINDIR/.amux.install.$$"
mv -f "$BINDIR/.amux.install.$$" "$BINDIR/amux"
cp "$tmp/amux.sh" "$CONFDIR/.amux.sh.install.$$"
mv -f "$CONFDIR/.amux.sh.install.$$" "$CONFDIR/amux.sh"

say ""
say "Installed amux $version -> $BINDIR/amux"
case ":$PATH:" in
	*":$BINDIR:"*) ;;
	*) say "Add $BINDIR to your PATH." ;;
esac
say "To open amux in every new terminal, add this to your ~/.zshrc (and/or ~/.bashrc):"
say ""
say "    [ -f \"$CONFDIR/amux.sh\" ] && . \"$CONFDIR/amux.sh\""
say ""
say "If an amux daemon is already running, load the new binary with: amux daemon restart"
say "amux updates itself from now on (amux update --check to see; amux config set update.auto off to opt out)."
