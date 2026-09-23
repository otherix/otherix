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
#      and the VM runs again on its source node).
#   5. a fresh offline migration to the SAME target, node-2, is accepted and
#      completes, and the VM runs there. The cancelled migration left its
#      stopped copy of the VM on node-2; the new migration names the cancelled
#      one as abandoned, so node-2 moves that copy into
#      <pool>/abandoned/<vm-id>-<cancelled-migration-id> (nothing is deleted)
#      instead of refusing the VM as already present. The smoke asserts the
#      moved copy is there.
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
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"
# Long enough for a few heartbeats to replace an observed phase that predates a
# migration's poweroff.
SETTLE_WAIT="${SETTLE_WAIT:-15}"
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }
vm_desired() { otx vm get "$1" --output json 2>/dev/null | jq -r '.desired_phase // empty' 2>/dev/null || true; }
# vm_node NAME -> the name of the node the VM is placed on ("" if unscheduled or
# gone).
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // empty' 2>/dev/null || true; }
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
# Succeeds with empty output when the probe ran and found none; fails when the
# probe could not run on the node, so a broken probe never reads as "no server".
# The remote script ends in `exit 0`, so a non-zero status can only come from
# reaching the node.
nbd_pids_on() {
  local out
  out="$(run_on "$1" bash -c "pgrep -x qemu-nbd 2>/dev/null | while read -r p; do
      tr '\\0' ' ' < /proc/\$p/cmdline 2>/dev/null | grep -qF '$2' && echo \$p
    done; exit 0" 2>/dev/null)" || return 1
  printf '%s\n' "$out" | tr -dc '0-9\n'
}

# wait_no_nbd HANDLE MIGRATION_ID FAIL_MESSAGE -> poll until the node runs no
# qemu-nbd for the migration; fail with the surviving pids on timeout, or say so
# when the node could not be probed at all.
wait_no_nbd() {
  local deadline=$(( SECONDS + CONVERGE_WAIT )) pids="" probed=0
  while (( SECONDS < deadline )); do
    if pids="$(nbd_pids_on "$1" "$2")"; then
      probed=1
      [[ -z "$pids" ]] && return 0
    else
      info "could not probe $1 for qemu-nbd; retrying"
    fi
    sleep 2
  done
  (( probed )) || fail "$3: could not probe $1 for qemu-nbd within ${CONVERGE_WAIT}s"
  fail "$3 (pids: $(echo "$pids" | tr '\n' ' '), after ${CONVERGE_WAIT}s)"
}

# wait_migration_terminal MIGRATION_ID -> echo the terminal phase; fail on timeout.
wait_migration_terminal() {
  local deadline=$(( SECONDS + MIGRATE_WAIT )) ph=""
  while (( SECONDS < deadline )); do
    ph="$(migration_phase "$1")"
    case "$ph" in completed|failed|cancelled) echo "$ph"; return 0 ;; esac
    sleep 2
  done
  fail "migration ${1:0:8} not terminal within ${MIGRATE_WAIT}s (phase='${ph:-none}')"
}

# ensure_running VM -> make sure the VM runs; fail if it will not. A VM that is
# meant to run is brought back up by its node's reconciler, and its observed
# phase lags by a heartbeat or two: right after an offline migration it can still
# read "running" from before the migration powered the guest off. So let it
# settle and wait for running; start it explicitly only if it is meant to be off.
ensure_running() {
  if [[ "$(vm_desired "$1")" == "running" ]]; then
    sleep "$SETTLE_WAIT"
    local deadline=$(( SECONDS + CONVERGE_WAIT ))
    while (( SECONDS < deadline )); do
      [[ "$(vm_phase "$1")" == "running" ]] && return 0
      sleep 2
    done
    fail "$1 not running ${CONVERGE_WAIT}s after settling (phase=$(vm_phase "$1"))"
  fi
  otx vm start "$1" --wait --wait-timeout 180s >/tmp/cancel_offline_start.out 2>&1 \
    || { cat /tmp/cancel_offline_start.out; fail "$1 does not start (phase=$(vm_phase "$1"))"; }
  [[ "$(vm_phase "$1")" == "running" ]] || fail "$1 not running after start (phase=$(vm_phase "$1"))"
}

