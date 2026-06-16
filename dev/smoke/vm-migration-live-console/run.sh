#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Seamless `vm console` across live migration - the real-agent gate for the
# CP-side console-follow reconnect loop. The CP console proxy used to resolve the
# owning node ONCE at WS-open; when the VM live-migrated, the source agent closed
# the serial mux at cutover and the operator's `vm console` session died (had to
# be re-opened). The fix makes the CP proxy FOLLOW the VM: on an upstream break it
# re-mints a fresh agent-local token against the new owner, re-dials it, and
# re-bridges the SAME operator WebSocket, replaying keystrokes buffered during the
# gap.
#
# What this smoke proves: a VM with an autologin root shell on its serial console;
# a single headless console session (the probe - the same console-token + proxy-WS
# data path `otherix vm console` uses, minus the raw-tty front end that needs a
# real terminal) types a PRE marker and sees it echo, the VM live-migrates
# (node-1 -> node-2), and a POST marker typed on the SAME WebSocket still echoes.
# Pre-fix the WS dies at cutover and the POST marker never returns -> FAIL.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree with the
# CP-side fix deployed:
#   make build && make local-dev-deploy
# Both node-1 and node-2 ready, the cluster default pool reconciled on both nodes.
#
# Usage: make smoke-vm-migration-live-console
#        (or: bash dev/smoke/vm-migration-live-console/run.sh)

set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$(cd "${SMOKE_DIR}/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"
VM="${VM_NAME:-consolefollow}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"
PHASE_WAIT="${PHASE_WAIT:-90}"
CONFIG="${OTHERIX_CONFIG:-${HOME}/.otherix/config}"
PRE_MARK="PRECONS_$$"
POST_MARK="POSTCONS_$$"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }
vm_node()  { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // ""' 2>/dev/null || true; }

assert_phase() {
  local name="$1" want="$2" to="${3:-$PHASE_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(vm_phase "$name")"
    [[ "$got" == "$want" ]] && { pass "$name phase=$want"; return 0; }
    sleep 2
  done
  fail "$name phase: want '$want' got '${got:-none}' after ${to}s"
}

assert_node() {
  local name="$1" want="$2" to="${3:-$PHASE_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(vm_node "$name")"
    [[ "$got" == "$want" ]] && { pass "$name node=$want"; return 0; }
    sleep 2
  done
  fail "$name node: want '$want' got '${got:-none}' after ${to}s"
}

pool_ready_both() {
  local n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx pool get default --output json 2>/dev/null \
        | jq -r --arg n "$n" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]] || return 1
  done
  return 0
}

# config_field <yaml-key> -> first value of an indented "<key>: <value>" line in
# the single-cluster dev config. The dev config has exactly one cluster, so the
# first match is the current one.
config_field() {
  grep -E "^[[:space:]]+$1:" "$CONFIG" 2>/dev/null | head -1 | awk '{print $2}'
}

READY="$(mktemp -t consolefollow-ready.XXXXXX)"; rm -f "$READY"
GO="$(mktemp -t consolefollow-go.XXXXXX)"; rm -f "$GO"
PROBE_OUT="$(mktemp -t consolefollow-probe.XXXXXX)"
PROBE_PID=""
cleanup() {
  echo "--- cleanup ---"
  [ -n "$PROBE_PID" ] && kill "$PROBE_PID" >/dev/null 2>&1 || true
  if [ -n "${SMOKE_KEEP:-}" ]; then
    info "SMOKE_KEEP set; leaving VM ${VM} in place (probe log at $PROBE_OUT)"
    return
  fi
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  rm -f "$READY" "$GO" "$PROBE_OUT" 2>/dev/null || true
}
trap cleanup EXIT

