#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Live VM migration smoke - drives `otherix vm migrate` (LIVE, the default, no
# --offline) across the two-node dev stack through the `otherix` CLI as a real
# operator. The sibling vm-migration smoke proves the OFFLINE stop+copy+start
# cutover; this one proves the LIVE cutover keeps the GUEST PROCESS RUNNING: the
# qemu instance is migrated A->B without the guest rebooting.
#
# What it proves, end to end, on ONE running VM:
#   create on the source node    -> running (qemu spawned, guest boots, the guest
#                                    starts an every-second console heartbeat)
#   migrate (live) to the target -> the VM's current node changes to target, the
#                                    phase is running on the target, and - the
#                                    teeth - the in-guest heartbeat counter
#                                    CONTINUES on the target instead of resetting
#                                    to 0 (a reset would mean a reboot, i.e. NOT a
#                                    live cutover)
#   the migration record         -> reaches phase 'completed', live=true
#   delete                       -> gone
#
# THE HEARTBEAT (how we prove "did not reboot"):
#   cloud-init starts a oneshot loop that writes "OTHERIX_HEARTBEAT <n> <epoch>"
#   to the serial console once a second, with a monotonically incrementing <n>.
#   The agent persists the serial console to <state_root>/vms/<id>/serial.log
#   (append-only). The VM id is STABLE across migration, so the post-cutover
#   serial.log lives on the TARGET node under the same id. We read:
#     N_a = max <n> seen on the SOURCE serial.log just before migrate
#     N_b = min <n> seen on the TARGET serial.log after the guest resumes there
#   and assert N_b >= N_a. A live cutover continues the SAME process, so the
#   counter keeps climbing (N_b ~ N_a, never resets). A reboot would restart the
#   loop from i=0, so N_b would be small (near 0) -> the assertion fails loudly.
#   We also assert no fresh getty " login:" banner appears in the TARGET stream
#   in the post-cutover window (a second reboot tell).
#
#   The counter starts at 0 on boot and ticks every second, so on a slow TCG
#   stack N_a easily reaches the tens/hundreds before migrate - plenty of signal.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# both node-1 and node-2 ready with the cluster default pool reconciled.
#
# Usage: make smoke-vm-migration-live   (or: bash dev/smoke/vm-migration-live/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # source node (create is pinned here)
VM="${VM_NAME:-migsmokelive}"                 # the single migration VM; delete-firsted
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"           # seconds for the live migrate cutover (disk copy on TCG is slow)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a full guest boot (TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
MIG_PHASE_WAIT="${MIG_PHASE_WAIT:-120}"       # seconds to wait for the migration record -> completed
HEARTBEAT_WAIT="${HEARTBEAT_WAIT:-720}"       # seconds to wait for the guest heartbeat to appear on a node

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME -> prints the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# vm_node NAME -> prints the VM's current node name ("" if unscheduled/gone). The
# CP keeps .node = current_node_id and only updates it after a migration cutover,
# so this is the node-change signal.
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // ""' 2>/dev/null || true; }

# assert_phase NAME WANT [TIMEOUT] -> poll status.phase until it equals WANT
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

# assert_node NAME WANT [TIMEOUT] -> poll the VM's current node until it equals WANT
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

# assert_gone NAME -> poll until `vm get` no longer returns the VM (404)
assert_gone() {
  local name="$1" deadline; deadline=$(( SECONDS + PHASE_WAIT ))
  while (( SECONDS < deadline )); do
    otx vm get "$name" --output json >/dev/null 2>&1 || { pass "$name gone (404)"; return 0; }
    sleep 2
  done
  fail "$name still visible after delete within ${PHASE_WAIT}s"
}

