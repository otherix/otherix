#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM create-redelivery smoke - proves the audit R2-M1 + R2-M2 durable-dedup fix
# end to end against a REAL agent (qemu + QMP). This crosses the CP->agent
# boundary, so per the iteration discipline a mock-green run is necessary but NOT
# sufficient; this is the operator-run real-agent proof.
#
# THE BUG (pre-fix):
#   The CP worker drives vm.create with the agent_task_id resumption seam: POST,
#   persist the agent task id, then poll. If the CP failed to persist the
#   agent_task_id AND the agent then restarted, the redelivery re-POSTed the
#   create; the agent's create dedup was the IN-MEMORY createTasks map (empty
#   after restart) while m.vms was reloaded from disk, so the agent OVERWROTE the
#   live VM record and spawned a SECOND runCreate against the running guest's disk
#   and taps (R2-M1, destructive). Separately, the CP re-polled the vanished agent
#   task (404), retried 404 forever, and finalized the task `failed` even though
#   the VM was actually created (R2-M2, divergence).
#
# THE FIX:
#   M1b (agent): Manager.Create dedups on the DURABLE m.vms[vmID] (reloaded from
#   disk) before inserting, and NEVER overwrites - a redelivered create for an
#   already-running VM returns an idempotent SUCCESS task without re-spawning.
#   M2 (CP): runCreate/runDelete clear the persisted agent_task_id on a permanent
#   agent-task 404, so the redelivery re-POSTs and M1b reconciles (a completed VM
#   -> success), closing the false-failed-after-restart divergence.
#
# WHAT THIS SMOKE PROVES (single VM pinned to node-1, no network, SLIRP):
#   1. create -> running (qemu spawned, QMP responsive); record qemu PID + disk.
#   2. Model the post-restart redelivery FAITHFULLY:
#        a. restart the agent  -> its in-memory task store + createTasks map are
#           dropped (exactly the asymmetry that was the bug); m.vms reloads from
#           disk and the running qemu is re-adopted.
#        b. re-arm the CP's vm.create job for the SAME task/vm: clear the task's
#           AgentTaskID, reset its Status to pending, and re-enqueue a vm.create
#           job carrying the same {task_id, vm_id, pool_id, node_id}. The
#           dispatcher redelivers it, the executor re-POSTs (stable idem key,
#           AgentTaskID now nil), and the agent's durable dedup answers.
#   3. ASSERT the invariant: the VM is NOT duplicated/clobbered - the qemu PID is
#      UNCHANGED across the redelivery (no second runCreate replaced the process),
#      the disk file is intact, exactly one VM exists, AND the task converges to
#      `success` (reconciled), not stuck/false-failed.
#
# Because the QMP-only "running" state (qemu up + QMP answering) is reached in
# seconds even under the slow TCG emulation a dev Mac uses, this smoke needs NO
# booted guest - it never issues a graceful op. Budget CREATE_WAIT for the cold
# image fetch.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready with the cluster default pool reconciled. etcdctl on PATH (the
# embedded etcd dev member listens on 127.0.0.1:2379, no TLS).
#
# Usage: make smoke-vm-create-redelivery
#        (or: bash dev/smoke/vm-create-redelivery/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
VM="${VM_NAME:-vmcr-smoke}"                   # the single VM; delete-firsted
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds for a CP-projected status.phase
TASK_WAIT="${TASK_WAIT:-180}"                 # seconds for the redelivered task to reach success
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"          # embedded etcd dev client endpoint (no TLS)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# etcd_get KEY -> raw value at KEY ("" when absent)
etcd_get() { ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" get "$1" --print-value-only 2>/dev/null || true; }
# etcd_put KEY VALUE
etcd_put() { ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" put "$1" "$2" >/dev/null; }

# vm_phase NAME -> CP-observed status.phase ("" if gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# assert_phase NAME WANT [TIMEOUT]
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

# qemu_pid -> the agent-recorded qemu PID for the VM ("" if the pidfile is gone)
qemu_pid() { run_on "$SMOKE_HANDLE_1" sudo cat "$PIDF" 2>/dev/null | tr -dc '0-9' || true; }

# disk_exists -> "yes" when the per-VM disk file is present on the node
disk_exists() { run_on "$SMOKE_HANDLE_1" sudo test -f "$DISK" && echo yes || echo no; }

# restart_agent -> bounce the agent process so its IN-MEMORY task store +
# createTasks map are dropped (the post-restart redelivery condition); m.vms is
# reloaded from disk on the next New()/ScanState and the running qemu re-adopted.
restart_agent() {
  case "${SMOKE_PLATFORM}" in
    lima)  run_on "$SMOKE_HANDLE_1" sudo systemctl restart otherix-agent ;;
    netns) systemctl --user restart otherix-agent ;;
  esac
}

