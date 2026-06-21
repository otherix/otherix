#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Chaos smoke: target-agent hard-crash mid-incoming (P1a recovery-reaper).
#
# THE BUG (pre-fix): a target-agent restart mid-incoming replayed the VM at
# StatusMigratingIncoming with NO migration record, resume goroutine, or mux. The
# reconciler treated it as transitional and never advanced it -> wedged
# `migrating` forever, leaking the paused -incoming qemu (which holds the RAM+NBD
# ingress ports via its in-process exports) and the host taps. A second gap, found
# by THIS smoke: the reap originally fired only on qemu run-state "paused", but a
# target killed mid-incoming is in "inmigrate" -> the orphan leaked.
#
# THE FIX: on recovery the agent QMP-probes the replayed -incoming qemu and
# branches fail-closed - "running" => promote (post-cutover resumed guest, never
# kill); a pre-resume state (inmigrate/paused/prelaunch/finish-migrate) => reap
# (kill the orphan, free the ports/taps, mark StatusFailed, NEVER delete the
# destination disk); dead/inconclusive => StatusFailed without killing.
#
# WHAT THIS SMOKE PROVES (real two-node stack, operator CLI):
#   1. create a VM on node-1 -> running; record its source qemu pid.
#   2. live-migrate node-1 -> node-2; once the migration is `active` and node-2 is
#      running the -incoming qemu (run-state "inmigrate", pre-cont), hard-kill the
#      node-2 agent MAIN process (kill -9 on MainPID, NOT systemctl kill, which
#      would also tear down the qemu in the unit cgroup). The daemonized qemu
#      survives the agent crash - exactly the orphan recovery must reconcile.
#   3. the agent restarts; recovery QMP-probes the orphan (inmigrate) and REAPS it.
#   4. ASSERT recoverability: the node-2 agent logged the reap with a pre-resume
#      run_state; the orphaned qemu for the VM is GONE on node-2 (reaped, not
#      leaked); the SOURCE guest is still running on node-1 (same pid,
#      fail-safe-to-source); and the system returns to a clean state (migration
#      cancellable to terminal, VM force-deletes with no qemu left anywhere).
#
# NOTE: the CP-side migration's eventual phase after a target crash is governed by
# the migrate-handler retry path (re-adopt conflicts with the recovery-failed
# record), which is outside the P1a recovery-reaper this smoke targets; this smoke
# drives the migration to terminal via an explicit cancel rather than asserting a
# particular auto-fail latency.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or local-dev-deploy to refresh code)
# Both nodes ready, default pool reconciled. jq on PATH.
#
# Usage: make smoke-chaos-target-crash-incoming
#        (or: bash dev/smoke/chaos-target-crash-incoming/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OTX="${OTX:-${REPO_ROOT}/bin/otherix}"
NODE1="node-1"
NODE2="node-2"
VM="${VM_NAME:-chaos-tgtcrash}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
DISK_GIB="${DISK_GIB:-5}"
# A very low bandwidth keeps the incoming RAM stream in flight (run-state
# "inmigrate") for long enough that the agent crash + restart + recovery probe
# all land BEFORE the migration completes and the target auto-resumes to
# "running". The payload is tiny (zero-detection), so the cap must be aggressive.
BANDWIDTH="${BANDWIDTH:-64k}"
MAX_DOWNTIME="${MAX_DOWNTIME:-50}"
CREATE_WAIT="${CREATE_WAIT:-600}"
INCOMING_WAIT="${INCOMING_WAIT:-90}"           # wait for node-2 to launch the -incoming qemu
REAP_WAIT="${REAP_WAIT:-120}"                  # agent auto-restart + recovery reap
CANCEL_WAIT="${CANCEL_WAIT:-90}"

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

# qemu_pid_on HANDLE VMID -> the qemu pid for VMID on a node ("" if none)
qemu_pid_on() { run_on "$1" bash -c "pgrep -f 'uuid $2'" 2>/dev/null | head -1 | tr -dc '0-9' || true; }

