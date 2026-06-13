#!/bin/sh
# Otherix installer. Usage:
#   curl -fsSL https://raw.githubusercontent.com/otherix/otherix/main/deploy/install/install.sh | OTHERIX_COMPONENT=api sh
# Env:
#   OTHERIX_COMPONENT  api | agent | cli   (default: api)
#   OTHERIX_VERSION    vX.Y.Z              (default: latest)
#   OTHERIX_REPO       owner/repo          (default: otherix/otherix)
set -eu

COMPONENT="${OTHERIX_COMPONENT:-api}"
VERSION="${OTHERIX_VERSION:-latest}"
REPO="${OTHERIX_REPO:-otherix/otherix}"

die() { echo "otherix-install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

arch() {
	case "$(uname -m)" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		*) die "unsupported arch $(uname -m)" ;;
	esac
}

resolve_version() {
	[ "$VERSION" != "latest" ] && { echo "$VERSION"; return; }
	have curl || die "curl is required"
	curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
		| grep -m1 '"tag_name"' | cut -d'"' -f4
}

dl() { curl -fsSL "$1" -o "$2" || die "download failed: $1"; }

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
	else die "need sha256sum or shasum to verify the download"; fi
}

# verify_sha256 FILE BASENAME SUMS - abort unless FILE's sha256 matches the
# SHA256SUMS entry for BASENAME. Fail-closed: a missing manifest entry or a
# mismatch aborts the install before any privileged step.
verify_sha256() {
	_f="$1"; _name="$2"; _sums="$3"
	_want="$(awk -v n="$_name" '$2==n || $2=="*"n {print $1; exit}' "$_sums")"
	[ -n "$_want" ] || die "no SHA256SUMS entry for $_name (refusing to install unverified artifact)"
	_got="$(sha256_of "$_f")"
	[ "$_want" = "$_got" ] || die "checksum mismatch for $_name: want $_want, got $_got (refusing to install)"
	echo "verified $_name ($_got)"
}

install_cli() {
	if [ "$(uname -s)" = "Darwin" ]; then
		echo "On macOS install the CLI via Homebrew:"
		echo "  brew install otherix/tap/otherix"
		exit 0
	fi
	v="$(resolve_version)"; a="$(arch)"
	tmp="$(mktemp -d)"
	cli_name="otherix_${v#v}_linux_${a}.tar.gz"
	dl "https://github.com/$REPO/releases/download/$v/$cli_name" "$tmp/$cli_name"
	dl "https://github.com/$REPO/releases/download/$v/SHA256SUMS" "$tmp/SHA256SUMS"
	verify_sha256 "$tmp/$cli_name" "$cli_name" "$tmp/SHA256SUMS"
	tar -xzf "$tmp/$cli_name" -C "$tmp"
	install -m 0755 "$tmp/otherix" /usr/local/bin/otherix
	echo "Installed otherix CLI to /usr/local/bin/otherix"
}

install_daemon() {
	pkg="otherix-$COMPONENT"
	[ "$(uname -s)" = "Linux" ] || die "$COMPONENT can only be installed on Linux"
	have dpkg || die "$COMPONENT install needs a Debian/Ubuntu host (dpkg)"
	[ "$(id -u)" = "0" ] || die "run as root (sudo) to install $pkg"
	v="$(resolve_version)"; a="$(arch)"
	tmp="$(mktemp -d)"
	deb_name="${pkg}_${v#v}_${a}.deb"
	deb="$tmp/$deb_name"
	dl "https://github.com/$REPO/releases/download/$v/$deb_name" "$deb"
	dl "https://github.com/$REPO/releases/download/$v/SHA256SUMS" "$tmp/SHA256SUMS"
	verify_sha256 "$deb" "$deb_name" "$tmp/SHA256SUMS"
	if have apt-get; then
		apt-get install -y "$deb"
	else
		dpkg -i "$deb" || die "dpkg install failed (missing deps?)"
	fi

	if [ "$COMPONENT" = "api" ] && [ ! -f /etc/otherix/admin.created ]; then
		email="admin@otherix.local"
		pass="$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | cut -c1-20)"
		{
			echo "OTHERIX_BOOTSTRAP_ADMIN_EMAIL=$email"
			echo "OTHERIX_BOOTSTRAP_ADMIN_PASSWORD=$pass"
		} >> /etc/otherix/api.env
		touch /etc/otherix/admin.created
		systemctl restart otherix-api.service >/dev/null 2>&1 || true
		echo
		echo "================ Otherix admin created ================"
		echo "  email:    $email"
		echo "  password: $pass"
		echo "  (printed once; stored in /etc/otherix/api.env)"
		echo "  After first login, remove OTHERIX_BOOTSTRAP_ADMIN_EMAIL and"
		echo "  OTHERIX_BOOTSTRAP_ADMIN_PASSWORD from /etc/otherix/api.env."
		echo "======================================================="
	fi
	echo "Installed $pkg $v"
}

case "$COMPONENT" in
	cli) install_cli ;;
	api|agent) install_daemon ;;
	*) die "OTHERIX_COMPONENT must be api, agent, or cli" ;;
esac