# heartbeat_max HANDLE STATE -> the maximum heartbeat counter <n> seen in the
# VM's serial.log on the node identified by HANDLE/STATE, or empty when no
# heartbeat line is present yet. Parses "OTHERIX_HEARTBEAT <n> <epoch>" lines and
# takes the largest <n>. Append-only log -> this is monotonic on a single node.
heartbeat_max() {
  local handle="$1" state="$2" out
  out="$(run_on "$handle" sudo grep -oE 'OTHERIX_HEARTBEAT [0-9]+' \
      "${state}/vms/${VMID}/serial.log" 2>/dev/null)" || true
  [[ -n "$out" ]] || { printf ''; return 0; }
  printf '%s\n' "$out" | awk '{print $2}' | sort -n | tail -1
}

# heartbeat_min HANDLE STATE SINCE -> the minimum heartbeat counter <n> in the
# VM's serial.log on HANDLE/STATE considering only lines with <n> >= SINCE-guard
# is not applied here; callers pass the raw min. Empty when no line is present.
heartbeat_min() {
  local handle="$1" state="$2" out
  out="$(run_on "$handle" sudo grep -oE 'OTHERIX_HEARTBEAT [0-9]+' \
      "${state}/vms/${VMID}/serial.log" 2>/dev/null)" || true
  [[ -n "$out" ]] || { printf ''; return 0; }
  printf '%s\n' "$out" | awk '{print $2}' | sort -n | head -1
}