# wait_agent_ready -> block until node-1 reports ready again after the restart
wait_agent_ready() {
  local deadline; deadline=$(( SECONDS + 90 ))
  while (( SECONDS < deadline )); do
    [[ "$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)" == "ready" ]] \
      && { pass "agent ready again after restart"; return 0; }
    sleep 2
  done
  fail "agent did not return to ready within 90s after restart"
}

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-create-redelivery smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (it pokes the embedded etcd dev member to model the CP job redelivery)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
[[ -n "$(etcd_get /otherix/seq/jobs)" || -n "$(etcd_get /otherix)" ]] || true  # touch endpoint early; real check below
ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 \
  || fail "etcd not reachable at $ETCD_EP (the dev CP embeds it on 2379; run make local-dev-start)"
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s"
pass "CP up; $NODE1 ready; default pool ready; etcd reachable"

# --- step 1: create -> running -----------------------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of a stale leftover
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running

VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
DISK="$(smoke_state 1)/../pools/default/vms/${VMID}/disk.qcow2"   # see note below
PIDF="$(smoke_state 1)/vms/${VMID}/qemu.pid"
# The pool root layout differs per stack; resolve the disk from the agent meta if
# the convention above misses (best-effort - the PID assertion is the load-bearing one).
run_on "$SMOKE_HANDLE_1" sudo test -f "$DISK" || \
  DISK="$(run_on "$SMOKE_HANDLE_1" sudo find / -path '*/vms/'"${VMID}"'/disk.qcow2' 2>/dev/null | head -1 || true)"
info "VM id=$VMID  pidfile=$PIDF  disk=$DISK"

PID_BEFORE="$(qemu_pid)"; [[ "$PID_BEFORE" =~ ^[0-9]+$ ]] || fail "no qemu pid for running VM"
[[ "$(disk_exists)" == "yes" ]] || info "disk path not resolved (PID assertion remains load-bearing)"
pass "created and running (qemu pid=$PID_BEFORE)"

# Resolve the CP task that created this VM (its agent_task_id is what the bug
# failed to persist; the redelivery below re-arms its job).
TASK_ID="$(otx task list --output json 2>/dev/null \
  | jq -r --arg vm "$VMID" 'first(.data[]? | select(.resource_id==$vm and .type=="vm.create") | .id)' || true)"
