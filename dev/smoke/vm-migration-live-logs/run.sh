#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Seamless `vm logs --follow` across live migration - the real-agent gate for the
# CP-side logs-follow reconnect loop. The CP log proxy used to resolve the owning
# node ONCE at stream-open; when the VM live-migrated, the source qemu was killed
# at cutover and the operator's `vm logs -f` stalled (had to be re-opened). The fix
# makes the CP proxy FOLLOW the VM: on an upstream break it re-resolves the current
# owner and re-dials it on the SAME client connection.
#
# What this smoke proves: a VM whose in-guest systemd unit writes a monotonic
# "SEQ <n>" counter to the serial console (ttyS0) once per second; a backgrounded
# `otherix vm logs --follow` keeps receiving SEQ lines ACROSS a live migration
# (node-1 -> node-2), the SAME follower process, with NO gap in the SEQ sequence.
# Pre-fix the follower stops growing at cutover (stream hung on the dead source) or
# exits; either makes this smoke FAIL at step 4. The SEQ writer lives in guest RAM,
# so it is NOT restarted by the migration - the counter is continuous across cutover.
#
# Intentionally simpler than the sibling live smokes: NO network (serial logs are
# NIC-independent; a no-network VM has no scheduler network constraint and migrates
# cleanly), no connectivity teeth. The single assertion is the followed, gapless SEQ
# stream.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree with the
# CP-side fix deployed:
#   make build && make local-dev-deploy
# Both node-1 and node-2 ready, the cluster default pool reconciled on both nodes.
#
# Usage: make smoke-vm-migration-live-logs
#        (or: bash dev/smoke/vm-migration-live-logs/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"
VM="${VM_NAME:-logsfollow}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"       # vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"     # live migrate cutover headroom
PHASE_WAIT="${PHASE_WAIT:-90}"          # CP-projected status.phase / node
SEQ_WAIT="${SEQ_WAIT:-120}"             # see first SEQ lines / post-migration growth
GROW_BY="${GROW_BY:-5}"                 # required new SEQ lines after cutover

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

# max_seq FILE -> highest integer among "SEQ <n>" lines (empty if none)
max_seq() { grep -oE 'SEQ [0-9]+' "$1" 2>/dev/null | awk '{print $2}' | sort -n | tail -1; }
# count_seq FILE -> number of SEQ lines
count_seq() { grep -cE 'SEQ [0-9]+' "$1" 2>/dev/null || true; }

# assert_no_seq_gap FILE -> fail if the SEQ integers skip; a single duplicate at the
# seam is tolerated (n==prev), a skip (n > prev+1) is a lost-lines FAIL.
assert_no_seq_gap() {
  grep -oE 'SEQ [0-9]+' "$1" | awk '{print $2}' | awk '
    { n=$1+0;
      if (seen && n!=prev && n!=prev+1) { printf("GAP: %d -> %d\n", prev, n); bad=1 }
      prev=n; seen=1 }
    END { if (bad) exit 1; if (!seen) { print "NO SEQ LINES"; exit 1 } }'
}

LOG_OUT="$(mktemp -t logsfollow.XXXXXX)"
LOGS_PID=""
cleanup() {
  echo "--- cleanup ---"
  [ -n "$LOGS_PID" ] && kill "$LOGS_PID" >/dev/null 2>&1 || true
  if [ -n "${SMOKE_KEEP:-}" ]; then
    info "SMOKE_KEEP set; leaving VM ${VM} in place (capture at $LOG_OUT)"
    return
  fi
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  rm -f "$LOG_OUT" 2>/dev/null || true
}
trap cleanup EXIT

# --- the migrant cloud-config (serial SEQ writer) ----------------------
# A systemd unit writes "SEQ <n>" to /dev/console once per second, n monotonically
# increasing. /dev/console is the active serial console on a cloud image (where the
# kernel/cloud-init boot output goes), which is exactly the UART the agent wires to
# `-serial unix:` and captures into serial.log - so it is arch-agnostic (amd64
# ttyS0 / arm64 ttyAMA0) where a hardcoded /dev/ttyS0 is not. The whole loop
# redirects to /dev/console once (opened once). The unit lives in guest RAM, so a
# live migration does NOT restart it - the counter stays continuous across cutover.
# Restart=always only covers a guest-internal crash.
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
  - path: /etc/systemd/system/seqwriter.service
    permissions: '0644'
    content: |
      [Unit]
      Description=serial SEQ writer for the logs-follow smoke
      After=multi-user.target
      [Service]
      ExecStart=/bin/sh -c 'i=0; while true; do echo "SEQ $i"; i=$((i+1)); sleep 1; done > /dev/console'
      Restart=always
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl daemon-reload
  - systemctl enable --now seqwriter.service
