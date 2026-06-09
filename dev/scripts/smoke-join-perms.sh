#!/usr/bin/env bash
# smoke-join-perms.sh - verify `otherix-api join` leaves the token file readable
# by the daemon user (Sec-C1).
#
# `sudo otherix-api join` runs as root but the daemon runs as the unprivileged
# `otherix` user and reads cluster_join.token_path at boot. A root:root 0600
# token would be unreadable by the daemon -> the join silently fails on a real
# packaged install. The loopback HA smoke runs everything under one uid and
# cannot catch this, so this check needs a real multi-uid Linux host.
#
# This is an OPERATOR-RUN smoke (must run on Linux as root, e.g. inside a Lima
# VM). It does NOT need a running control plane: it exercises only the local
# config + token write + chown path via `otherix-api join --no-restart`.
#
# Usage (inside a Linux VM, as root):
#   BIN=/path/to/otherix-api bash dev/scripts/smoke-join-perms.sh
set -euo pipefail

BIN="${BIN:-otherix-api}"
DAEMON_USER="${DAEMON_USER:-otherix}"
STATE_DIR=/var/lib/otherix
ETC_DIR=/etc/otherix
TOKEN_FILE="$STATE_DIR/cluster-join-token"
CONFIG="$ETC_DIR/api.yaml"
FP="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

log() { printf '>> %s\n' "$*"; }
fail() { printf '!! FAIL: %s\n' "$*" >&2; exit 1; }
ok() { printf '   OK: %s\n' "$*"; }

[ "$(id -u)" = "0" ] || fail "must run as root (use sudo)"
command -v "$BIN" >/dev/null 2>&1 || [ -x "$BIN" ] || fail "otherix-api binary not found at '$BIN' (set BIN=...)"

log "provision daemon user + state dir (mirrors api-postinstall.sh)"
if ! getent passwd "$DAEMON_USER" >/dev/null 2>&1; then
	useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$DAEMON_USER"
fi
mkdir -p "$STATE_DIR" "$ETC_DIR"
chown -R "$DAEMON_USER:$DAEMON_USER" "$STATE_DIR"
chmod 0750 "$STATE_DIR"
rm -f "$TOKEN_FILE"

log "seed a minimal single-mode api.yaml"
cat >"$CONFIG" <<'YAML'
server:
  listen: "0.0.0.0:8080"
etcd:
  mode: "single"
  name: "otherix-0"
  peer_url: "auto"
YAML

log "run 'otherix-api join --no-restart' as root"
"$BIN" join \
	--cp-url "https://127.0.0.1:8443" \
	--token "smoke-test-token" \
	--ca-fingerprint "sha256:$FP" \
	--name "otherix-1" \
	--config "$CONFIG" \
	--token-dest "$TOKEN_FILE" \
	--no-restart

log "assert: token written"
[ -f "$TOKEN_FILE" ] || fail "token file not created at $TOKEN_FILE"
ok "token file exists"

log "assert: token owned by the daemon user (Sec-C1 chown)"
owner="$(stat -c '%U' "$TOKEN_FILE")"
[ "$owner" = "$DAEMON_USER" ] || fail "token owned by '$owner', want '$DAEMON_USER' (root-owned token is unreadable by the daemon)"
ok "token owned by $DAEMON_USER"

log "assert: token mode is 0600 (not world-readable - it redeems the CA key)"
mode="$(stat -c '%a' "$TOKEN_FILE")"
[ "$mode" = "600" ] || fail "token mode is $mode, want 600"
ok "token mode 0600"

log "assert: the daemon user can actually READ the token (the real seam)"
sudo -u "$DAEMON_USER" cat "$TOKEN_FILE" >/dev/null 2>&1 || fail "daemon user '$DAEMON_USER' cannot read the token - join would fail at boot"
ok "daemon user can read the token"

log "assert: api.yaml flipped to mode=join"
grep -q 'mode: *"\{0,1\}join' "$CONFIG" || fail "api.yaml not rewritten to mode=join"
ok "api.yaml mode=join"

printf '\nPASS: otherix-api join leaves the token readable by the daemon user (Sec-C1)\n'
