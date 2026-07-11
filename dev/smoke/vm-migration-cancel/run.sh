#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Smoke: a CP-side `migration cancel` propagates to the agents (source aborts,
# target reaps) PROMPTLY, instead of leaving the migration running on the agents
# until their 30-minute timeout.
#
# THE GAP (pre-fix): `POST /v1/migrations/{id}/cancel` only marked the migration
# record cancelled. The source agent kept mirroring (its outgoing task stayed
# running, the CP worker stayed blocked in PollTask), and the target's -incoming
# qemu leaked until the agent's incoming timeout. The migration record said
# cancelled while the agents kept working - a control/observed divergence.
#
# THE FIX: the cancel handler best-effort tells the SOURCE agent to abort its
# outgoing push (guest stays running, fail-safe-to-source) and the live TARGET to
# reap its incoming setup. The source abort flips its outgoing task terminal,
# unblocking the worker, which finalizes the backing task `cancelled` and reaps
# the target.
#
# WHAT THIS SMOKE PROVES (real two-node stack, operator CLI):
#   1. create a VM on node-1 -> running; record its source qemu pid.
#   2. live-migrate to node-2 (bandwidth-capped); once active + node-2 is running
#      the -incoming qemu, `otherix migration cancel`.
#   3. ASSERT promptly (NOT waiting for any agent timeout): migration -> cancelled;
#      the SOURCE guest still runs on node-1 (same pid, fail-safe); the target
#      -incoming qemu is GONE on node-2 (reaped by propagation); the backing task
#      reaches `cancelled` (not failed); and the VM is migratable again.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or local-dev-deploy to refresh code)
# Both nodes ready, default pool reconciled. jq + etcdctl on PATH (etcd dev member
# on 127.0.0.1:2379, no TLS - used to read the backing task, which has no CLI).
#
# Usage: make smoke-vm-migration-cancel
#        (or: bash dev/smoke/vm-migration-cancel/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OTX="${OTX:-${REPO_ROOT}/bin/otherix}"
NODE1="node-1"
NODE2="node-2"
VM="${VM_NAME:-cancel-prop}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
DISK_GIB="${DISK_GIB:-5}"
BANDWIDTH="${BANDWIDTH:-64k}"                   # slow so the migration is firmly active/incoming at cancel time
MAX_DOWNTIME="${MAX_DOWNTIME:-50}"
CREATE_WAIT="${CREATE_WAIT:-600}"
ACTIVE_WAIT="${ACTIVE_WAIT:-90}"
# The propagation must converge in SECONDS (no agent timeout). A generous bound
# that is still FAR below the agent's 30-minute incoming timeout backstop.
CONVERGE_WAIT="${CONVERGE_WAIT:-60}"
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }
latest_migration_id() {
  otx migration list --output json 2>/dev/null \
    | jq -r --arg vm "$1" '[.data[]? | select(.vm_id==$vm)] | sort_by(.created_at) | last | .id // empty' 2>/dev/null || true
}
migration_phase() { otx migration get "$1" --output json 2>/dev/null | jq -r '.phase // empty' 2>/dev/null || true; }
qemu_pid_on() { run_on "$1" bash -c "pgrep -f 'uuid $2'" 2>/dev/null | head -1 | tr -dc '0-9' || true; }

# task_status_for MIGRATION_ID -> the backing vm.migrate task's Status (the task
# has no CLI surface; read it from the dev etcd member, PascalCase keys). A
# vm.migrate task's ResourceType is "migration" and ResourceID is the MIGRATION
# id (migrations are a first-class resource), not the VM id.
task_status_for() {
  ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" get /otherix/tasks/ --prefix --print-value-only 2>/dev/null \
    | jq -rs --arg mid "$1" 'map(select(.Type=="vm.migrate" and .ResourceID==$mid)) | sort_by(.CreatedAt) | last | .Status // empty' 2>/dev/null || true
}

