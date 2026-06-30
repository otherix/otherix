#!/bin/sh
# Otherix external SSH connector installer. Usage:
#   curl -fsSL https://get.otherix.dev/ssh | sh
# Installs the `otherix-ssh` connector an external person uses to reach a
# granted VM. No Otherix account is needed: an operator runs
# `otherix ssh-grant create` and sends back a bundle, you run
# `otherix-ssh add <bundle>` once, then `ssh <vm>.<suffix>`.
# Env (all optional):
#   OTHERIX_VERSION  vX.Y.Z       (default: latest)
#   OTHERIX_REPO     owner/repo   (default: otherix/otherix)
#   OTHERIX_BINDIR   install dir  (default: /usr/local/bin if writable, else ~/.local/bin)
set -eu

VERSION="${OTHERIX_VERSION:-latest}"
REPO="${OTHERIX_REPO:-otherix/otherix}"
BIN="otherix-ssh"

die() { echo "otherix-ssh-install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

os() {
	case "$(uname -s)" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		*) die "unsupported OS $(uname -s) (use Homebrew: brew install otherix/tap/otherix-ssh)" ;;
	esac
}

arch() {
	case "$(uname -m)" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		*) die "unsupported arch $(uname -m)" ;;
	esac
}

resolve_version() {
	[ "$VERSION" != "latest" ] && { echo "$VERSION"; return; }
	# Capture the response first, then parse: piping curl straight into a
	# short-circuiting `grep -m1` makes grep close the pipe early and curl
	# abort the write with exit 23 (harmless, but it prints a scary error).
	_rel="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest")" \
		|| die "could not reach the GitHub releases API"
	printf '%s\n' "$_rel" | grep '"tag_name"' | head -n1 | cut -d'"' -f4
}

dl() { curl -fsSL "$1" -o "$2" || die "download failed: $1"; }

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
	else die "need sha256sum or shasum to verify the download"; fi
}

# verify_sha256 FILE BASENAME SUMS - abort unless FILE's sha256 matches the
# SHA256SUMS entry for BASENAME. Fail-closed: a missing manifest entry or a
# mismatch aborts the install before the binary is placed on PATH.
verify_sha256() {
	_f="$1"; _name="$2"; _sums="$3"
	_want="$(awk -v n="$_name" '$2==n || $2=="*"n {print $1; exit}' "$_sums")"
	[ -n "$_want" ] || die "no SHA256SUMS entry for $_name (refusing to install unverified artifact)"
	_got="$(sha256_of "$_f")"
	[ "$_want" = "$_got" ] || die "checksum mismatch for $_name: want $_want, got $_got (refusing to install)"
	echo "verified $_name ($_got)"
}

# install_dir picks where to drop the binary. otherix-ssh is a user-scoped
# tool, so it never needs root: use OTHERIX_BINDIR when set, else
# /usr/local/bin when it is writable (e.g. Homebrew-style installs), else fall
# back to ~/.local/bin so no sudo is required.
install_dir() {
	if [ -n "${OTHERIX_BINDIR:-}" ]; then echo "$OTHERIX_BINDIR"; return; fi
	if [ -w /usr/local/bin ] 2>/dev/null; then echo /usr/local/bin; return; fi
	echo "$HOME/.local/bin"
}

have curl || die "curl is required"

V="$(resolve_version)"; O="$(os)"; A="$(arch)"
[ -n "$V" ] || die "could not resolve a release version"

TMP="$(mktemp -d)"
ASSET="${BIN}_${V#v}_${O}_${A}.tar.gz"
dl "https://github.com/$REPO/releases/download/$V/$ASSET" "$TMP/$ASSET"
dl "https://github.com/$REPO/releases/download/$V/SHA256SUMS" "$TMP/SHA256SUMS"
verify_sha256 "$TMP/$ASSET" "$ASSET" "$TMP/SHA256SUMS"
tar -xzf "$TMP/$ASSET" -C "$TMP"
[ -f "$TMP/$BIN" ] || die "release archive $ASSET did not contain $BIN"

DEST="$(install_dir)"
mkdir -p "$DEST" || die "cannot create install dir $DEST"
install -m 0755 "$TMP/$BIN" "$DEST/$BIN" 2>/dev/null \
	|| die "cannot write $DEST/$BIN (set OTHERIX_BINDIR to a writable directory)"

echo "Installed $BIN $V to $DEST/$BIN"
case ":$PATH:" in
	*":$DEST:"*) ;;
	*) echo "note: $DEST is not on your PATH; add it, e.g. 'export PATH=\"$DEST:\$PATH\"'" ;;
esac
echo
echo "Next: import the bundle your operator sent you, then ssh in:"
echo "  $BIN add <bundle-file|->"
echo "  ssh <vm>.<suffix>"
