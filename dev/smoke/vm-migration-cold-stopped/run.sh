#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Smoke: an offline (cold) migration never leaves a qemu-nbd server behind on
# its target node.
#
# An offline migration's target serves the destination disk over qemu-nbd while
# the source pushes it. That server holds the disk's write lock, so while it
# runs the VM cannot start on its new node. When the migrated VM is to run, the
# post-cutover start releases the server; a VM that was stopped when it was
# migrated is not started, so the target agent must release the server itself.
# An agent restart in the middle of a migration must not leave one behind either.
#
# WHAT THIS SMOKE PROVES (real three-node stack, operator CLI):
#   Scenario 1 - a stopped VM migrates cleanly:
#     1. create a VM on node-1, power it off.
#     2. migrate it offline to node-2 -> completed, VM on node-2.
#     3. node-2 runs no qemu-nbd for that migration within seconds.
#     4. the VM starts on node-2 (it could not while a server held the disk).
#     5. the VM deletes cleanly.
#   Scenario 2 - the target agent restarts mid-migration:
#     1. create a running VM on node-1 (large disk, to widen the push window).
#     2. start an offline migration to node-2 and wait until node-2 serves the
#        destination disk over qemu-nbd.
#     3. restart node-2's agent; node-2 is ready again.
#     4. node-2 runs no qemu-nbd for that migration within seconds.
#     5. once the migration is terminal, the VM starts wherever it left the VM:
#        on node-2 if it completed, on node-1 if it failed or was cancelled.
#     6. node-2 still runs no qemu-nbd for that migration.
#   Scenario 3 - the SOURCE agent restarts mid-push, then the VM migrates again:
#     1. create a VM on node-1 (large disk), power it off, and write 4 GiB of
#        data into its disk so the source push takes long enough to interrupt.
#     2. start an offline migration to node-2 and wait until node-1 runs the
#        qemu-img push for it.
#     3. restart node-1's agent; the migration fails and the VM stays on node-1.
#     4. node-2 still holds the stopped copy the failed migration adopted.
#        Migrate the VM to node-2 again: it completes and the VM runs there,
#        because the new migration names the failed one as abandoned and node-2
#        moves the old copy into <pool>/abandoned/<vm-id>-<failed-migration-id>
#        (nothing is deleted) instead of refusing the VM as already present.
#     5. that moved copy is on node-2.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or local-dev-deploy to refresh code)
# All three nodes ready, default pool reconciled. jq on PATH; qemu-io on the
# nodes (on Linux, on the host).
#
# Usage: make smoke-vm-migration-cold-stopped
#        (or: bash dev/smoke/vm-migration-cold-stopped/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OTX="${OTX:-${REPO_ROOT}/bin/otherix}"
NODE1="node-1"
NODE2="node-2"
NODE3="node-3"
VM1="${VM1_NAME:-cold-stopped}"
VM2="${VM2_NAME:-cold-restart}"
VM3="${VM3_NAME:-cold-source-restart}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
DISK_GIB="${DISK_GIB:-4}"
# A larger disk lengthens the source-side push, widening the window in which an
# agent restart lands while the disk is still in flight: scenario 3 fills the
# disk with data from its 1 GiB offset to its end. At least 5.
RESTART_DISK_GIB="${RESTART_DISK_GIB:-10}"
CREATE_WAIT="${CREATE_WAIT:-600}"
NBD_WAIT="${NBD_WAIT:-120}"
CONVERGE_WAIT="${CONVERGE_WAIT:-60}"
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"
READY_WAIT="${READY_WAIT:-120}"
# Long enough for a few heartbeats to replace an observed phase that predates a
# migration's poweroff.
SETTLE_WAIT="${SETTLE_WAIT:-15}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }
vm_desired() { otx vm get "$1" --output json 2>/dev/null | jq -r '.desired_phase // empty' 2>/dev/null || true; }
# vm_node NAME -> the name of the node the VM is placed on ("" if unscheduled or
# gone). The CP repoints it only at a migration cutover.
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // empty' 2>/dev/null || true; }
node_status() { otx node get "$1" --output json 2>/dev/null | jq -r '.status // empty' 2>/dev/null || true; }
vm_listed() { otx vm list --output json 2>/dev/null | jq -e --arg n "$1" '[.data[]? | select(.name==$n)] | length > 0' >/dev/null 2>&1; }
latest_migration_id() {
  otx migration list --output json 2>/dev/null \
    | jq -r --arg vm "$1" '[.data[]? | select(.vm_id==$vm)] | sort_by(.created_at) | last | .id // empty' 2>/dev/null || true
}
migration_phase() { otx migration get "$1" --output json 2>/dev/null | jq -r '.phase // empty' 2>/dev/null || true; }

