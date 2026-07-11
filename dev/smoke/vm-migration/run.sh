#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Offline VM migration smoke - drives `otherix vm migrate --offline` across the
# two-node dev stack through the `otherix` CLI as a real operator, closing a
# real-agent coverage gap: the CP-side migrate path is unit/e2e tested, but a
# full stop+copy+start cutover across two REAL agents (qemu + disk transfer) is
# only proven here.
#
# What it proves, end to end, on ONE VM:
#   create on the source node       -> running (qemu spawned, guest boots)
#   migrate --offline to the target -> the VM's current node changes to target,
#                                       phase is running on the target, and the
#                                       guest boots there (serial sentinel)
#   the migration record            -> reaches phase 'completed'
#   delete                          -> gone
#
# Offline migration stops the VM on the source, copies its disks to the target
# pool, and restarts it on the target (no live cutover). We pick the source from
# wherever the VM landed (node-1 by pinning create to it) and the target as a
# DIFFERENT ready node discovered from `node list`. After the cutover we assert
# the guest actually boots on the target by waiting for the every-boot guest-up
# sentinel in the TARGET node's serial.log - a real console probe, not just a
# CP-phase read (mirrors the vm-lifecycle smoke's sentinel technique).
#
# The guest-up sentinel is a systemd oneshot (cloud-init below) that echoes
# OTHERIX_GUEST_UP to the serial console on EVERY boot; the agent persists that
# console to <state_root>/vms/<id>/serial.log (append-only). The VM id is stable
# across the migration, so the post-cutover serial.log lives on the TARGET node
# under the same id.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# both node-1 and node-2 ready with the cluster default pool reconciled.
#
# Usage: make smoke-vm-migration   (or: bash dev/smoke/vm-migration/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # source node (create is pinned here)
VM="${VM_NAME:-migsmoke}"                     # the single migration VM; delete-firsted
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"           # seconds for the offline migrate cutover (disk copy on TCG is slow)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a full guest boot (TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
MIG_PHASE_WAIT="${MIG_PHASE_WAIT:-120}"       # seconds to wait for the migration record -> completed

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

# guest_up_count HANDLE STATE -> number of boot-completion markers in the VM's
# serial.log on the node identified by HANDLE/STATE (0 when the file is absent).
# Counts EITHER the cloud-init OTHERIX_GUEST_UP sentinel OR the getty login
# prompt: a migrated guest boots from the COPIED disk where cloud-init already
# ran on the source, so its first-boot sentinel does not re-fire, but the login
# prompt appears on every boot and is the same "guest reached userspace" proof
# an operator sees. Append-only log -> the count is monotonic, so callers wait
# for it to INCREASE.
guest_up_count() {
  local handle="$1" state="$2" n
  n="$(run_on "$handle" sudo grep -cE "OTHERIX_GUEST_UP| login:" "${state}/vms/${VMID}/serial.log" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_guest_up HANDLE STATE BASE -> block until guest_up_count on that node
# exceeds BASE (a fresh boot reached multi-user.target).
wait_guest_up() {
  local handle="$1" state="$2" base="$3" deadline; deadline=$(( SECONDS + GUEST_WAIT ))
  info "waiting for a guest boot to complete on $handle (sentinel count > ${base}, <= ${GUEST_WAIT}s)"
  while (( SECONDS < deadline )); do
    (( $(guest_up_count "$handle" "$state") > base )) && { pass "guest booted"; return 0; }
    sleep 5
  done
  echo "--- $VM serial tail ($handle) ---"
  run_on "$handle" sudo tail -40 "${state}/vms/${VMID}/serial.log" 2>/dev/null || true
  fail "guest did not reach a booted state on $handle within ${GUEST_WAIT}s"
}

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- the guest-up cloud-config (every-boot serial sentinel) ------------
# A systemd oneshot, ordered After=multi-user.target and WantedBy it, so it runs
# at the tail of EVERY boot. It echoes the sentinel to the arch-correct serial
# device (ttyS0 on amd64, ttyAMA0 on arm64), which the agent captures into
# serial.log. Quoted heredoc -> the $SC shell var stays literal for the guest.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-guest-up.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix migration-smoke guest-up sentinel
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; echo OTHERIX_GUEST_UP > "$SC"'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-guest-up.service ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- preconditions -----------------------------------------------------
echo "=== vm-migration smoke: preconditions ==="
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

# Resolve the target node's exec handle + state root for the serial probe. The
# dev stack maps node-1 -> handle/state index 1 and node-2 -> index 2; create is
# pinned to node-1, so the discovered second ready node is always index 2.
TARGET_HANDLE="$SMOKE_HANDLE_2"; TARGET_STATE="$SMOKE_STATE_2"

# --- step 1: create on the source -> running ---------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of a stale leftover
printf '%s' "$CLOUD_INIT" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --disk-gib 10 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
info "VM id=$VMID"

# Confirm the VM landed on the source node we pinned (this is the migration's
# from-node; the offline cutover needs a different to-node).
SOURCE="$(vm_node "$VM")"
[[ "$SOURCE" == "$NODE1" ]] || fail "VM landed on '$SOURCE', expected source $NODE1"
[[ "$SOURCE" != "$TARGET" ]] || fail "source and target resolved to the same node ($SOURCE)"
pass "created and running on source $SOURCE"

# --- step 2: offline migrate to the target -----------------------------
echo "=== step 2: migrate $VM --offline $SOURCE -> $TARGET ==="
otx vm migrate "$VM" --node "$TARGET" --offline \
  --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "vm migrate --offline did not complete within ${MIGRATE_WAIT}s"

# After the cutover the CP repoints .node to the target and the VM phase is
# running there. The migrate --wait already blocked on the task, but the
# CP-projected node/phase can lag the task terminal slightly, so poll.
assert_node "$VM" "$TARGET"
assert_phase "$VM" running
pass "VM migrated: current node changed to $TARGET, phase running"

# --- step 3: the migration record reached completed --------------------
echo "=== step 3: migration record -> completed ==="
# `migration list --vm` filters by VM UUID, newest-first; the first row is this
# migration. Then `migration get <id>` surfaces its terminal phase.
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
pass "migration $MIGID phase=completed"

# --- step 4: guest boots on the target (real console probe) ------------
echo "=== step 4: guest boots on the target $TARGET ==="
# The offline cutover restarted the VM on the target; wait for a fresh guest-up
# sentinel to appear in the TARGET node's serial.log (count > 0). This proves the
# guest actually boots after migration, not merely that qemu+QMP came up.
wait_guest_up "$TARGET_HANDLE" "$TARGET_STATE" 0
pass "guest booted on target $TARGET"

# --- step 5: delete -> gone --------------------------------------------
echo "=== step 5: delete ==="
otx vm delete "$VM" --wait --force --wait-timeout "${PHASE_WAIT}s" || fail "vm delete failed"
assert_gone "$VM"
pass "delete -> gone"

trap - EXIT
echo
echo "${GREEN}=== vm-migration smoke PASSED ===${NC}"
