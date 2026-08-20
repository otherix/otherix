#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Smoke: cancelling an OFFLINE migration reaps the target's incoming setup,
# instead of leaving a qemu-nbd server and a reserved migration port behind for
# the life of the agent process.
#
# THE GAP (pre-fix): three control-plane sites declined to tell a bound target to
# reap when the migration was offline - propagateCancel, cancelTargetIncoming and
# reapTargetIncoming - all on the premise that "an offline target has nothing to
# reap". It has: the qemu-nbd server holding the destination disk's write lock,
# the reserved ingress port, and a migration record nothing else ever makes
# terminal. That last one keeps the agent's HasActiveForVM true, which blocks
# tombstone teardown of the VM for as long as the agent runs. Unlike a live
# target, an offline target has NO agent-side backstop: its record sits at
# phase=setup for the whole of a healthy push, so no deadline can distinguish an
# abandoned setup from a slow one.
#
# THE FIX: all three sites now reap both modes. The agent's existing offline
# cancel arm kills the qemu-nbd, frees the port and stamps the record terminal.
#
# WHAT THIS SMOKE PROVES (real three-node stack, operator CLI):
#   1. create a VM on node-1 -> running.
#   2. start an OFFLINE migration to node-2 and wait until node-2's qemu-nbd for
#      THIS migration exists (it is spawned in StartIncoming, before the source
#      even begins pushing, so this is the widest available window).
#   3. `otherix migration cancel`, then ASSERT PROMPTLY: the migration is
#      cancelled; node-2's qemu-nbd for this migration is GONE; the backing task
#      finalizes `cancelled`.
#   4. the VM is still usable on node-1 (an offline migration powers the guest
#      off before pushing, so fail-safe-to-source here means the disk is intact
#      and the VM starts again on its source node), and a fresh migration is
#      accepted and completes - proving the per-VM guard was released.
#
# NOT COVERED HERE: the double-release this branch also fixes needs a cancel to
# land after the source push already completed, so that the cutover commits and
# the post-cutover start runs releaseIncomingNBD against an already-terminal
# record. That interleaving is not reachable deterministically from the CLI; it
# is covered by TestReleaseIncomingNBDLeavesTerminalRecordPortsAlone.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or local-dev-deploy to refresh code)
# All three nodes ready, default pool reconciled. jq + etcdctl on PATH (etcd dev member
# on 127.0.0.1:2379, no TLS - used to read the backing task, which has no CLI).
#
# Usage: make smoke-vm-migration-cancel-offline
#        (or: bash dev/smoke/vm-migration-cancel-offline/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OTX="${OTX:-${REPO_ROOT}/bin/otherix}"
NODE1="node-1"
NODE2="node-2"
NODE3="node-3"
VM="${VM_NAME:-cancel-offline}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
# A larger disk lengthens the source-side qemu-img push, widening the window in
# which the cancel lands pre-cutover. Offline migration has no bandwidth cap to
# throttle with (that is a live migrate parameter), so disk size is the only lever.
DISK_GIB="${DISK_GIB:-10}"
CREATE_WAIT="${CREATE_WAIT:-600}"
NBD_WAIT="${NBD_WAIT:-120}"
# The reap must converge in SECONDS. There is no agent-side backstop for an
# offline target at all, so anything that has not converged by here never will.
CONVERGE_WAIT="${CONVERGE_WAIT:-60}"
REMIGRATE_WAIT="${REMIGRATE_WAIT:-600}"
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.current_node_id // empty' 2>/dev/null || true; }
latest_migration_id() {
  otx migration list --output json 2>/dev/null \
    | jq -r --arg vm "$1" '[.data[]? | select(.vm_id==$vm)] | sort_by(.created_at) | last | .id // empty' 2>/dev/null || true
}
migration_phase() { otx migration get "$1" --output json 2>/dev/null | jq -r '.phase // empty' 2>/dev/null || true; }