# pids_on HANDLE EXE MIGRATION_ID -> pids of EXE processes on that node whose
# cmdline carries MIGRATION_ID (the export name the target mints, and so the
# source push's target options, is the migration id). `pgrep -x EXE` matches on
# the executable name, so it cannot match the wrapper shell this runs in - a
# plain `pgrep -f <id>` would, because the id is in the wrapper's own cmdline,
# and would report a phantom process forever.
# Succeeds with empty output when the probe ran and found none; fails when the
# probe could not run on the node, so a broken probe never reads as "no process".
# The remote script ends in `exit 0`, so a non-zero status can only come from
# reaching the node.
pids_on() {
  local out
  out="$(run_on "$1" bash -c "pgrep -x '$2' 2>/dev/null | while read -r p; do
      tr '\\0' ' ' < /proc/\$p/cmdline 2>/dev/null | grep -qF '$3' && echo \$p
    done; exit 0" 2>/dev/null)" || return 1
  printf '%s\n' "$out" | tr -dc '0-9\n'
}

# nbd_pids_on HANDLE MIGRATION_ID -> pids of the qemu-nbd servers a target runs
# for the migration (see pids_on).
nbd_pids_on() { pids_on "$1" qemu-nbd "$2"; }

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
  otx vm start "$1" --wait --wait-timeout 180s >/tmp/cold_stopped_start.out 2>&1 \
    || { cat /tmp/cold_stopped_start.out; fail "$1 does not start (phase=$(vm_phase "$1"))"; }
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

cleanup() {
  echo "--- cleanup ---"
  [[ -n "${MIGRATION_ID:-}" ]] && otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || true
  otx vm delete "$VM1" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  otx vm delete "$VM2" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  otx vm delete "$VM3" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  [[ -n "${ABANDONED:-}" ]] && remove_abandoned_copy "$SMOKE_HANDLE_2" "$ABANDONED" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-cold-stopped: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build)"
smoke_require_node_cmd qemu-io
(( RESTART_DISK_GIB >= 5 )) || fail "RESTART_DISK_GIB must be at least 5 (scenario 3 fills the disk from its 1 GiB offset)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
for n in "$NODE1" "$NODE2" "$NODE3"; do
  st="$(node_status "$n")"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
pass "CP up; all three nodes ready"

# === scenario 1: a stopped VM migrates cleanly ==========================
echo "=== scenario 1.1: create $VM1 on $NODE1, power it off ==="
MIGRATION_ID=""
otx vm delete "$VM1" --force --wait --wait-timeout 60s >/dev/null 2>&1 || true
otx vm create "$VM1" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --disk-gib "$DISK_GIB" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
VM1ID="$(otx vm get "$VM1" --output json | jq -r '.id')"
otx vm poweroff "$VM1" --wait --wait-timeout 180s >/dev/null || fail "vm poweroff failed"
[[ "$(vm_phase "$VM1")" == "stopped" ]] || fail "$VM1 not stopped after poweroff (phase=$(vm_phase "$VM1"))"
pass "$VM1 created on node-1 and stopped (id=${VM1ID:0:8})"

echo "=== scenario 1.2: offline-migrate the stopped VM -> $NODE2 ==="
otx vm migrate "$VM1" --node "$NODE2" --offline --wait --wait-timeout "${MIGRATE_WAIT}s" \
  >/tmp/cold_stopped_migrate.out 2>&1 \
  || { cat /tmp/cold_stopped_migrate.out; fail "offline migration of the stopped VM did not complete"; }
MIGRATION_ID="$(latest_migration_id "$VM1ID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"
ph="$(migration_phase "$MIGRATION_ID")"
[[ "$ph" == "completed" ]] || fail "migration not completed (phase='${ph:-none}')"
now_node="$(vm_node "$VM1")"
[[ "$now_node" == "$NODE2" ]] || fail "$VM1 not on node-2 after the migration (node=${now_node:-none})"
pass "migration ${MIGRATION_ID:0:8} completed; $VM1 on node-2"

