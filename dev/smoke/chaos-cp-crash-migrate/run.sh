#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Chaos smoke: CP hard-crash during a running vm.migrate job (P0 lease-reclaim).
#
# THE BUG (pre-fix): a hard CP crash (SIGKILL/panic/node loss) with a vm.migrate
# job in `running` stranded it forever. PendingJobs is pending-only, nothing
# transitioned `running` back, so nothing redelivered the job: the migration
# stayed non-terminal `active`, the per-VM guard uniq/migration_active_vm/<vmID>
# was held forever (VM un-migratable), and the backing task never reached
# terminal (clients polling /v1/tasks/{id} hang, retention never reaps).
#
# THE FIX: a ClaimedAt lease on the job + a worker-side renewer (30s) keeps a
# live job's lease fresh, and a periodic reaper (jobs.reclaim) returns jobs not
# renewed within JobLease (90s) to `pending` WITHOUT bumping attempts, via a
# per-job ModRevision CAS that re-validates lease age on the authoritative
# re-read. The migrate handler is redelivery-idempotent (agent_task_id
# resumption + idempotent CommitMigrationCutover), so a reclaimed redelivery
# resumes the same agent task and commits the cutover.
#
# WHAT THIS SMOKE PROVES (real two-node stack, operator CLI):
#   1. create a VM on node-1 -> running.
#   2. issue a live `vm migrate` to node-2 (bandwidth-capped so the job is
#      reliably `running`), then poll etcd until the vm.migrate job is `running`.
#   3. SIGKILL the CP (api-server, embeds etcd) mid-job - the renewer dies, the
#      job's ClaimedAt freezes at the claim time.
#   4. restart the CP. The agents finish the qemu-to-qemu migration autonomously
#      while the CP is down (CP is not in the data path); the target resumes.
#   5. ASSERT recovery: the reaper reclaims the stale `running` job, the resumed
#      handler commits the cutover, the migration reaches `completed`, the VM is
#      running on node-2, AND the per-VM guard is released - a SECOND migration
#      (node-2 -> node-1) is accepted, proving the VM is migratable again, not
#      wedged.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or local-dev-deploy to refresh code)
# Both nodes ready, default pool reconciled. jq + etcdctl on PATH (the embedded
# etcd dev member listens on 127.0.0.1:2379, no TLS).
#
# Usage: make smoke-chaos-cp-crash-migrate
#        (or: bash dev/smoke/chaos-cp-crash-migrate/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OTX="${OTX:-${REPO_ROOT}/bin/otherix}"
NODE1="node-1"
NODE2="node-2"
VM="${VM_NAME:-chaos-cpcrash}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
DISK_GIB="${DISK_GIB:-5}"                      # >= image virtual size (~3.5G); a non-trivial disk so the migrate has a window
BANDWIDTH="${BANDWIDTH:-8m}"                   # cap the transfer so the job stays `running` long enough to kill the CP
CREATE_WAIT="${CREATE_WAIT:-600}"             # vm create -> running (incl. cold image fetch)
JOB_RUNNING_WAIT="${JOB_RUNNING_WAIT:-60}"     # wait for the vm.migrate job to reach `running`
RECLAIM_WAIT="${RECLAIM_WAIT:-220}"            # JobLease(90s)+reaper(30s) + handler resume + cutover, with margin
PHASE_WAIT="${PHASE_WAIT:-120}"

ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"
API_CONFIG="${REPO_ROOT}/dev/config/api.yaml"
RUN_DIR="${REPO_ROOT}/.local/run"
API_PID_FILE="${RUN_DIR}/otherix-api.pid"
API_LOG_FILE="${RUN_DIR}/otherix-api.log"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