# login_banner_count HANDLE STATE -> number of getty " login:" prompts in the
# VM's serial.log on that node (0 when the file is absent). A live cutover never
# adds one on the target; a reboot would. Used as a secondary reboot tell.
login_banner_count() {
  local handle="$1" state="$2" n
  n="$(run_on "$handle" sudo grep -cE ' login:' "${state}/vms/${VMID}/serial.log" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_heartbeat HANDLE STATE -> block until at least one heartbeat line appears
# in the VM's serial.log on that node, then print the max counter seen.
wait_heartbeat() {
  local handle="$1" state="$2" deadline mx; deadline=$(( SECONDS + HEARTBEAT_WAIT ))
  info "waiting for the in-guest heartbeat on $handle (<= ${HEARTBEAT_WAIT}s)"
  while (( SECONDS < deadline )); do
    mx="$(heartbeat_max "$handle" "$state")"
    [[ -n "$mx" ]] && { printf '%s' "$mx"; return 0; }
    sleep 5
  done
  echo "--- $VM serial tail ($handle) ---" >&2
  run_on "$handle" sudo tail -40 "${state}/vms/${VMID}/serial.log" 2>/dev/null >&2 || true
  fail "no in-guest heartbeat appeared on $handle within ${HEARTBEAT_WAIT}s"
}

cleanup() {
  echo "--- cleanup ---"
  if [ -n "${SMOKE_KEEP:-}" ]; then
    info "SMOKE_KEEP set; leaving VM ${VM} (id=${VMID:-?}) in place for inspection"
    return
  fi
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- the heartbeat cloud-config (every-second serial heartbeat) --------
# A first-boot runcmd writes a heartbeat script and launches it DETACHED (setsid
# + nohup) so it survives cloud-init exiting. The loop writes an incrementing
# "OTHERIX_HEARTBEAT <n> <epoch>" line to the arch-correct serial device (ttyS0
# on amd64, ttyAMA0 on arm64) once a second; the agent captures that console into
# serial.log. Crucially the loop is ONE long-lived process: a LIVE migration
# preserves it (and its monotonic counter) across the move, so the counter
# CONTINUES on the target - the continuity this smoke keys on. A stop/start
# (non-live) reboots the guest; cloud-init runcmd does not re-fire on the same
# instance, so NO heartbeat appears on the target - also a clear failure signal.
# Quoted heredoc -> the shell vars stay literal for the guest.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /usr/local/bin/otherix-heartbeat.sh
    permissions: '0755'
    content: |
      #!/bin/sh
      SC=/dev/ttyS0
      [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0
      i=0
      while :; do
        echo "OTHERIX_HEARTBEAT $i $(date -u +%s)" > "$SC"
        i=$((i+1))
        sleep 1
      done
runcmd:
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-heartbeat.sh >/dev/null 2>&1 < /dev/null &" ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-live smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

# Both the source (node-1) and at least one other ready node must exist; the
# default pool must be reconciled on both for create + migrate to bind.
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# Discover the migration TARGET: a ready node that is NOT the source. We read the
# ready-node set from the CP rather than hard-coding node-2 so the smoke follows
# whatever the stack named the second node.
TARGET="$(otx node list --status ready --output json 2>/dev/null \
  | jq -r --arg src "$NODE1" '[.data[]? | select(.name != $src) | .name] | first // ""')"
[[ -n "$TARGET" ]] || fail "no second ready node found besides $NODE1 (run make local-dev-start for a two-node stack)"
info "source=$NODE1 target=$TARGET"

# default pool reconciled on BOTH the source and the target.
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
for n in "$NODE1" "$TARGET"; do
  deadline=$(( SECONDS + 60 )); ok=0
  while (( SECONDS < deadline )); do pool_ready "$n" && { ok=1; break; }; sleep 3; done
  (( ok == 1 )) || fail "default pool not ready on $n within 60s (CP auto-provision)"
done
pass "CP up (${CP_VERSION}); $NODE1 + $TARGET ready; default pool ready on both"

# Resolve the source + target node exec handles + state roots for the serial
# probe. The dev stack maps node-1 -> handle/state index 1 and node-2 -> index 2;
# create is pinned to node-1, so the discovered second ready node is always
# index 2.
SOURCE_HANDLE="$SMOKE_HANDLE_1"; SOURCE_STATE="$SMOKE_STATE_1"
TARGET_HANDLE="$SMOKE_HANDLE_2"; TARGET_STATE="$SMOKE_STATE_2"

# --- step 1: create on the source -> running ---------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of a stale leftover
printf '%s' "$CLOUD_INIT" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --disk-gib 10 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
info "VM id=$VMID"

# Confirm the VM landed on the source node we pinned (this is the migration's
# from-node; the live cutover needs a different to-node).
SOURCE="$(vm_node "$VM")"
[[ "$SOURCE" == "$NODE1" ]] || fail "VM landed on '$SOURCE', expected source $NODE1"
[[ "$SOURCE" != "$TARGET" ]] || fail "source and target resolved to the same node ($SOURCE)"
pass "created and running on source $SOURCE"

# --- step 2: the guest heartbeat is alive on the source ----------------
echo "=== step 2: in-guest heartbeat alive on $SOURCE ==="
# Wait for the guest to boot and the heartbeat loop to start writing to the
# console. Then read the max counter just before the migrate - this is N_a, the
# value the live cutover must CONTINUE from on the target.
wait_heartbeat "$SOURCE_HANDLE" "$SOURCE_STATE" >/dev/null
N_A="$(heartbeat_max "$SOURCE_HANDLE" "$SOURCE_STATE")"
[[ "$N_A" =~ ^[0-9]+$ ]] || fail "could not read source heartbeat counter (got '${N_A:-none}')"
info "source heartbeat max N_a=$N_A"
pass "in-guest heartbeat alive on source $SOURCE (N_a=$N_A)"

# --- step 3: live migrate to the target --------------------------------
echo "=== step 3: migrate $VM (live) $SOURCE -> $TARGET ==="
# No --offline: live is the default (the CLI sends Live=true). A live cutover
# migrates the RUNNING qemu process to the target without a guest reboot.
otx vm migrate "$VM" --node "$TARGET" \
  --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "vm migrate (live) did not complete within ${MIGRATE_WAIT}s"

# After the cutover the CP repoints .node to the target and the VM phase is
# running there. The migrate --wait already blocked on the task, but the
# CP-projected node/phase can lag the task terminal slightly, so poll.
assert_node "$VM" "$TARGET"
assert_phase "$VM" running
pass "VM migrated: current node changed to $TARGET, phase running"

# --- step 4: the migration record reached completed (live=true) --------
echo "=== step 4: migration record -> completed (live) ==="
# `migration list --vm` filters by VM UUID, newest-first; the first row is this
# migration. Then `migration get <id>` surfaces its terminal phase + live flag.
MIGID="$(otx migration list --vm "$VMID" --output json 2>/dev/null \
  | jq -r '.data[0].id // ""')"
[[ "$MIGID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve migration id for VM $VMID (got '${MIGID:-none}')"
info "migration id=$MIGID"
deadline=$(( SECONDS + MIG_PHASE_WAIT )); mphase=""
while (( SECONDS < deadline )); do
  mphase="$(otx migration get "$MIGID" --output json 2>/dev/null | jq -r '.phase // ""' || true)"
  [[ "$mphase" == "completed" ]] && break
  case "$mphase" in failed|cancelled) fail "migration $MIGID reached terminal '$mphase', expected completed" ;; esac
  sleep 2
done
[[ "$mphase" == "completed" ]] || fail "migration $MIGID phase: want 'completed' got '${mphase:-none}' after ${MIG_PHASE_WAIT}s"
mlive="$(otx migration get "$MIGID" --output json 2>/dev/null | jq -r '.live // empty' || true)"
[[ "$mlive" == "true" ]] || fail "migration $MIGID live: want 'true' got '${mlive:-none}' (this is the LIVE smoke)"
pass "migration $MIGID phase=completed live=true"

# --- step 5: THE TEETH - heartbeat continuity across the cutover -------
echo "=== step 5: heartbeat continuity $SOURCE -> $TARGET (no reboot) ==="
# Wait for the heartbeat to resume on the TARGET serial.log, then assert the
# counter CONTINUED instead of resetting. N_b is the min counter the target ever
# logged: on a live cutover the SAME loop keeps ticking, so the target's first
# logged value is ~N_a (>= N_a). A reboot restarts the loop at 0, so N_b would be
# near 0 -> N_b < N_a -> fail loudly.
wait_heartbeat "$TARGET_HANDLE" "$TARGET_STATE" >/dev/null
N_B="$(heartbeat_min "$TARGET_HANDLE" "$TARGET_STATE")"
N_B_MAX="$(heartbeat_max "$TARGET_HANDLE" "$TARGET_STATE")"
[[ "$N_B" =~ ^[0-9]+$ ]] || fail "could not read target heartbeat counter (got '${N_B:-none}')"
info "target heartbeat: N_b(min)=$N_B max=$N_B_MAX (source N_a=$N_A)"

if (( N_B < N_A )); then
  echo "--- $VM serial tail ($TARGET_HANDLE) ---" >&2
  run_on "$TARGET_HANDLE" sudo tail -60 "${TARGET_STATE}/vms/${VMID}/serial.log" 2>/dev/null >&2 || true
  fail "heartbeat RESET across cutover: target min N_b=$N_B < source max N_a=$N_A (guest rebooted -> NOT a live migration)"
fi
pass "heartbeat continuous: target N_b=$N_B >= source N_a=$N_A (guest process survived the cutover)"

# Secondary reboot tell: a live cutover never re-runs getty on the target, so no
# fresh " login:" banner should appear in the target serial.log. (The source
# stream has exactly one from its own boot; the target's append-only log starts
# fresh at the cutover, so any login banner there is a reboot.)
LOGIN_T="$(login_banner_count "$TARGET_HANDLE" "$TARGET_STATE")"
info "target login-banner count=$LOGIN_T (expect 0 - a fresh banner means a reboot)"
(( LOGIN_T == 0 )) || fail "fresh getty login banner appeared on target ($LOGIN_T) - guest rebooted, not a live cutover"
pass "no fresh login banner on target (no reboot tell)"

# --- step 6: delete -> gone --------------------------------------------
echo "=== step 6: delete ==="
otx vm delete "$VM" --wait --force --wait-timeout "${PHASE_WAIT}s" || fail "vm delete failed"
assert_gone "$VM"
pass "delete -> gone"

trap - EXIT
echo
echo "${GREEN}=== vm-migration-live smoke PASSED ===${NC}"