cleanup() {
  echo "--- cleanup ---"
  [[ -n "${MIGRATION_ID:-}" ]] && otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || true
  otx vm delete "$VM" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  run_on "$SMOKE_HANDLE_2" sudo pkill -f "name $VM" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-cancel: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (reads the backing task; no task CLI)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 || fail "etcd not reachable at $ETCD_EP"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
pass "CP up; both nodes ready; etcd reachable"

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
SRC_PID="$(qemu_pid_on "$SMOKE_HANDLE_1" "$VMID")"
[[ "$SRC_PID" =~ ^[0-9]+$ ]] || fail "no source qemu pid on node-1"
pass "created and running on node-1 (id=${VMID:0:8} src_qemu_pid=$SRC_PID)"

# --- step 2: migrate to active + incoming, then cancel -----------------
echo "=== step 2: migrate -> $NODE2; cancel once active + node-2 -incoming qemu present ==="
MIGRATION_ID=""
otx vm migrate "$VM" --node "$NODE2" --bandwidth "$BANDWIDTH" --max-downtime "$MAX_DOWNTIME" >/tmp/cancel_prop_migrate.out 2>&1 \
  || { cat /tmp/cancel_prop_migrate.out; fail "vm migrate request failed"; }
MIGRATION_ID="$(latest_migration_id "$VMID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"

deadline=$(( SECONDS + ACTIVE_WAIT )); tgt_pid=""
while (( SECONDS < deadline )); do
  tgt_pid="$(qemu_pid_on "$SMOKE_HANDLE_2" "$VMID")"
  ph="$(migration_phase "$MIGRATION_ID")"
  [[ "$tgt_pid" =~ ^[0-9]+$ && "$ph" == "active" ]] && break
  [[ "$ph" == "completed" ]] && fail "migration completed before cancel (too fast; lower BANDWIDTH)"
  sleep 0.5
done
[[ "$tgt_pid" =~ ^[0-9]+$ ]] || fail "node-2 never launched the -incoming qemu within ${ACTIVE_WAIT}s"
info "migration active; node-2 -incoming qemu pid=$tgt_pid; issuing cancel"
otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || fail "migration cancel request failed"
[[ "$(migration_phase "$MIGRATION_ID")" == "cancelled" ]] || fail "migration not cancelled right after cancel (got '$(migration_phase "$MIGRATION_ID")')"
pass "migration cancelled (CP authoritative)"

# --- step 3: propagation converged PROMPTLY ----------------------------
echo "=== step 3: assert the agents tore down promptly (no 30-min timeout) ==="
# Target incoming qemu reaped on node-2.
deadline=$(( SECONDS + CONVERGE_WAIT )); leaked="?"
while (( SECONDS < deadline )); do
  [[ -z "$(qemu_pid_on "$SMOKE_HANDLE_2" "$VMID")" ]] && { leaked="no"; break; }
  leaked="yes"; sleep 2
done
[[ "$leaked" == "no" ]] || fail "target -incoming qemu still alive on node-2 ${CONVERGE_WAIT}s after cancel - propagation did NOT reap it"
pass "target -incoming qemu reaped on node-2 promptly"

# Backing task finalized cancelled (not failed, not stuck running).
deadline=$(( SECONDS + CONVERGE_WAIT )); tstatus=""
while (( SECONDS < deadline )); do
  tstatus="$(task_status_for "$MIGRATION_ID")"
  [[ "$tstatus" == "cancelled" ]] && break
  [[ "$tstatus" == "failed" ]] && fail "backing task finalized 'failed' after a user cancel - want 'cancelled'"
  sleep 2
done
[[ "$tstatus" == "cancelled" ]] || fail "backing task did not finalize 'cancelled' within ${CONVERGE_WAIT}s (got '${tstatus:-none}')"
pass "backing vm.migrate task finalized 'cancelled'"

# --- step 4: source guest is safe + VM usable --------------------------
echo "=== step 4: source guest safe on node-1; VM migratable again ==="
NOW_SRC_PID="$(qemu_pid_on "$SMOKE_HANDLE_1" "$VMID")"
[[ "$NOW_SRC_PID" == "$SRC_PID" ]] \
  || fail "source qemu on node-1 changed ($SRC_PID -> ${NOW_SRC_PID:-gone}); the source guest was not preserved"
pass "source guest still running on node-1 (pid unchanged=$SRC_PID, fail-safe-to-source)"
[[ "$(vm_phase "$VM")" == "running" ]] || fail "VM not running after the cancelled migration (phase=$(vm_phase "$VM"))"

# A fresh migration is accepted (the guard was released by the terminal cancel).
if otx vm migrate "$VM" --node "$NODE2" --bandwidth "8m" --wait --wait-timeout "180s" >/tmp/cancel_prop_migrate2.out 2>&1; then
  pass "re-migration accepted and completed; guard released after cancel"
else
  if grep -qiE "conflict|migration_active|already.*migrat" /tmp/cancel_prop_migrate2.out; then
    cat /tmp/cancel_prop_migrate2.out
    fail "re-migration REJECTED as conflict - the per-VM guard was NOT released by the cancel"
  fi
  m2="$(latest_migration_id "$VMID")"; ph="$(migration_phase "$m2")"
  [[ "$ph" == "completed" ]] || fail "re-migration did not complete (phase='${ph:-none}')"
  pass "re-migration completed (phase=$ph); guard released"
fi

trap - EXIT
cleanup
echo
echo "${GREEN}=== vm-migration-cancel smoke PASSED ===${NC}"