# --- the migrant cloud-config (autologin root shell on the serial console) ---
# Drops an autologin override on serial-getty@.service so an interactive root
# shell sits on the serial console - the same UART the agent wires to
# `-serial unix:` and proxies over the console WebSocket. With autologin, typing
# `echo <marker>` over the WS echoes the command and its output back, proving the
# operator->guest->operator round trip. %I makes the override arch-agnostic; the
# runcmd restarts whichever serial getty the kernel console spawned (ttyAMA0 on
# arm64, ttyS0 on amd64).
gen_userdata() {
  cat <<'EOF'
#cloud-config
users:
  - name: otherix
    plain_text_passwd: otherix
    lock_passwd: false
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [sudo]
    shell: /bin/bash
chpasswd:
  expire: false
ssh_pwauth: true
write_files:
  - path: /etc/systemd/system/serial-getty@.service.d/autologin.conf
    permissions: '0644'
    content: |
      [Service]
      ExecStart=
      ExecStart=-/sbin/agetty --autologin root --noclear --keep-baud 115200,57600,38400,9600 %I $TERM
runcmd:
  - systemctl daemon-reload
  - systemctl restart serial-getty@ttyAMA0.service || true
  - systemctl restart serial-getty@ttyS0.service || true
EOF
}

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-live-console smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"

[ -r "$CONFIG" ] || fail "CLI config not readable at '$CONFIG' (set OTHERIX_CONFIG)"
TOKEN="$(config_field token)"
CP_URL="$(config_field server)"
[ -n "$TOKEN" ] || fail "no API token in '$CONFIG'"
[ -n "$CP_URL" ] || CP_URL="http://localhost:8080"

for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

deadline=$(( SECONDS + 60 )); pool_ok=0
while (( SECONDS < deadline )); do pool_ready_both && { pool_ok=1; break; }; sleep 3; done
(( pool_ok == 1 )) || fail "default pool not ready on both nodes within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 + $NODE2 ready; default pool ready on both"

# --- step 1: the migrant VM on node-1 (autologin serial console) -------
echo "=== step 1: create $VM on $NODE1 (autologin serial console) -> running ==="
gen_userdata | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
[[ "$(vm_node "$VM")" == "$NODE1" ]] || fail "$VM did not land on $NODE1 (the migration source)"
pass "$VM created and running on $NODE1"

# --- step 2: open ONE console session, type the PRE marker -------------
echo "=== step 2: open a console session and type the PRE marker ($PRE_MARK) ==="
go run "${SMOKE_DIR}/probe" \
  --cp-url "$CP_URL" --token "$TOKEN" --vm "$VM" \
  --ready "$READY" --go "$GO" \
  --marker-pre "$PRE_MARK" --marker-post "$POST_MARK" \
  > "$PROBE_OUT" 2>&1 &
PROBE_PID=$!
info "console probe pid=$PROBE_PID -> $PROBE_OUT"

deadline=$(( SECONDS + 180 )); ready_ok=0
while (( SECONDS < deadline )); do
  [ -f "$READY" ] && { ready_ok=1; break; }
  kill -0 "$PROBE_PID" 2>/dev/null || { cat "$PROBE_OUT" >&2; fail "console probe exited before the PRE marker echoed"; }
  sleep 2
done
(( ready_ok == 1 )) || { cat "$PROBE_OUT" >&2; fail "PRE marker never echoed within 180s (guest console not interactive)"; }
pass "console interactive on $NODE1; PRE marker echoed"

# --- step 3: live-migrate 1 -> 2 with the console session open ---------
echo "=== step 3: live-migrate $VM $NODE1 -> $NODE2 (console session stays open) ==="
otx vm migrate "$VM" --node "$NODE2" \
  --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "vm migrate (live) $NODE1 -> $NODE2 did not complete within ${MIGRATE_WAIT}s"
assert_node "$VM" "$NODE2"
assert_phase "$VM" running
kill -0 "$PROBE_PID" 2>/dev/null || { cat "$PROBE_OUT" >&2; fail "console probe died during migration"; }
pass "migration complete; $VM running on $NODE2; console probe still alive"

# --- step 4: release the POST marker on the SAME session, assert echo --
echo "=== step 4: type the POST marker ($POST_MARK) on the SAME console WS ==="
: > "$GO"
if wait "$PROBE_PID"; then
  PROBE_PID=""
  pass "POST marker echoed on the SAME console session after cutover (console followed the VM)"
else
  rc=$?
  PROBE_PID=""
  echo "${RED}--- probe output ---${NC}" >&2
  cat "$PROBE_OUT" >&2
  fail "console did NOT follow the VM across live migration (probe exit $rc)"
fi

# --- step 5: teardown --------------------------------------------------
echo "=== step 5: teardown ==="
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
trap - EXIT
rm -f "$READY" "$GO" "$PROBE_OUT" 2>/dev/null || true
echo
echo "=== vm-migration-live-console smoke PASSED ==="
