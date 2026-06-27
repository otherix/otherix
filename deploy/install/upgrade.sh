#!/bin/sh
# Otherix upgrade. Upgrades the components ALREADY installed on this host - the
# api server, the agent, and/or the CLI - to the latest release. It detects what
# is present and touches only that, so it is safe on a control-plane host, a
# hypervisor node, an operator workstation, or a single-node all-in-one. State
# is preserved (etcd data, certs, config); nothing is provisioned. Use
# quickstart.sh for the initial bring-up, not this.
#   curl -fsSL https://get.otherix.dev/upgrade.sh | sudo sh
# Env:
#   OTHERIX_VERSION  vX.Y.Z       (default: latest)
#   OTHERIX_REPO     owner/repo   (default: otherix/otherix)
set -eu

VERSION="${OTHERIX_VERSION:-latest}"
REPO="${OTHERIX_REPO:-otherix/otherix}"

step() { echo; echo "==> $1"; }
die() { echo "upgrade: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

[ "$(id -u)" = "0" ] || die "run as root (sudo)"
[ "$(uname -s)" = "Linux" ] || die "the Otherix packages/CLI are Linux-only"
have curl || die "curl is required"
have dpkg || die "needs a Debian/Ubuntu host (dpkg)"

arch() {
	case "$(uname -m)" in
		x86_64 | amd64) echo amd64 ;;
		aarch64 | arm64) echo arm64 ;;
		*) die "unsupported arch $(uname -m)" ;;
	esac
}

resolve_version() {
	[ "$VERSION" != "latest" ] && { echo "$VERSION"; return; }
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

verify_sha256() {
	_f="$1"; _name="$2"; _sums="$3"
	_want="$(awk -v n="$_name" '$2==n || $2=="*"n {print $1; exit}' "$_sums")"
	[ -n "$_want" ] || die "no SHA256SUMS entry for $_name (refusing to install unverified artifact)"
	_got="$(sha256_of "$_f")"
	[ "$_want" = "$_got" ] || die "checksum mismatch for $_name: want $_want, got $_got"
}

# upgrade_deb PKG - download+verify+install one daemon .deb. The package upgrade
# restarts the service via its postinst.
upgrade_deb() {
	_deb="${1}_${V#v}_${A}.deb"
	dl "https://github.com/$REPO/releases/download/$V/$_deb" "$TMP/$_deb"
	verify_sha256 "$TMP/$_deb" "$_deb" "$TMP/SHA256SUMS"
	if have apt-get; then apt-get install -y "$TMP/$_deb" >/dev/null
	else dpkg -i "$TMP/$_deb" || die "dpkg install $1 failed (missing deps?)"; fi
}

V="$(resolve_version)"; A="$(arch)"
[ -n "$V" ] || die "could not resolve a release version"
TMP="$(mktemp -d)"
dl "https://github.com/$REPO/releases/download/$V/SHA256SUMS" "$TMP/SHA256SUMS"

UPGRADED=""
if dpkg -s otherix-api >/dev/null 2>&1; then
	step "Upgrading otherix-api to $V"
	upgrade_deb otherix-api
	UPGRADED="$UPGRADED otherix-api"
fi
if dpkg -s otherix-agent >/dev/null 2>&1; then
	step "Upgrading otherix-agent to $V"
	upgrade_deb otherix-agent
	UPGRADED="$UPGRADED otherix-agent"
fi
if [ -x /usr/local/bin/otherix ]; then
	step "Upgrading the CLI to $V"
	_cli="otherix_${V#v}_linux_${A}.tar.gz"
	dl "https://github.com/$REPO/releases/download/$V/$_cli" "$TMP/$_cli"
	verify_sha256 "$TMP/$_cli" "$_cli" "$TMP/SHA256SUMS"
	tar -xzf "$TMP/$_cli" -C "$TMP"
	install -m 0755 "$TMP/otherix" /usr/local/bin/otherix
	UPGRADED="$UPGRADED cli"
fi

[ -n "$UPGRADED" ] || die "no Otherix components found on this host - install with quickstart.sh or install.sh first"

echo
echo "Upgraded to $V:$UPGRADED"
echo "Daemon services were restarted by their package upgrade; all state was preserved."