# nbd_pids_on HANDLE MIGRATION_ID -> pids of qemu-nbd servers on that node whose
# cmdline carries MIGRATION_ID (the export name the target mints is the migration
# id). `pgrep -x qemu-nbd` matches on the executable name, so it cannot match the
# wrapper shell this runs in - a plain `pgrep -f <id>` would, because the id is in
# the wrapper's own cmdline, and would report a phantom process forever.
nbd_pids_on() {
  run_on "$1" bash -c "pgrep -x qemu-nbd 2>/dev/null | while read -r p; do
      tr '\\0' ' ' < /proc/\$p/cmdline 2>/dev/null | grep -qF '$2' && echo \$p
    done" 2>/dev/null | tr -dc '0-9\n' || true
}

# task_status_for MIGRATION_ID -> the backing vm.migrate task's Status (the task
# has no CLI surface; read it from the dev etcd member, PascalCase keys). A
# vm.migrate task's ResourceType is "migration" and ResourceID is the MIGRATION
# id, not the VM id.
task_status_for() {
  ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" get /otherix/tasks/ --prefix --print-value-only 2>/dev/null \
    | jq -rs --arg mid "$1" 'map(select(.Type=="vm.migrate" and .ResourceID==$mid)) | sort_by(.CreatedAt) | last | .Status // empty' 2>/dev/null || true
}

cleanup() {
  echo "--- cleanup ---"
  [[ -n "${MIGRATION_ID:-}" ]] && otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || true
  otx vm delete "$VM" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  run_on "$SMOKE_HANDLE_2" sudo pkill -f "name $VM" >/dev/null 2>&1 || true
  run_on "$SMOKE_HANDLE_3" sudo pkill -f "name $VM" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-cancel-offline: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (reads the backing task; no task CLI)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 || fail "etcd not reachable at $ETCD_EP"
for n in "$NODE1" "$NODE2" "$NODE3"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
pass "CP up; all three nodes ready; etcd reachable"

# --- step 1: create the VM on node-1 -> running ------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
otx vm delete "$VM" --force --wait --wait-timeout 60s >/dev/null 2>&1 || true
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --disk-gib "$DISK_GIB" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
[[ "$(vm_phase "$VM")" == "running" ]] || fail "$VM not running after create"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
SRC_NODE_ID="$(vm_node "$VM")"
pass "created and running on node-1 (id=${VMID:0:8})"

# --- step 2: offline migrate, cancel once the target's qemu-nbd is up --
echo "=== step 2: offline-migrate -> $NODE2; cancel once node-2 serves the disk ==="
MIGRATION_ID=""
otx vm migrate "$VM" --node "$NODE2" --offline >/tmp/cancel_offline_migrate.out 2>&1 \
  || { cat /tmp/cancel_offline_migrate.out; fail "vm migrate --offline request failed"; }
MIGRATION_ID="$(latest_migration_id "$VMID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"

deadline=$(( SECONDS + NBD_WAIT )); nbd_pid=""
while (( SECONDS < deadline )); do
  nbd_pid="$(nbd_pids_on "$SMOKE_HANDLE_2" "$MIGRATION_ID" | head -1)"
  [[ "$nbd_pid" =~ ^[0-9]+$ ]] && break
  ph="$(migration_phase "$MIGRATION_ID")"
  case "$ph" in
    completed) fail "migration completed before its qemu-nbd was ever observed (too fast; raise DISK_GIB)" ;;
    failed|cancelled) fail "migration went '$ph' before the cancel was issued" ;;
  esac
  sleep 0.5
done
[[ "$nbd_pid" =~ ^[0-9]+$ ]] || fail "node-2 never spawned a qemu-nbd for this migration within ${NBD_WAIT}s"
info "node-2 serving the destination disk (qemu-nbd pid=$nbd_pid); issuing cancel"

otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || fail "migration cancel request failed"
ph="$(migration_phase "$MIGRATION_ID")"
[[ "$ph" == "cancelled" ]] || fail "migration not cancelled right after cancel (got '$ph')"
pass "migration cancelled (CP authoritative)"

