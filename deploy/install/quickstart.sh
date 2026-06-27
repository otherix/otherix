#!/bin/sh
# Otherix single-node quickstart. One command brings up a control plane, a
# local hypervisor agent, a default NAT network, and a demo VM you can SSH into:
#   curl -fsSL https://get.otherix.dev/quickstart.sh | sudo sh
# Env (all optional):
#   OTHERIX_VERSION         vX.Y.Z              (default: latest)
#   OTHERIX_REPO            owner/repo          (default: otherix/otherix)
#   OTHERIX_NETWORK_SUBNET  CIDR                (default: 10.88.0.0/24)
#   OTHERIX_VM_NAME         name                (default: demo)
#   OTHERIX_VM_USER         login user          (default: otherix)
#   OTHERIX_VM_IMAGE_URL    cloud image URL     (default: Ubuntu 24.04 server cloudimg for the host arch)
set -eu

VERSION="${OTHERIX_VERSION:-latest}"
REPO="${OTHERIX_REPO:-otherix/otherix}"
SUBNET="${OTHERIX_NETWORK_SUBNET:-10.88.0.0/24}"
NET_NAME="default"
BRIDGE_NAME="oxbr0"
VM_NAME="${OTHERIX_VM_NAME:-demo}"
VM_USER="${OTHERIX_VM_USER:-otherix}"
NODE_NAME="$(hostname -s 2>/dev/null || echo node-1)"
CLUSTER_NAME="local"
# The packaged api serves the user API over HTTPS on :8080 (server.tls.enabled
# defaults to true; the packaged config does not override it). The leaf cert is
# signed by the cluster CA written here on first boot.
API_URL="https://127.0.0.1:8080"
AGENT_CP_URL="https://127.0.0.1:8443"
CA_FILE="/var/lib/otherix/ca/cluster-ca.crt"

STEP=""
step() { STEP="$1"; echo; echo "==> $1"; }
die() { echo "quickstart: FAILED at step [${STEP}]: $*" >&2; exit 1; }
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
	# Capture the response first, then parse: piping curl straight into a
	# short-circuiting `grep -m1` makes grep close the pipe early and curl
	# abort the write with exit 23 (harmless, but it prints a scary error).
	_rel="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest")" \
		|| die "could not reach the GitHub releases API"
	printf '%s\n' "$_rel" | grep '"tag_name"' | head -n1 | cut -d'"' -f4
}