echo "=== scenario 1.3: node-2 runs no qemu-nbd for the completed migration ==="
wait_no_nbd "$SMOKE_HANDLE_2" "$MIGRATION_ID" \
  "node-2 still runs qemu-nbd for a completed migration of a stopped VM"
pass "node-2 released the incoming qemu-nbd"

echo "=== scenario 1.4: $VM1 starts on node-2 ==="
ensure_running "$VM1"
[[ "$(vm_node "$VM1")" == "$NODE2" ]] || fail "$VM1 started off node-2 (node=$(vm_node "$VM1"))"
pass "$VM1 running on node-2"

echo "=== scenario 1.5: delete $VM1 ==="
otx vm delete "$VM1" --force --wait --wait-timeout 180s >/dev/null || fail "vm delete $VM1 failed"
! vm_listed "$VM1" || fail "$VM1 still listed after delete"
pass "$VM1 deleted"

# === scenario 2: the target agent restarts mid-migration ================
echo "=== scenario 2.1: create $VM2 on $NODE1 -> running ==="
MIGRATION_ID=""
otx vm delete "$VM2" --force --wait --wait-timeout 60s >/dev/null 2>&1 || true
otx vm create "$VM2" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --disk-gib "$RESTART_DISK_GIB" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
[[ "$(vm_phase "$VM2")" == "running" ]] || fail "$VM2 not running after create"
VM2ID="$(otx vm get "$VM2" --output json | jq -r '.id')"
[[ "$(vm_node "$VM2")" == "$NODE1" ]] || fail "$VM2 not placed on node-1 (node=$(vm_node "$VM2"))"
pass "$VM2 created and running on node-1 (id=${VM2ID:0:8})"

echo "=== scenario 2.2: offline-migrate -> $NODE2; wait until node-2 serves the disk ==="
otx vm migrate "$VM2" --node "$NODE2" --offline >/tmp/cold_restart_migrate.out 2>&1 \
  || { cat /tmp/cold_restart_migrate.out; fail "vm migrate --offline request failed"; }
MIGRATION_ID="$(latest_migration_id "$VM2ID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"
deadline=$(( SECONDS + NBD_WAIT )); nbd_pid=""; probed=0
while (( SECONDS < deadline )); do
  if pids="$(nbd_pids_on "$SMOKE_HANDLE_2" "$MIGRATION_ID")"; then
    probed=1
    nbd_pid="$(head -1 <<<"$pids")"
    [[ "$nbd_pid" =~ ^[0-9]+$ ]] && break
  fi
  ph="$(migration_phase "$MIGRATION_ID")"
  case "$ph" in
    completed) fail "migration completed before its qemu-nbd was ever observed (too fast; raise RESTART_DISK_GIB)" ;;
    failed|cancelled) fail "migration went '$ph' before the agent restart" ;;
  esac
  sleep 0.5
done
(( probed )) || fail "could not probe node-2 for qemu-nbd within ${NBD_WAIT}s"
[[ "$nbd_pid" =~ ^[0-9]+$ ]] || fail "node-2 never spawned a qemu-nbd for this migration within ${NBD_WAIT}s"
info "node-2 serving the destination disk (qemu-nbd pid=$nbd_pid); restarting node-2's agent"

echo "=== scenario 2.3: restart node-2's agent ==="
smoke_restart_agent 2 || fail "could not restart node-2's agent"
deadline=$(( SECONDS + READY_WAIT )); st=""
while (( SECONDS < deadline )); do
  st="$(node_status "$NODE2")"
  [[ "$st" == "ready" ]] && break
  sleep 2
done
[[ "$st" == "ready" ]] || fail "node-2 not ready ${READY_WAIT}s after the agent restart (status='${st:-none}')"
pass "node-2 agent restarted and ready"

echo "=== scenario 2.4: node-2 runs no qemu-nbd for the migration ==="
wait_no_nbd "$SMOKE_HANDLE_2" "$MIGRATION_ID" \
  "node-2 still runs qemu-nbd for the migration after its agent restarted"
pass "no qemu-nbd left on node-2 after the agent restart"