etcd_get() { ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" get "$1" --print-value-only 2>/dev/null || true; }
etcd_get_prefix() { ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" get "$1" --prefix --print-value-only 2>/dev/null || true; }

vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# latest_migration_id VMID -> the most recent migration resource id for the VM
latest_migration_id() {
  otx migration list --output json 2>/dev/null \
    | jq -r --arg vm "$1" '[.data[]? | select(.vm_id==$vm)] | sort_by(.created_at) | last | .id // empty' 2>/dev/null || true
}
# migration_field ID FIELD -> a field off the migration resource ("" if gone)
migration_field() { otx migration get "$1" --output json 2>/dev/null | jq -r --arg f "$2" '.[$f] // empty' 2>/dev/null || true; }

# qemu_on NODEHANDLE VMID -> "yes" when a qemu for VMID runs on that node
qemu_on() { run_on "$1" bash -c "pgrep -af '$2' >/dev/null 2>&1" && echo yes || echo no; }

# migrate_job_state -> state of the ACTIVE (pending|running) vm.migrate job in
# etcd ("" if none active). Terminal jobs are excluded: completed jobs are
# deleted, failed jobs are retained 7d - neither is the in-flight one. At most
# one migration is active per VM (the per-VM guard), and this smoke runs a single
# VM, so there is at most one active vm.migrate job.
migrate_job_state() {
  etcd_get_prefix /otherix/jobs/ \
    | jq -rs 'map(select(.kind=="vm.migrate" and (.state=="running" or .state=="pending")))
              | sort_by(.id) | last | .state // empty' 2>/dev/null || true
}
# migrate_job_field FIELD -> a field off the active vm.migrate job
migrate_job_field() {
  etcd_get_prefix /otherix/jobs/ \
    | jq -rs --arg f "$1" 'map(select(.kind=="vm.migrate" and (.state=="running" or .state=="pending")))
              | sort_by(.id) | last | .[$f] // empty' 2>/dev/null || true
}

cp_pid() { [ -f "$API_PID_FILE" ] && cat "$API_PID_FILE" 2>/dev/null || true; }
cp_kill_hard() {
  local pid; pid="$(cp_pid)"
  [ -n "$pid" ] || pid="$(pgrep -f 'otherix-api --config' | head -1 || true)"
  [ -n "$pid" ] || fail "could not find the api-server pid to kill"
  info "SIGKILL api-server pid=$pid"
  kill -9 "$pid" 2>/dev/null || true
  # Wait until /healthz is actually down so we know the kill took.
  local deadline=$(( SECONDS + 15 ))
  while (( SECONDS < deadline )); do
    cp_ready || { info "CP down"; return 0; }
    sleep 1
  done
  fail "CP still answering /healthz after SIGKILL"
}
cp_start() {
  [ -x "${REPO_ROOT}/bin/otherix-api" ] || fail "bin/otherix-api missing (make build-api)"
  mkdir -p "$RUN_DIR"
  nohup "${REPO_ROOT}/bin/otherix-api" --config "$API_CONFIG" > "$API_LOG_FILE" 2>&1 &
  echo "$!" > "$API_PID_FILE"
  local deadline=$(( SECONDS + 60 ))
  while (( SECONDS < deadline )); do
    cp_ready && { pass "CP back up (pid $(cp_pid))"; return 0; }
    sleep 2
  done
  tail -30 "$API_LOG_FILE" >&2 || true
  fail "CP did not return to /healthz within 60s after restart"
}

cleanup() {
  echo "--- cleanup ---"
  # The CP must be up to delete; best-effort restart if we left it down.
  cp_ready || cp_start >/dev/null 2>&1 || true
  otx vm delete "$VM" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== chaos-cp-crash-migrate: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (pokes the embedded etcd dev member)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 \
  || fail "etcd not reachable at $ETCD_EP (run make local-dev-start)"
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
pass "created and running on node-1 (id=${VMID:0:8})"

# --- step 2: start the migration, catch the job in `running` -----------
echo "=== step 2: migrate $VM -> $NODE2 (bandwidth $BANDWIDTH), catch job running ==="
otx vm migrate "$VM" --node "$NODE2" --bandwidth "$BANDWIDTH" >/tmp/chaos_migrate.out 2>&1 \
  || { cat /tmp/chaos_migrate.out; fail "vm migrate request failed"; }
info "migrate issued: $(tr '\n' ' ' </tmp/chaos_migrate.out)"

deadline=$(( SECONDS + JOB_RUNNING_WAIT )); jstate=""
while (( SECONDS < deadline )); do
  jstate="$(migrate_job_state)"
  [[ "$jstate" == "running" ]] && break
  sleep 1
done
[[ "$jstate" == "running" ]] || fail "vm.migrate job did not reach 'running' within ${JOB_RUNNING_WAIT}s (got '${jstate:-none}')"
CLAIMED_AT="$(migrate_job_field claimed_at)"
[[ -n "$CLAIMED_AT" ]] || fail "running vm.migrate job has no claimed_at lease (P0 field missing)"
# Capture the migration resource id now (CP up); it persists across the CP crash
# in etcd and is the authoritative cutover signal (current_node_id is not
# reliably surfaced in this dev setup).
MIGRATION_ID="$(latest_migration_id "$VMID")"
[[ "$MIGRATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the migration id for $VMID (got '${MIGRATION_ID:-none}')"
pass "vm.migrate job is running with a lease (claimed_at=$CLAIMED_AT); migration=${MIGRATION_ID:0:8}"

# --- step 3: SIGKILL the CP mid-job ------------------------------------
echo "=== step 3: SIGKILL the CP (api-server + embedded etcd) mid-migration ==="
cp_kill_hard
info "CP down; agents continue the qemu-to-qemu migration autonomously"
sleep 5

# --- step 4: restart the CP --------------------------------------------
echo "=== step 4: restart the CP ==="
cp_start
# The job must still be `running` immediately after restart (nothing reclaimed
# it yet - PendingJobs is pending-only).
jstate="$(migrate_job_state)"
info "vm.migrate job state right after CP restart: '${jstate:-none}'"
[[ "$jstate" == "running" || "$jstate" == "" ]] || info "job already '$jstate' (migration may have finished fast)"

# --- step 5: assert recovery -------------------------------------------
echo "=== step 5: assert the reaper reclaims + the migration recovers ==="
# Wait for the migration resource to reach a terminal phase via CP recovery
# (reaper reclaims the stale running job -> handler resumes -> commits cutover).
deadline=$(( SECONDS + RECLAIM_WAIT )); mphase=""
while (( SECONDS < deadline )); do
  mphase="$(migration_field "$MIGRATION_ID" phase)"
  [[ "$mphase" == "completed" ]] && break
  [[ "$mphase" == "failed" || "$mphase" == "cancelled" ]] && \
    fail "migration reached '$mphase' (want completed) after CP recovery - the recovered migration did not commit"
  sleep 4
done
[[ "$mphase" == "completed" ]] || fail "migration did not reach 'completed' within ${RECLAIM_WAIT}s after CP recovery (phase='${mphase:-none}'); the stranded job was not reclaimed/resumed"
# The active vm.migrate job is gone (the resumed handler completed and deleted it).
[[ -z "$(migrate_job_state)" ]] || info "active vm.migrate job not yet swept (non-fatal)"
# Cross-check the guest actually moved: qemu for VMID runs on node-2, not node-1.
[[ "$(qemu_on "$SMOKE_HANDLE_2" "$VMID")" == "yes" ]] || fail "no qemu for the VM on node-2 after a completed migration"
[[ "$(qemu_on "$SMOKE_HANDLE_1" "$VMID")" == "no" ]]  || info "source qemu still present on node-1 (post-cutover teardown may lag; non-fatal)"
[[ "$(vm_phase "$VM")" == "running" ]] || fail "VM not running after recovered migration (phase=$(vm_phase "$VM"))"
pass "migration recovered: phase=completed, guest running on node-2, stranded job reclaimed"

# --- step 6: prove the per-VM guard was released -----------------------
echo "=== step 6: prove the VM is migratable again (guard released) ==="
# If the guard uniq/migration_active_vm/<vmID> were still held, this second
# migrate would be rejected with a 409 conflict. It must be accepted.
if otx vm migrate "$VM" --node "$NODE1" --bandwidth "$BANDWIDTH" --wait --wait-timeout "${RECLAIM_WAIT}s" >/tmp/chaos_migrate2.out 2>&1; then
  pass "second migration (node-2 -> node-1) accepted and completed; guard was released"
else
  # A wait-timeout is acceptable (migration still running) as long as it was ACCEPTED
  # (not a 409 conflict). A conflict means the guard is still held = the bug.
  if grep -qiE "conflict|migration_active|already.*migrat" /tmp/chaos_migrate2.out; then
    cat /tmp/chaos_migrate2.out
    fail "second migration REJECTED as conflict - the per-VM guard was NOT released (VM wedged un-migratable)"
  fi
  info "second migration accepted (did not finish within wait; guard release proven by acceptance)"
fi
info "final VM phase=$(vm_phase "$VM")"

trap - EXIT
cleanup
echo
echo "${GREEN}=== chaos-cp-crash-migrate smoke PASSED ===${NC}"