EOF
}

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-live-logs smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

deadline=$(( SECONDS + 60 )); pool_ok=0
while (( SECONDS < deadline )); do pool_ready_both && { pool_ok=1; break; }; sleep 3; done
(( pool_ok == 1 )) || fail "default pool not ready on both nodes within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 + $NODE2 ready; default pool ready on both"

# --- step 1: the migrant VM on node-1 (serial SEQ writer, NO network) --
echo "=== step 1: create $VM on $NODE1 (serial SEQ writer, no network) -> running ==="
gen_userdata | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
[[ "$(vm_node "$VM")" == "$NODE1" ]] || fail "$VM did not land on $NODE1 (the migration source)"
pass "$VM created and running on $NODE1"

# --- step 2: background `vm logs --follow` -----------------------------
echo "=== step 2: start 'vm logs --follow $VM' in the background ==="
( "$OTX" vm logs --follow "$VM" > "$LOG_OUT" 2>/dev/null ) &
LOGS_PID=$!
info "logs follower pid=$LOGS_PID -> $LOG_OUT"

deadline=$(( SECONDS + SEQ_WAIT )); seen_ok=0
while (( SECONDS < deadline )); do
  [[ "$(count_seq "$LOG_OUT")" -ge 3 ]] && { seen_ok=1; break; }
  kill -0 "$LOGS_PID" 2>/dev/null || fail "logs follower exited before migration (no stream)"
  sleep 2
done
(( seen_ok == 1 )) || fail "no SEQ lines from $VM within ${SEQ_WAIT}s (serial writer or logs stream broken)"
PRE_MAX="$(max_seq "$LOG_OUT")"
pass "logs streaming on $NODE1; pre-migration max SEQ=$PRE_MAX"

# --- step 3: live-migrate 1 -> 2 while logs follow ---------------------
echo "=== step 3: live-migrate $VM $NODE1 -> $NODE2 (logs must keep following) ==="
otx vm migrate "$VM" --node "$NODE2" \
  --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "vm migrate (live) $NODE1 -> $NODE2 did not complete within ${MIGRATE_WAIT}s"
assert_node "$VM" "$NODE2"
assert_phase "$VM" running

# --- step 4: the stream FOLLOWED - same follower, capture grows past PRE_MAX ---
echo "=== step 4: assert the logs stream followed across cutover ==="
kill -0 "$LOGS_PID" 2>/dev/null || fail "logs follower exited at/after cutover (stream did not follow the VM)"
want=$(( PRE_MAX + GROW_BY ))
deadline=$(( SECONDS + SEQ_WAIT )); grow_ok=0
while (( SECONDS < deadline )); do
  cur="$(max_seq "$LOG_OUT")"
  [[ -n "$cur" && "$cur" -ge "$want" ]] && { grow_ok=1; break; }
  kill -0 "$LOGS_PID" 2>/dev/null || fail "logs follower exited after cutover"
  sleep 2
done
(( grow_ok == 1 )) || fail "logs stream did NOT follow across migration: max SEQ stuck at '$(max_seq "$LOG_OUT")' (< $want) after ${SEQ_WAIT}s - the stream hung on the dead source"
pass "logs followed across cutover (max SEQ $(max_seq "$LOG_OUT") >= $want, same follower pid=$LOGS_PID)"

# --- step 5: the SEQ sequence is continuous across the cutover window ---
echo "=== step 5: assert no SEQ gap across the cutover window ==="
if ! assert_no_seq_gap "$LOG_OUT"; then
  echo "${RED}--- captured SEQ tail ---${NC}" >&2
  grep -oE 'SEQ [0-9]+' "$LOG_OUT" | tail -20 >&2
  fail "SEQ sequence has a gap across the migration (lost log lines)"
fi
pass "SEQ sequence continuous across cutover (no lost lines)"

# --- step 6: teardown --------------------------------------------------
echo "=== step 6: teardown ==="
kill "$LOGS_PID" >/dev/null 2>&1 || true
LOGS_PID=""
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
pass "VM deleted; logs follower stopped"

trap - EXIT
rm -f "$LOG_OUT" 2>/dev/null || true
echo
echo "=== vm-migration-live-logs smoke PASSED ==="