[[ "$TASK_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the vm.create task id for $VMID (got '${TASK_ID:-none}')"
POOL_ID="$(otx pool get default --output json | jq -r --arg n "$NODE1" 'first(.instances[]? | select(.node==$n) | .id)')"
NODE_ID="$(otx node get "$NODE1" --output json | jq -r '.id')"
info "task=$TASK_ID pool=$POOL_ID node=$NODE_ID"

# --- step 2a: restart the agent (drop in-memory createTasks) ------------
echo "=== step 2a: restart agent (drops in-memory task store + createTasks) ==="
restart_agent
wait_agent_ready
# Re-adoption may re-stamp the pidfile; capture the post-restart pid as the new
# baseline (the running qemu is re-adopted, NOT re-spawned, so it persists).
sleep 3
PID_AFTER_RESTART="$(qemu_pid)"
[[ "$PID_AFTER_RESTART" =~ ^[0-9]+$ ]] || fail "no qemu pid after agent restart; the running VM was not re-adopted"
info "qemu pid after restart=$PID_AFTER_RESTART"

# --- step 2b: re-arm the CP vm.create job for the SAME task/vm ----------
echo "=== step 2b: re-arm the vm.create job (model the lost-ACK redelivery) ==="
TASK_KEY="/otherix/tasks/${TASK_ID}"
task_json="$(etcd_get "$TASK_KEY")"
[[ -n "$task_json" ]] || fail "task $TASK_ID not found in etcd at $TASK_KEY"
# Clear AgentTaskID (so the executor re-POSTs) and reset Status to pending (so
# UpdateTaskRunning transitions it again instead of treating it as committed).
new_task="$(jq -c '.AgentTaskID=null | .Status="pending" | .FinishedAt=null | .Result=null | .Error=null' <<<"$task_json")"
etcd_put "$TASK_KEY" "$new_task"
info "task re-armed: AgentTaskID cleared, Status=pending"

# Allocate the next job sequence (value-compare bump of the counter) and write a
# fresh pending vm.create job carrying the same args. Args is a Go []byte field,
# so it JSON-encodes as base64.
seq_raw="$(etcd_get /otherix/seq/jobs)"; [[ "$seq_raw" =~ ^[0-9]+$ ]] || seq_raw=0
next_seq=$(( seq_raw + 1 ))
etcd_put /otherix/seq/jobs "$next_seq"
JOB_KEY="$(printf '/otherix/jobs/%020d' "$next_seq")"
args_b64="$(jq -cn --arg t "$TASK_ID" --arg v "$VMID" --arg p "$POOL_ID" --arg n "$NODE_ID" \
  '{task_id:$t, vm_id:$v, pool_id:$p, node_id:$n}' | base64 | tr -d '\n')"
job_json="$(jq -cn --argjson id "$next_seq" --arg a "$args_b64" \
  '{id:$id, kind:"vm.create", args:$a, state:"pending", attempts:0}')"
etcd_put "$JOB_KEY" "$job_json"
pass "redelivery armed: job $JOB_KEY (seq $next_seq) re-enqueued for task $TASK_ID"

# --- step 3: assert no clobber + task reconciles to success ------------
echo "=== step 3: assert the redelivery did NOT clobber and the task succeeds ==="
# The dispatcher picks up the new pending job, the executor re-POSTs (AgentTaskID
# nil -> stable idem key), and the agent's durable dedup (m.vms[vmID] running)
# returns an idempotent SUCCESS without a second runCreate.
deadline=$(( SECONDS + TASK_WAIT )); status=""
while (( SECONDS < deadline )); do
  status="$(otx task get "$TASK_ID" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$status" == "success" ]] && break
  [[ "$status" == "failed" || "$status" == "cancelled" ]] && \
    fail "redelivered task reached '$status' (want success) - the create did NOT reconcile"
  sleep 3
done
[[ "$status" == "success" ]] || fail "redelivered task did not reach success within ${TASK_WAIT}s (got '${status:-none}')"
pass "redelivered task reconciled to success"

# The VM is still running and was never re-spawned: the qemu PID is UNCHANGED from
# the post-restart baseline (a second runCreate would have replaced the process).
assert_phase "$VM" running
PID_FINAL="$(qemu_pid)"
[[ "$PID_FINAL" =~ ^[0-9]+$ ]] || fail "no qemu pid after redelivery; the live VM was destroyed (CLOBBER)"
[[ "$PID_FINAL" == "$PID_AFTER_RESTART" ]] \
  || fail "qemu pid changed across the redelivery ($PID_AFTER_RESTART -> $PID_FINAL); a SECOND runCreate clobbered the live VM"
[[ "$(disk_exists)" != "no" ]] || fail "per-VM disk vanished across the redelivery (CLOBBER)"

# Exactly one VM with this name exists (the redelivery did not materialise a duplicate).
count="$(otx vm list --output json 2>/dev/null | jq --arg n "$VM" '[.data[]? | select(.name==$n)] | length' || echo 0)"
[[ "$count" == "1" ]] || fail "expected exactly 1 VM named $VM, found $count (duplicate materialised)"
pass "no clobber: qemu pid stable ($PID_FINAL), disk intact, single VM"

# --- step 4: cleanup ---------------------------------------------------
echo "=== step 4: delete ==="
otx vm delete "$VM" --wait --force --wait-timeout "${TASK_WAIT}s" || fail "vm delete failed"
deadline=$(( SECONDS + PHASE_WAIT ))
while (( SECONDS < deadline )); do
  otx vm get "$VM" --output json >/dev/null 2>&1 || { pass "$VM gone (404)"; break; }
  sleep 2
done

trap - EXIT
echo
echo "${GREEN}=== vm-create-redelivery smoke PASSED ===${NC}"