# running_api_version prints the version reported by an Otherix control plane
# already serving /healthz at $1, or nothing when none answers.
running_api_version() {
	# -k: this is a loopback liveness probe; the cluster CA may not yet be on
	# disk when we first poll, and we only need the version string.
	curl -fsSk "$1/healthz" 2>/dev/null \
		| sed -n 's/.*"version":"\([^"]*\)".*/\1/p'
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

# install_deb PKG - download+verify+install one daemon .deb from the release.
install_deb() {
	_pkg="$1"; _v="$2"; _a="$3"; _tmp="$4"
	_deb="${_pkg}_${_v#v}_${_a}.deb"
	dl "https://github.com/$REPO/releases/download/$_v/$_deb" "$_tmp/$_deb"
	verify_sha256 "$_tmp/$_deb" "$_deb" "$_tmp/SHA256SUMS"
	if have apt-get; then apt-get install -y "$_tmp/$_deb" >/dev/null
	else dpkg -i "$_tmp/$_deb" || die "dpkg install $_pkg failed (missing deps?)"; fi
}

# gen_secret - url-safe-ish random string for passwords.
gen_secret() { head -c 18 /dev/urandom | base64 | tr -d '/+=' | cut -c1-20; }

# api - run the CLI against the configured local cluster.
otx() { otherix --cluster "$CLUSTER_NAME" "$@"; }

default_image_url() {
	[ -n "${OTHERIX_VM_IMAGE_URL:-}" ] && { echo "$OTHERIX_VM_IMAGE_URL"; return; }
	echo "https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-$(arch).img"
}

# ---- 1. preflight -------------------------------------------------------
step "Preflight checks"
[ "$(id -u)" = "0" ] || die "run as root (sudo)"
[ "$(uname -s)" = "Linux" ] || die "the quickstart targets Linux"
have dpkg || die "needs a Debian/Ubuntu host (dpkg)"
[ -e /dev/kvm ] || die "/dev/kvm not found - this host has no KVM virtualization"
have curl || { have apt-get && apt-get install -y curl >/dev/null; } || die "curl is required"
have jq || { have apt-get && apt-get install -y jq >/dev/null; } || die "jq is required"
V="$(resolve_version)"; A="$(arch)"
[ -n "$V" ] || die "could not resolve a release version"

# Refuse to run against an Otherix control plane that is already serving on the
# API port. Nothing of ours is up yet at this point, so any responder is a
# foreign or stale control plane (an older release, or a dev binary) that the
# steps below would otherwise silently drive - the exact failure a dev machine
# with a leftover server hits. A prior run of THIS version is allowed to resume.
_running="$(running_api_version "$API_URL")"
if [ -n "$_running" ] && [ "$_running" != "${V#v}" ]; then
	die "an Otherix control plane (version ${_running}) is already serving on ${API_URL}, but this quickstart installs ${V#v}. Stop it (e.g. 'sudo systemctl stop otherix-api', a manual otherix-api, or your dev stack) or use a clean host, then re-run."
fi

# When no control plane of ours is up, every port this stack needs - including
# the embedded etcd client/peer ports - must be free. A leftover etcd, a manual
# otherix-api, or a dev stack holding one of these makes the api crash-loop on
# startup ("bind: address already in use") and the wait below time out opaquely;
# catch it here with a precise message instead. Skipped on a same-version resume
# (the running api legitimately holds these).
if [ -z "$_running" ]; then
	for _p in 8080 8443 9443 2379 2380; do
		if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${_p}\$"; then
			die "port :${_p} is already in use - another process (a leftover Otherix, its embedded etcd, or a dev stack) is holding it. Find it with 'sudo ss -ltnp | grep :${_p}', stop it, or use a clean host, then re-run."
		fi
	done
fi

TMP="$(mktemp -d)"
dl "https://github.com/$REPO/releases/download/$V/SHA256SUMS" "$TMP/SHA256SUMS"

# ---- 2. install api + agent + cli --------------------------------------
step "Installing otherix-api, otherix-agent, and the CLI ($V $A)"
install_deb otherix-api "$V" "$A" "$TMP"
install_deb otherix-agent "$V" "$A" "$TMP"
cli_name="otherix_${V#v}_linux_${A}.tar.gz"
dl "https://github.com/$REPO/releases/download/$V/$cli_name" "$TMP/$cli_name"
verify_sha256 "$TMP/$cli_name" "$cli_name" "$TMP/SHA256SUMS"
tar -xzf "$TMP/$cli_name" -C "$TMP"
install -m 0755 "$TMP/otherix" /usr/local/bin/otherix

ADMIN_USER="admin"
if [ ! -f /etc/otherix/admin.created ]; then
	ADMIN_PASS="$(gen_secret)"
	{
		echo "OTHERIX_BOOTSTRAP_ADMIN_USERNAME=$ADMIN_USER"
		echo "OTHERIX_BOOTSTRAP_ADMIN_PASSWORD=$ADMIN_PASS"
	} >> /etc/otherix/api.env
	touch /etc/otherix/admin.created
	systemctl restart otherix-api.service >/dev/null 2>&1 || true
else
	# Resume an earlier interrupted run rather than stranding a
	# half-provisioned host: recover the admin password the first run
	# generated (every step below is idempotent, so a re-run converges).
	ADMIN_PASS="$(awk -F= '/^OTHERIX_BOOTSTRAP_ADMIN_PASSWORD=/{print substr($0, index($0,"=")+1); exit}' /etc/otherix/api.env 2>/dev/null || true)"
	[ -n "$ADMIN_PASS" ] || die "host already bootstrapped but the admin password is no longer in /etc/otherix/api.env (removed post-login); cannot resume - drive the cluster with the otherix CLI directly"
fi

# ---- 3. wait for the control plane -------------------------------------
step "Waiting for the control plane"
i=0; until curl -fsSk "$API_URL/healthz" >/dev/null 2>&1; do
	i=$((i+1))
	if [ "$i" -gt 60 ]; then
		echo "--- last otherix-api logs ---" >&2
		journalctl -u otherix-api.service -n 20 --no-pager >&2 2>/dev/null || true
		die "control plane did not become healthy on $API_URL (see otherix-api logs above)"
	fi
	sleep 1
done

# ---- 4. configure the CLI cluster profile ------------------------------
step "Configuring the CLI cluster profile '$CLUSTER_NAME'"
# Feed the password via env, not argv (argv is visible via `ps`).
[ -r "$CA_FILE" ] || die "cluster CA not found at $CA_FILE (the control plane should have written it on first boot)"
OTHERIX_PASSWORD="$ADMIN_PASS" otherix config add cluster \
	--name "$CLUSTER_NAME" \
	--server "$API_URL" \
	--login "$ADMIN_USER" \
	--ca-file "$CA_FILE" \
	--force >/dev/null || die "config add cluster failed"

# ---- 5. bootstrap the local agent --------------------------------------
step "Bootstrapping the local hypervisor agent ($NODE_NAME)"
bundle="$(otx node join-token create --node-name "$NODE_NAME" --ttl 10m --output json)" \
	|| die "join-token mint failed"
token="$(echo "$bundle" | jq -r '.token')"
FP="$(echo "$bundle" | jq -r '.ca_fingerprint_sha256')"
[ -n "$token" ] && [ "$token" != "null" ] || die "join-token mint returned no token"
[ -n "$FP" ] && [ "$FP" != "null" ] || die "join-token bundle carried no CA fingerprint"
# Pass the token via env (not argv) so it never appears in the process table.
OTX_JOIN_TOKEN="$token" otherix-agent bootstrap \
	--token-env OTX_JOIN_TOKEN \
	--ca-fingerprint "sha256:$FP" \
	--cp-url "$AGENT_CP_URL" \
	--node-name "$NODE_NAME" \
	--advertised-endpoint "https://127.0.0.1:9443" \
	--migration-host "127.0.0.1" \
	--force >/dev/null || die "agent bootstrap failed"
systemctl restart otherix-agent.service >/dev/null 2>&1 || die "could not restart otherix-agent"
# Node status is a top-level scalar string ({"status":"ready"}), not a nested
# object - a '.status.phase' path errors out and never matches.
i=0; until [ "$(otx node get "$NODE_NAME" --output json 2>/dev/null | jq -r '.status // ""')" = "ready" ]; do
	i=$((i+1)); [ "$i" -gt 60 ] && die "node $NODE_NAME did not become ready"
	sleep 2
done

# ---- 6. default NAT + DHCP network -------------------------------------
step "Creating the default NAT network ($NET_NAME, $SUBNET)"
if ! otx network get "$NET_NAME" >/dev/null 2>&1; then
	otx network create "$NET_NAME" \
		--type bridge \
		--bridge-name "$BRIDGE_NAME" \
		--managed \
		--egress nat \
		--subnet "$SUBNET" \
		--dhcp >/dev/null || die "network create failed"
fi

# ---- 7. set the cluster default network --------------------------------
step "Setting '$NET_NAME' as the cluster default network"
otx cluster set-default-network "$NET_NAME" >/dev/null || die "set-default-network failed"

# ---- 8. create the demo VM ---------------------------------------------
step "Creating the demo VM ($VM_NAME)"
VM_PASS="$(gen_secret)"
KEYS=""
home="$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)"
[ -n "$home" ] || home="/root"
for k in "$home/.ssh/id_ed25519.pub" "$home/.ssh/id_rsa.pub"; do
	[ -r "$k" ] && KEYS="$KEYS$(printf '\n      - %s' "$(cat "$k")")"
done
if [ -n "$KEYS" ]; then
	SSH_BLOCK="$(printf '    ssh_authorized_keys:%s' "$KEYS")"
else
	SSH_BLOCK=""
fi
IMG="$(default_image_url)"
if ! otx vm get "$VM_NAME" >/dev/null 2>&1; then
	printf '#cloud-config\nusers:\n  - name: %s\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    groups: [sudo]\n    shell: /bin/bash\n    lock_passwd: false\n    plain_text_passwd: %s\n%s\nssh_pwauth: true\nchpasswd:\n  expire: false\n' \
		"$VM_USER" "$VM_PASS" "$SSH_BLOCK" \
		| otx vm create "$VM_NAME" --image-url "$IMG" --arch "$A" --user-data - >/dev/null \
		|| die "vm create failed"
fi

# ---- wait for running + an IP ------------------------------------------
step "Waiting for $VM_NAME to boot and lease an IP"
VM_IP=""
i=0; while :; do
	j="$(otx vm get "$VM_NAME" --output json 2>/dev/null || true)"
	phase="$(echo "$j" | jq -r '.status.phase // ""')"
	VM_IP="$(echo "$j" | jq -r '.nics[0].ipv4_address // ""')"
	[ "$phase" = "running" ] && [ -n "$VM_IP" ] && [ "$VM_IP" != "null" ] && break
	i=$((i+1)); [ "$i" -gt 120 ] && die "VM $VM_NAME did not reach running with an IP (last phase: ${phase:-unknown})"
	sleep 2
done

# ---- 9. summary --------------------------------------------------------
cat <<EOF

==========================================================
  Otherix single-node cluster is up, and '$VM_NAME' is running.

  SSH in (NAT network $SUBNET, reachable from this host):
    ssh $VM_USER@$VM_IP
EOF
[ -n "$KEYS" ] && echo "    (your SSH public key was installed)"
cat <<EOF
    password for $VM_USER: $VM_PASS

  Serial console (no network needed):
    otherix vm console $VM_NAME

  Control-plane admin:
    username: $ADMIN_USER
    password: $ADMIN_PASS
    (also stored in /etc/otherix/api.env)
  CLI config: /root/.otherix/config (cluster '$CLUSTER_NAME')

  Make your own VM (the default network is attached automatically):
    otherix vm create web-1 --image-url <url> --arch $A
==========================================================
EOF