# abandoned_copy_path NODE_INDEX POOL VM_ID MIGRATION_ID -> the dir that node's
# agent moves a copy left by the abandoned migration MIGRATION_ID into:
# <pool root>/abandoned/<vm-id>-<migration-id>. smoke_state's pools/ is what the
# agent sees as /var/lib/otherix/pools on both platforms. Prints nothing and
# fails unless every component is set and well-formed, so no caller can build a
# path with an empty or wildcard component.
abandoned_copy_path() {
  local root
  root="$(smoke_state "$1")"
  [[ -n "$root" && -n "$2" && "$2" != */* && "$2" != .* ]] || return 1
  [[ "$3" =~ ^[0-9a-f-]{36}$ && "$4" =~ ^[0-9a-f-]{36}$ ]] || return 1
  printf '%s/pools/%s/abandoned/%s-%s' "$root" "$2" "$3" "$4"
}

# file_on HANDLE PATH -> "present" or "absent" for the regular file PATH on the
# node. The remote script prints a verdict and ends in `exit 0`, so a non-zero
# status means the probe could not run on the node, never "absent".
file_on() {
  # shellcheck disable=SC2016 # $0 expands in the remote bash, bound to PATH
  run_on "$1" sudo bash -c 'if [ -f "$0" ]; then echo present; else echo absent; fi; exit 0' "$2" 2>/dev/null
}

# assert_abandoned_copy HANDLE PATH WHAT -> fail unless PATH on the node holds
# the moved disk, and fail loud when the node could not be probed.
assert_abandoned_copy() {
  local out
  out="$(file_on "$1" "$2/disk.qcow2")" || fail "$3: could not probe $1 for $2"
  [[ "$out" == "present" ]] || fail "$3: no disk.qcow2 under $2 on $1 (probe said '${out:-nothing}')"
}

# remove_abandoned_copy HANDLE PATH -> rm -rf exactly PATH on the node, and only
# when it is a path abandoned_copy_path built (never a glob, never a path with an
# empty component).
remove_abandoned_copy() {
  [[ "$2" =~ /pools/[^/]+/abandoned/[0-9a-f-]{36}-[0-9a-f-]{36}$ ]] || return 1
  run_on "$1" sudo rm -rf -- "$2"
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
  [[ -n "${ABANDONED:-}" ]] && remove_abandoned_copy "$SMOKE_HANDLE_2" "$ABANDONED" >/dev/null 2>&1 || true
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
SRC_NODE="$(vm_node "$VM")"
POOL="$(otx vm get "$VM" --output json | jq -r '.pool // empty')"
[[ -n "$POOL" ]] || fail "could not resolve the pool of $VM"
[[ "$SRC_NODE" == "$NODE1" ]] || fail "$VM not placed on node-1 (node=${SRC_NODE:-none})"
pass "created and running on node-1 (id=${VMID:0:8})"

# --- step 2: offline migrate, cancel once the target's qemu-nbd is up --
echo "=== step 2: offline-migrate -> $NODE2; cancel once node-2 serves the disk ==="
MIGRATION_ID=""; ABANDONED=""
otx vm migrate "$VM" --node "$NODE2" --offline >/tmp/cancel_offline_migrate.out 2>&1 \
  || { cat /tmp/cancel_offline_migrate.out; fail "vm migrate --offline request failed"; }
MIGRATION_ID="$(latest_migration_id "$VMID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"
ABANDONED="$(abandoned_copy_path 2 "$POOL" "$VMID" "$MIGRATION_ID")" \
  || fail "could not build the abandoned-copy path (pool='$POOL' vm='$VMID' migration='$MIGRATION_ID')"

deadline=$(( SECONDS + NBD_WAIT )); nbd_pid=""; probed=0
while (( SECONDS < deadline )); do
  if pids="$(nbd_pids_on "$SMOKE_HANDLE_2" "$MIGRATION_ID")"; then
    probed=1
    nbd_pid="$(head -1 <<<"$pids")"
    [[ "$nbd_pid" =~ ^[0-9]+$ ]] && break
  fi
  ph="$(migration_phase "$MIGRATION_ID")"
  case "$ph" in
    completed) fail "migration completed before its qemu-nbd was ever observed (too fast; raise DISK_GIB)" ;;
    failed|cancelled) fail "migration went '$ph' before the cancel was issued" ;;
  esac
  sleep 0.5
done
(( probed )) || fail "could not probe node-2 for qemu-nbd within ${NBD_WAIT}s"
[[ "$nbd_pid" =~ ^[0-9]+$ ]] || fail "node-2 never spawned a qemu-nbd for this migration within ${NBD_WAIT}s"
info "node-2 serving the destination disk (qemu-nbd pid=$nbd_pid); issuing cancel"

otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || fail "migration cancel request failed"
ph="$(migration_phase "$MIGRATION_ID")"
[[ "$ph" == "cancelled" ]] || fail "migration not cancelled right after cancel (got '$ph')"
pass "migration cancelled (CP authoritative)"

# --- step 3: the offline target was actually reaped --------------------
echo "=== step 3: assert node-2's incoming setup was reaped (no agent backstop exists) ==="
wait_no_nbd "$SMOKE_HANDLE_2" "$MIGRATION_ID" \
  "node-2 qemu-nbd for this migration still alive after cancel - the offline target was NOT reaped"
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
echo "=== step 4: VM intact and running on $NODE1 ==="
# An offline migration powers the guest off before pushing, so fail-safe-to-source
# here means the VM is still owned by node-1 with its disk intact - not that it is
# still running. Prove it materially by having it run again.
now_node="$(vm_node "$VM")"
[[ "$now_node" == "$SRC_NODE" ]] \
  || fail "VM moved off its source node after a cancelled migration ($SRC_NODE -> ${now_node:-none})"
ensure_running "$VM"
[[ "$(vm_node "$VM")" == "$SRC_NODE" ]] || fail "$VM runs off its source node (node=$(vm_node "$VM"))"
pass "VM intact and running on node-1 (fail-safe-to-source)"

# --- step 5: migrate again to the SAME target --------------------------
echo "=== step 5: offline-migrate -> $NODE2 again; its old copy moves aside ==="
# node-2 still holds the stopped copy the cancelled migration adopted. The new
# migration names the cancelled one as abandoned, so node-2 moves that copy
# into its pool's abandoned/ dir instead of refusing the VM as already present.
# A completed migration also proves the per-VM migration guard was released.
otx vm migrate "$VM" --node "$NODE2" --offline >/tmp/cancel_offline_migrate2.out 2>&1 \
  || { cat /tmp/cancel_offline_migrate2.out; fail "second vm migrate --offline request failed"; }
m2="$(latest_migration_id "$VMID")"
[[ "$m2" =~ ^[0-9a-f-]{36}$ && "$m2" != "$MIGRATION_ID" ]] \
  || fail "could not resolve the second migration id (got '${m2:-none}')"
ph2="$(wait_migration_terminal "$m2")"
[[ "$ph2" == "completed" ]] || fail "migration to the node a cancelled migration left a copy on went '$ph2', want completed"
now_node="$(vm_node "$VM")"
[[ "$now_node" == "$NODE2" ]] || fail "$VM not on node-2 after the second migration (node=${now_node:-none})"
ensure_running "$VM"
pass "second migration ${m2:0:8} completed; $VM running on node-2"

assert_abandoned_copy "$SMOKE_HANDLE_2" "$ABANDONED" "node-2 did not keep the cancelled migration's copy"
pass "the cancelled migration's copy sits at $ABANDONED on node-2"

trap - EXIT
cleanup
echo
echo "${GREEN}=== vm-migration-cancel-offline smoke PASSED ===${NC}"