echo "=== scenario 2.5: the VM is usable where the migration left it ==="
outcome="$(wait_migration_terminal "$MIGRATION_ID")"
now_node="$(vm_node "$VM2")"
case "$outcome" in
  completed)
    [[ "$now_node" == "$NODE2" ]] || fail "migration completed but $VM2 is not on node-2 (node=${now_node:-none})"
    ensure_running "$VM2"
    pass "migration completed despite the restart; $VM2 running on node-2"
    ;;
  failed|cancelled)
    [[ "$now_node" == "$NODE1" ]] \
      || fail "migration $outcome but $VM2 is not on node-1 (node=${now_node:-none})"
    ensure_running "$VM2"
    pass "migration $outcome after the restart; $VM2 running on node-1"
    ;;
esac
info "scenario 2 outcome: migration $outcome"

echo "=== scenario 2.6: node-2 still runs no qemu-nbd for the terminal migration ==="
# A setup redelivered to the restarted agent could spawn a fresh server after
# the check in 2.4; once the migration is terminal nothing may be left.
wait_no_nbd "$SMOKE_HANDLE_2" "$MIGRATION_ID" \
  "node-2 runs qemu-nbd for the migration after it went $outcome"
pass "no qemu-nbd on node-2 for the $outcome migration"

# === scenario 3: the source agent restarts mid-push ======================
echo "=== scenario 3.1: create $VM3 on $NODE1, power it off, fill its disk ==="
MIGRATION_ID=""; ABANDONED=""
otx vm delete "$VM3" --force --wait --wait-timeout 60s >/dev/null 2>&1 || true
otx vm create "$VM3" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --disk-gib "$RESTART_DISK_GIB" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
VM3ID="$(otx vm get "$VM3" --output json | jq -r '.id')"
[[ "$VM3ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the id of $VM3 (got '${VM3ID:-none}')"
POOL="$(otx vm get "$VM3" --output json | jq -r '.pool // empty')"
[[ -n "$POOL" && "$POOL" != */* ]] || fail "could not resolve the pool of $VM3 (got '${POOL:-none}')"
[[ "$(vm_node "$VM3")" == "$NODE1" ]] || fail "$VM3 not placed on node-1 (node=$(vm_node "$VM3"))"
otx vm poweroff "$VM3" --wait --wait-timeout 180s >/dev/null || fail "vm poweroff failed"
[[ "$(vm_phase "$VM3")" == "stopped" ]] || fail "$VM3 not stopped after poweroff (phase=$(vm_phase "$VM3"))"
disk="$(smoke_state 1)/pools/${POOL}/vms/${VM3ID}/disk.qcow2"
st="$(file_on "$SMOKE_HANDLE_1" "$disk")" || fail "could not probe node-1 for $disk"
[[ "$st" == "present" ]] || fail "$VM3 disk not found at $disk on node-1 (probe said '${st:-nothing}')"
# The push copies only allocated data, and a fresh cloud image is small, so
# fill the disk with pattern data from the 1 GiB offset (past the image) to its
# end, giving the restart time to land mid-push; RESTART_DISK_GIB sizes it.
# 256 MiB per request: a single large qemu-io write allocates a buffer of its
# full size and can meet the OOM killer on a small node.
chunks=$(( (RESTART_DISK_GIB - 1) * 4 ))
# shellcheck disable=SC2016 # $0, $1 and $i expand in the remote bash, bound to the disk and chunk count
run_on "$SMOKE_HANDLE_1" sudo bash -c 'for i in $(seq 0 $(( $1 - 1 ))); do qemu-io -f qcow2 -c "write -P 0x5a $((1024 + i*256))M 256M" "$0" >/dev/null || exit 1; done' "$disk" "$chunks" \
  || fail "could not write data into $VM3's disk on node-1"
pass "$VM3 stopped on node-1 with $(( RESTART_DISK_GIB - 1 )) GiB of data in its disk (id=${VM3ID:0:8}, pool=$POOL)"

echo "=== scenario 3.2: offline-migrate -> $NODE2; wait until node-1 pushes the disk ==="
otx vm migrate "$VM3" --node "$NODE2" --offline >/tmp/cold_source_restart_migrate.out 2>&1 \
  || { cat /tmp/cold_source_restart_migrate.out; fail "vm migrate --offline request failed"; }
MIGRATION_ID="$(latest_migration_id "$VM3ID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"
FAILED_ID="$MIGRATION_ID"
ABANDONED="$(abandoned_copy_path 2 "$POOL" "$VM3ID" "$FAILED_ID")" \
  || fail "could not build the abandoned-copy path (pool='$POOL' vm='$VM3ID' migration='$FAILED_ID')"
deadline=$(( SECONDS + NBD_WAIT )); img_pid=""; probed=0
while (( SECONDS < deadline )); do
  if pids="$(pids_on "$SMOKE_HANDLE_1" qemu-img "$MIGRATION_ID")"; then
    probed=1
    img_pid="$(head -1 <<<"$pids")"
    [[ "$img_pid" =~ ^[0-9]+$ ]] && break
  fi
  ph="$(migration_phase "$MIGRATION_ID")"
  case "$ph" in
    completed) fail "migration completed before its push was ever observed (too fast; raise RESTART_DISK_GIB)" ;;
    failed|cancelled) fail "migration went '$ph' before the source agent restart" ;;
  esac
  sleep 0.5
done
(( probed )) || fail "could not probe node-1 for qemu-img within ${NBD_WAIT}s"
[[ "$img_pid" =~ ^[0-9]+$ ]] || fail "node-1 never ran a qemu-img push for this migration within ${NBD_WAIT}s"
info "node-1 pushing the disk (qemu-img pid=$img_pid); restarting node-1's agent"

echo "=== scenario 3.3: restart node-1's agent; the migration fails ==="
smoke_restart_agent 1 || fail "could not restart node-1's agent"
# Best effort: node-1 may never have been seen leaving ready, so this wait can
# pass at once. The assertion is the migration outcome and the VM's node below.
deadline=$(( SECONDS + READY_WAIT )); st=""
while (( SECONDS < deadline )); do
  st="$(node_status "$NODE1")"
  [[ "$st" == "ready" ]] && break
  sleep 2
done
[[ "$st" == "ready" ]] || info "node-1 not ready ${READY_WAIT}s after the agent restart (status='${st:-none}')"
outcome="$(wait_migration_terminal "$MIGRATION_ID")"
case "$outcome" in
  failed) ;;
  completed) fail "migration completed: the push finished before the source agent restart (raise RESTART_DISK_GIB)" ;;
  *) fail "migration went '$outcome' after the source agent restart, want failed" ;;
esac
now_node="$(vm_node "$VM3")"
[[ "$now_node" == "$NODE1" ]] || fail "migration failed but $VM3 is not on node-1 (node=${now_node:-none})"
pass "migration ${FAILED_ID:0:8} failed after the source agent restart; $VM3 on node-1"

echo "=== scenario 3.4: migrate $VM3 -> $NODE2 again ==="
otx vm migrate "$VM3" --node "$NODE2" --offline >/tmp/cold_source_restart_migrate2.out 2>&1 \
  || { cat /tmp/cold_source_restart_migrate2.out; fail "second vm migrate --offline request failed"; }
MIGRATION_ID="$(latest_migration_id "$VM3ID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ && "$MIGRATION_ID" != "$FAILED_ID" ]] \
  || fail "could not resolve the second migration id (got '${MIGRATION_ID:-none}')"
outcome="$(wait_migration_terminal "$MIGRATION_ID")"
[[ "$outcome" == "completed" ]] \
  || fail "migration to the node a failed migration left a copy on went '$outcome', want completed"
now_node="$(vm_node "$VM3")"
[[ "$now_node" == "$NODE2" ]] || fail "$VM3 not on node-2 after the second migration (node=${now_node:-none})"
ensure_running "$VM3"
[[ "$(vm_node "$VM3")" == "$NODE2" ]] || fail "$VM3 started off node-2 (node=$(vm_node "$VM3"))"
pass "second migration ${MIGRATION_ID:0:8} completed; $VM3 running on node-2"

echo "=== scenario 3.5: node-2 kept the failed migration's copy aside ==="
assert_abandoned_copy "$SMOKE_HANDLE_2" "$ABANDONED" "node-2 did not keep the failed migration's copy"
pass "the failed migration's copy sits at $ABANDONED on node-2"

trap - EXIT
cleanup
vm_listed "$VM1" && fail "$VM1 still listed after cleanup"
vm_listed "$VM2" && fail "$VM2 still listed after cleanup"
vm_listed "$VM3" && fail "$VM3 still listed after cleanup"
echo
echo "${GREEN}=== vm-migration-cold-stopped smoke PASSED ===${NC}"