# --- step 3: the offline target was actually reaped --------------------
echo "=== step 3: assert node-2's incoming setup was reaped (no agent backstop exists) ==="
deadline=$(( SECONDS + CONVERGE_WAIT )); leaked="?"
while (( SECONDS < deadline )); do
  if [[ -z "$(nbd_pids_on "$SMOKE_HANDLE_2" "$MIGRATION_ID")" ]]; then leaked="no"; break; fi
  leaked="yes"; sleep 2
done
[[ "$leaked" == "no" ]] \
  || fail "node-2 qemu-nbd for this migration still alive ${CONVERGE_WAIT}s after cancel - the offline target was NOT reaped"
pass "node-2 qemu-nbd reaped promptly"

deadline=$(( SECONDS + CONVERGE_WAIT )); tstatus=""
while (( SECONDS < deadline )); do
  tstatus="$(task_status_for "$MIGRATION_ID")"
  [[ "$tstatus" == "cancelled" ]] && break
  [[ "$tstatus" == "failed" ]] && fail "backing task finalized 'failed' after a user cancel - want 'cancelled'"
  sleep 2
done
[[ "$tstatus" == "cancelled" ]] || fail "backing task did not finalize 'cancelled' within ${CONVERGE_WAIT}s (got '${tstatus:-none}')"
pass "backing vm.migrate task finalized 'cancelled'"

# --- step 4: the VM is intact on its source node and usable ------------
echo "=== step 4: VM intact on $NODE1 and migratable again ==="
# An offline migration powers the guest off before pushing, so fail-safe-to-source
# here means the VM is still owned by node-1 with its disk intact - not that it is
# still running. Prove it materially by starting it again.
now_node="$(vm_node "$VM")"
[[ "$now_node" == "$SRC_NODE_ID" ]] \
  || fail "VM moved off its source node after a cancelled migration (${SRC_NODE_ID:0:8} -> ${now_node:0:8})"
if [[ "$(vm_phase "$VM")" != "running" ]]; then
  otx vm start "$VM" --wait --wait-timeout 180s >/dev/null 2>&1 \
    || fail "VM does not start again on node-1 after the cancelled migration - the source copy was not preserved"
fi
[[ "$(vm_phase "$VM")" == "running" ]] || fail "VM not running on node-1 (phase=$(vm_phase "$VM"))"
pass "VM intact and running on node-1 (fail-safe-to-source)"

# A fresh offline migration must be accepted and complete, proving the per-VM
# migration guard was released by the terminal cancel.
#
# It targets node-3, not node-2, for a reason that is a KNOWN LIMITATION rather
# than a property of this smoke: the agent's offline cancel arm reaps the
# qemu-nbd, the port and the record, but deliberately does NOT remove the
# adopted VM record the cancelled migration left behind. AdoptForMigration
# refuses a UUID already in m.vms, so a second migration back to the SAME target
# fails "already present" until that agent restarts. Removing the adopted record
# means removing its destination disk with it, and a cancel that raced a
# completed push would then destroy the copy the committed cutover is about to
# start - trading a recoverable dead end for a lost VM. Pre-existing: before the
# control plane reaped offline targets at all, the same stale record blocked the
# same re-migration.
otx vm migrate "$VM" --node "$NODE3" --offline --wait --wait-timeout "${REMIGRATE_WAIT}s" \
  >/tmp/cancel_offline_migrate2.out 2>&1 \
  || { cat /tmp/cancel_offline_migrate2.out; fail "re-migration after the cancel did not complete"; }
m2="$(latest_migration_id "$VMID")"
ph2="$(migration_phase "$m2")"
[[ "$ph2" == "completed" ]] || fail "re-migration did not complete (phase='${ph2:-none}')"
pass "re-migration completed; the per-VM migration guard was released by the cancel"

trap - EXIT
cleanup
echo
echo "${GREEN}=== vm-migration-cancel-offline smoke PASSED ===${NC}"