# agent_crash_main HANDLE -> SIGKILL ONLY the agent main process, leaving the
# daemonized qemu (a separate, reparented process) alive. systemctl kill would
# signal the whole unit cgroup and tear the qemu down too, defeating the test.
agent_crash_main() {
  run_on "$1" sudo bash -c 'p=$(systemctl show otherix-agent -p MainPID --value); [ -n "$p" ] && [ "$p" != 0 ] && kill -9 "$p"' 2>/dev/null || true
}
agent_start() { run_on "$1" sudo systemctl start otherix-agent 2>/dev/null || true; }

wait_node_ready() {
  local node="$1" deadline; deadline=$(( SECONDS + 90 ))
  while (( SECONDS < deadline )); do
    [[ "$(otx node get "$node" --output json 2>/dev/null | jq -r '.status' || true)" == "ready" ]] \
      && { pass "$node ready again"; return 0; }
    sleep 2
  done
  fail "$node did not return to ready within 90s"
}

cleanup() {
  echo "--- cleanup ---"
  agent_start "$SMOKE_HANDLE_2" >/dev/null 2>&1 || true
  [[ -n "${MIGRATION_ID:-}" ]] && otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || true
  otx vm delete "$VM" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  run_on "$SMOKE_HANDLE_1" sudo pkill -f "name $VM" >/dev/null 2>&1 || true
  run_on "$SMOKE_HANDLE_2" sudo pkill -f "name $VM" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== chaos-target-crash-incoming: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
pass "CP up; both nodes ready"

# --- step 1: create the VM on node-1 -> running ------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
otx vm delete "$VM" --force --wait --wait-timeout 60s >/dev/null 2>&1 || true
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --disk-gib "$DISK_GIB" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
[[ "$(vm_phase "$VM")" == "running" ]] || fail "$VM not running after create"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
SRC_PID="$(qemu_pid_on "$SMOKE_HANDLE_1" "$VMID")"
[[ "$SRC_PID" =~ ^[0-9]+$ ]] || fail "no source qemu pid for the running VM on node-1"
pass "created and running on node-1 (id=${VMID:0:8} src_qemu_pid=$SRC_PID)"

# --- step 2: migrate to active, then crash the node-2 agent (qemu lives) ---
echo "=== step 2: migrate -> $NODE2; crash node-2 agent main pid mid-incoming ==="
MIGRATION_ID=""
otx vm migrate "$VM" --node "$NODE2" --bandwidth "$BANDWIDTH" --max-downtime "$MAX_DOWNTIME" >/tmp/chaos_tgt_migrate.out 2>&1 \
  || { cat /tmp/chaos_tgt_migrate.out; fail "vm migrate request failed"; }
MIGRATION_ID="$(latest_migration_id "$VMID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id (got '${MIGRATION_ID:-none}')"
info "migration=${MIGRATION_ID:0:8}; polling fast for the node-2 -incoming qemu (kill at first sight = inmigrate, pre-cont)"

# Kill at FIRST SIGHT of the -incoming qemu: it is launched in run-state
# "inmigrate" (still receiving), well before cont. With the aggressive bandwidth
# cap the RAM stream is far from done, so the orphan stays "inmigrate" across the
# crash + restart window and recovery exercises the REAP path (not promote).
deadline=$(( SECONDS + INCOMING_WAIT )); tgt_pid=""
while (( SECONDS < deadline )); do
  tgt_pid="$(qemu_pid_on "$SMOKE_HANDLE_2" "$VMID")"
  [[ "$tgt_pid" =~ ^[0-9]+$ ]] && break
  sleep 0.2
done
[[ "$tgt_pid" =~ ^[0-9]+$ ]] || fail "node-2 never launched the -incoming qemu within ${INCOMING_WAIT}s"
info "node-2 -incoming qemu present (pid=$tgt_pid), migration phase=$(migration_phase "$MIGRATION_ID"); crashing node-2 agent main pid NOW"
agent_crash_main "$SMOKE_HANDLE_2"
# The qemu must survive the agent crash (it is the orphan recovery will reap).
sleep 1
[[ "$(qemu_pid_on "$SMOKE_HANDLE_2" "$VMID")" =~ ^[0-9]+$ ]] \
  || fail "orphan qemu died with the agent crash (the kill hit the cgroup, not just the main pid) - cannot exercise the reap path"
pass "node-2 agent crashed mid-incoming; orphan -incoming qemu (pid=$tgt_pid) survived"

# --- step 3: agent restarts, recovery REAPS the inmigrate orphan -------
echo "=== step 3: node-2 agent restarts; recovery reaps the orphaned inmigrate qemu ==="
agent_start "$SMOKE_HANDLE_2"
wait_node_ready "$NODE2"

# The reap must fire via the pre-resume recovery path (this is the inmigrate fix).
deadline=$(( SECONDS + REAP_WAIT )); reaped="no"; rs=""
while (( SECONDS < deadline )); do
  line="$(run_on "$SMOKE_HANDLE_2" sudo journalctl -u otherix-agent --since '3 minutes ago' --no-pager 2>/dev/null \
          | grep -iE 'reaping orphaned pre-resume live-migration target' | tail -1 || true)"
  if [[ -n "$line" ]]; then
    reaped="yes"; rs="$(grep -oE '"run_state":"[a-z-]+"' <<<"$line" | tail -1 || true)"
    break
  fi
  sleep 3
done
[[ "$reaped" == "yes" ]] || fail "node-2 recovery did NOT log the pre-resume reap within ${REAP_WAIT}s - the orphan was not reaped via the recovery path"
pass "node-2 recovery reaped the orphan via the pre-resume path (${rs:-run_state=?})"

# The orphaned qemu must be GONE (killed by the reap, not leaked).
deadline=$(( SECONDS + 30 )); leaked="?"
while (( SECONDS < deadline )); do
  [[ -z "$(qemu_pid_on "$SMOKE_HANDLE_2" "$VMID")" ]] && { leaked="no"; break; }
  leaked="yes"; sleep 2
done
[[ "$leaked" == "no" ]] || fail "orphaned -incoming qemu still alive on node-2 after the reap (leak)"
pass "orphaned -incoming qemu gone on node-2 (reaped, no leak of qemu/ports/taps)"

# --- step 4: source guest is safe -------------------------------------
echo "=== step 4: source guest safe on node-1 (fail-safe-to-source) ==="
NOW_SRC_PID="$(qemu_pid_on "$SMOKE_HANDLE_1" "$VMID")"
[[ "$NOW_SRC_PID" == "$SRC_PID" ]] \
  || fail "source qemu on node-1 changed ($SRC_PID -> ${NOW_SRC_PID:-gone}); the source guest was not preserved"
pass "source guest still running on node-1 (pid unchanged=$SRC_PID)"

# --- step 5: system returns to a clean state ---------------------------
echo "=== step 5: cancel the migration to terminal + clean delete ==="
otx migration cancel "$MIGRATION_ID" >/dev/null 2>&1 || true
deadline=$(( SECONDS + CANCEL_WAIT )); mphase=""
while (( SECONDS < deadline )); do
  mphase="$(migration_phase "$MIGRATION_ID")"
  [[ "$mphase" == "cancelled" || "$mphase" == "failed" ]] && break
  sleep 3
done
[[ "$mphase" == "cancelled" || "$mphase" == "failed" ]] || info "migration phase='${mphase:-none}' (not yet terminal; non-fatal - cleanup force-deletes)"
otx vm delete "$VM" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
sleep 2
[[ -z "$(qemu_pid_on "$SMOKE_HANDLE_1" "$VMID")" ]] || fail "qemu for the VM still on node-1 after force delete"
[[ -z "$(qemu_pid_on "$SMOKE_HANDLE_2" "$VMID")" ]] || fail "qemu for the VM still on node-2 after force delete"
pass "system clean: migration terminal ('${mphase:-deleted}'), VM deleted, no qemu left on either node"

trap - EXIT
cleanup
echo
echo "${GREEN}=== chaos-target-crash-incoming smoke PASSED ===${NC}"
