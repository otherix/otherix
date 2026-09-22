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
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or local-dev-deploy to refresh code)
# All three nodes ready, default pool reconciled. jq on PATH.
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
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
DISK_GIB="${DISK_GIB:-4}"
# A larger disk lengthens the source-side push, widening the window in which the
# target agent restart lands while node-2 still serves the disk.
RESTART_DISK_GIB="${RESTART_DISK_GIB:-10}"
CREATE_WAIT="${CREATE_WAIT:-600}"
NBD_WAIT="${NBD_WAIT:-120}"
CONVERGE_WAIT="${CONVERGE_WAIT:-60}"
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"
READY_WAIT="${READY_WAIT:-120}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }
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

# wait_no_nbd HANDLE MIGRATION_ID FAIL_MESSAGE -> poll until the node runs no
# qemu-nbd for the migration; fail with the surviving pids on timeout.
wait_no_nbd() {
  local deadline=$(( SECONDS + CONVERGE_WAIT )) pids=""
  while (( SECONDS < deadline )); do
    pids="$(nbd_pids_on "$1" "$2")"
    [[ -z "$pids" ]] && return 0
    sleep 2
  done
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

# ensure_running VM -> start the VM if it is not running; fail if it will not.
ensure_running() {
  if [[ "$(vm_phase "$1")" != "running" ]]; then
    otx vm start "$1" --wait --wait-timeout 180s >/tmp/cold_stopped_start.out 2>&1 \
      || { cat /tmp/cold_stopped_start.out; fail "$1 does not start (phase=$(vm_phase "$1"))"; }
  fi
  [[ "$(vm_phase "$1")" == "running" ]] || fail "$1 not running after start (phase=$(vm_phase "$1"))"
}

cleanup() {
  echo "--- cleanup ---"
  [[ -n "${MIGRATION_ID:-}" ]] && otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || true
  otx vm delete "$VM1" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  otx vm delete "$VM2" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-cold-stopped: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build)"
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
deadline=$(( SECONDS + NBD_WAIT )); nbd_pid=""
while (( SECONDS < deadline )); do
  nbd_pid="$(nbd_pids_on "$SMOKE_HANDLE_2" "$MIGRATION_ID" | head -1)"
  [[ "$nbd_pid" =~ ^[0-9]+$ ]] && break
  ph="$(migration_phase "$MIGRATION_ID")"
  case "$ph" in
    completed) fail "migration completed before its qemu-nbd was ever observed (too fast; raise RESTART_DISK_GIB)" ;;
    failed|cancelled) fail "migration went '$ph' before the agent restart" ;;
  esac
  sleep 0.5
done
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

trap - EXIT
cleanup
vm_listed "$VM1" && fail "$VM1 still listed after cleanup"
vm_listed "$VM2" && fail "$VM2 still listed after cleanup"
echo
echo "${GREEN}=== vm-migration-cold-stopped smoke PASSED ===${NC}"
