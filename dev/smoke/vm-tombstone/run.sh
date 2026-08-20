#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM-tombstone smoke - proves that a VM the control plane has deleted while its
# node was unreachable is actually torn down on that node once the agent comes
# back, and STAYS torn down across restarts.
#
# WHY A REAL AGENT: the CP -> agent teardown signal rides the heartbeat response
# and is consumed by the agent's VM reconciler, which drives qemu and the on-disk
# state directory. A mock agent cannot show the qemu process dying, the state
# directory going away, or - the load-bearing part - that the agent no longer
# replays the VM from <state>/vms/<uuid>/meta.json on the NEXT start.
#
# WHAT IT PROVES
#
#   scenario 1 (the ordinary case):
#     1. create a VM on node-1 and wait for running (qemu up, QMP answering);
#     2. stop node-1's agent and wait past the CP's heartbeat-staleness window,
#        so the delete worker treats the node as not serviceable;
#     3. `otherix vm delete` -> the task reaches success WITHOUT the agent (it is
#        stopped; nothing could have answered), leaving a soft-deleted VM row and
#        a guest still running on the node;
#     4. start the agent: it replays the VM from meta.json, reports it, the CP
#        answers with a teardown signal, and the reconciler tears the VM down -
#        no qemu for that uuid, no state dir, no pool disk dir;
#     5. start the agent AGAIN and re-assert: nothing replays, because the state
#        the replay reads is gone. Without this step the smoke proves very
#        little, since the agent rebuilds its VM registry from meta.json on
#        every start;
#     6. the signal STOPS: once the node no longer reports the VM the CP must
#        stop asking for a teardown, so the api log gains no new teardown line
#        over several heartbeats.
#
#   scenario 2 (crash part-way through a teardown):
#     the same flow, except the VM's meta.json is left saying `deleting` while
#     the agent is down - the state an agent that crashed mid-teardown leaves
#     behind. The replayed VM must be re-driven to completion: the real qemu is
#     verified and killed, and the disk and state directory are removed.
#
# NOT COVERED HERE: a teardown signal arriving while the VM is live-migrating.
# The reconciler defers on an active migration record, but that record lives in
# the agent's memory for the seconds a cutover takes, and staging a deleted VM
# row against that window on a real stack is not reproducible enough to assert
# on. It is covered by the reconciler unit tests instead.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready with the cluster default pool reconciled.
#
# Usage: make smoke-vm-tombstone
#        (or: bash dev/smoke/vm-tombstone/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
VM_LIVE="${VM_LIVE:-vmtomb-live}"        # scenario 1: replayed as running
VM_CRASH="${VM_CRASH:-vmtomb-crash}"     # scenario 2: replayed as deleting
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"        # vm create -> running, incl. a cold image fetch
DELETE_WAIT="${DELETE_WAIT:-180}"        # vm delete task -> terminal
STALE_WAIT="${STALE_WAIT:-45}"           # > api.yaml workers.heartbeat.rebalance_grace (30s)
READY_WAIT="${READY_WAIT:-120}"          # agent restart -> node ready again
TEARDOWN_WAIT="${TEARDOWN_WAIT:-180}"    # teardown signal -> qemu + state gone
QUIET_WAIT="${QUIET_WAIT:-45}"           # window over which the signal must stay silent
API_LOG="${API_LOG:-.local/run/otherix-api.log}"

STATE1="$(smoke_state 1)"
H1="${SMOKE_HANDLE_1}"

# Set to yes once a created VM's pool disk dir has actually been observed on the
# node; see torn_down.
POOL_SEEN="no"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# qemu_count UUID -> number of qemu processes on node-1 whose cmdline names it.
# The match runs HERE, not on the node: a remote `sh -c 'pgrep ... | grep <uuid>'`
# also matches that wrapper shell's own command line and over-counts by one.
qemu_count() {
  run_on "$H1" pgrep -a qemu-system 2>/dev/null | grep -c -- "$1" || true
}

# dir_exists PATH -> yes|no, for a path on node-1
dir_exists() {
  run_on "$H1" sudo test -d "$1" >/dev/null 2>&1 && echo yes || echo no
}

# state_dir / pool_dir UUID -> the two on-node directories a teardown removes
state_dir() { printf '%s/vms/%s' "$STATE1" "$1"; }
pool_dir()  { printf '/var/lib/otherix/pools/default/vms/%s' "$1"; }

agent_stop()  { run_on "$H1" sudo systemctl stop otherix-agent; }
agent_start() { run_on "$H1" sudo systemctl start otherix-agent; }

# wait_node_ready - block until the CP sees node-1 heartbeating again
wait_node_ready() {
  local deadline; deadline=$(( SECONDS + READY_WAIT ))
  while (( SECONDS < deadline )); do
    [[ "$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)" == "ready" ]] \
      && { pass "$NODE1 ready again"; return 0; }
    sleep 3
  done
  fail "$NODE1 did not return to ready within ${READY_WAIT}s"
}

# torn_down UUID -> 0 when no qemu, no state dir and - when the pool disk dir was
# visible from here in the first place - no pool disk dir remain.
#
# Every leg below reports "gone" when the channel to the node breaks: a failed
# `run_on` yields no qemu lines and a failing `test -d`. So the predicate starts
# with a path that is always there, and a broken channel FAILS the assertion
# instead of satisfying it - without that probe one flaky invocation is a green
# run of the very step this smoke exists for.
torn_down() {
  [[ "$(dir_exists "$STATE1")" == "yes" ]] || return 1
  [[ "$(qemu_count "$1")" == "0" ]] || return 1
  [[ "$(dir_exists "$(state_dir "$1")")" == "no" ]] || return 1
  # The pool root is a private mount on the netns stack, where run_on enters only
  # the netns and never sees it. Requiring a dir that was never observable would
  # assert nothing, so this leg counts only when the create step saw it.
  if [[ "$POOL_SEEN" == "yes" ]]; then
    [[ "$(dir_exists "$(pool_dir "$1")")" == "no" ]] || return 1
  fi
  return 0
}

# assert_torn_down UUID LABEL [TIMEOUT] - wait for the teardown, then report what
# is still standing if it never happened.
assert_torn_down() {
  local id="$1" label="$2" to="${3:-$TEARDOWN_WAIT}" deadline
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    torn_down "$id" && { pass "$label: qemu, state dir and pool disk dir all gone"; return 0; }
    sleep 3
  done
  fail "$label: not torn down within ${to}s (qemu=$(qemu_count "$id") state_dir=$(dir_exists "$(state_dir "$id")") pool_dir=$(dir_exists "$(pool_dir "$id")"))"
}

# assert_still_gone UUID LABEL - a single immediate check, no waiting: nothing
# may have come back.
assert_still_gone() {
  local id="$1" label="$2"
  torn_down "$id" \
    || fail "$label: the vm came back (qemu=$(qemu_count "$id") state_dir=$(dir_exists "$(state_dir "$id")") pool_dir=$(dir_exists "$(pool_dir "$id")"))"
  pass "$label: still gone"
}

# tombstone_signals UUID -> how many times the CP has asked node-1 to tear this
# vm down. The api log is json lines; one line per heartbeat that carries it.
tombstone_signals() {
  grep "signalling teardown of a deleted vm the node still holds" "$API_LOG" 2>/dev/null \
    | grep -c -- "$1" || true
}

# delete_vm_expect_success NAME - delete and wait for the task to commit.
delete_vm_expect_success() {
  otx vm delete "$1" --force --wait --wait-timeout "${DELETE_WAIT}s" \
    || fail "vm delete $1 did not reach terminal success within ${DELETE_WAIT}s"
}

cleanup() {
  echo "--- cleanup ---"
  run_on "$H1" sudo systemctl start otherix-agent >/dev/null 2>&1 || true
  # Bounded: cleanup runs both as the pre-flight and inside a failure path, and
  # the default wait is five minutes per call - a wedged VM would hang here for
  # ten before the smoke could even report why it failed.
  otx vm delete "$VM_LIVE" --force --wait --wait-timeout "${DELETE_WAIT}s" >/dev/null 2>&1 || true
  otx vm delete "$VM_CRASH" --force --wait --wait-timeout "${DELETE_WAIT}s" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-tombstone smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
[ -f "$API_LOG" ] || fail "api log not found at '$API_LOG' (run make local-dev-start, or set API_LOG=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

smoke_reap_orphan_vms

pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s"
pass "CP up; $NODE1 ready; default pool ready"

# =======================================================================
# scenario 1: a running vm deleted while its node was unreachable
# =======================================================================
echo
echo "=== scenario 1: delete $VM_LIVE while $NODE1 is down, then bring the agent back ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of a stale leftover

otx vm create "$VM_LIVE" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_LIVE did not reach running within ${CREATE_WAIT}s"

LIVE_ID="$(otx vm get "$VM_LIVE" --output json | jq -r '.id')"
[[ "$LIVE_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the id of $VM_LIVE (got '$LIVE_ID')"
[[ "$(qemu_count "$LIVE_ID")" == "1" ]] || fail "$VM_LIVE: want exactly 1 qemu for $LIVE_ID after create, found $(qemu_count "$LIVE_ID")"
[[ "$(dir_exists "$(state_dir "$LIVE_ID")")" == "yes" ]] || fail "$VM_LIVE: no state dir after create"
POOL_SEEN="$(dir_exists "$(pool_dir "$LIVE_ID")")"
pass "$VM_LIVE running on $NODE1 (id=$LIVE_ID, qemu up, state dir present, pool disk dir visible=$POOL_SEEN)"

info "stopping the agent on $NODE1 and waiting ${STALE_WAIT}s past the staleness window"
agent_stop
sleep "$STALE_WAIT"
[[ "$(qemu_count "$LIVE_ID")" == "1" ]] || fail "$VM_LIVE: qemu died with the agent; it must outlive it"
pass "agent stopped, guest still running (qemu outlives the agent)"

# The agent is down, so a delete that reaches terminal success cannot have been
# serviced by it: this is the skip-the-agent path, and it is what leaves a guest
# running on a node the CP believes it has cleaned up.
delete_vm_expect_success "$VM_LIVE"
otx vm get "$VM_LIVE" --output json >/dev/null 2>&1 && fail "$VM_LIVE still readable after delete"
[[ "$(qemu_count "$LIVE_ID")" == "1" ]] || fail "$VM_LIVE: qemu vanished without the agent"
pass "delete committed with the agent down; guest still running on $NODE1"

echo "--- restart 1: the agent replays the vm and must tear it down ---"
agent_start
wait_node_ready
assert_torn_down "$LIVE_ID" "$VM_LIVE after restart 1"

signals="$(tombstone_signals "$LIVE_ID")"
(( signals > 0 )) || fail "$VM_LIVE was torn down but the CP never logged a teardown signal for it"
pass "CP signalled the teardown ${signals}x before the vm went away"

echo "--- restart 2: nothing may replay ---"
run_on "$H1" sudo systemctl restart otherix-agent
wait_node_ready
sleep 20   # two reconciler ticks plus heartbeats: time enough for a replay to show
assert_still_gone "$LIVE_ID" "$VM_LIVE after restart 2"

echo "--- the teardown signal must stop once the node stops reporting the vm ---"
before="$(tombstone_signals "$LIVE_ID")"
sleep "$QUIET_WAIT"
after="$(tombstone_signals "$LIVE_ID")"
[[ "$before" == "$after" ]] \
  || fail "the CP is still signalling a teardown for a vm the node no longer reports ($before -> $after over ${QUIET_WAIT}s)"
pass "teardown signal stopped (${after} total, unchanged over ${QUIET_WAIT}s)"

# =======================================================================
# scenario 2: a teardown the agent crashed part-way through
# =======================================================================
echo
echo "=== scenario 2: $VM_CRASH replayed mid-teardown must be re-driven to completion ==="

otx vm create "$VM_CRASH" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_CRASH did not reach running within ${CREATE_WAIT}s"

CRASH_ID="$(otx vm get "$VM_CRASH" --output json | jq -r '.id')"
[[ "$CRASH_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve the id of $VM_CRASH (got '$CRASH_ID')"
[[ "$(qemu_count "$CRASH_ID")" == "1" ]] || fail "$VM_CRASH: want exactly 1 qemu for $CRASH_ID after create, found $(qemu_count "$CRASH_ID")"
POOL_SEEN="$(dir_exists "$(pool_dir "$CRASH_ID")")"
pass "$VM_CRASH running on $NODE1 (id=$CRASH_ID, pool disk dir visible=$POOL_SEEN)"

agent_stop
sleep "$STALE_WAIT"
delete_vm_expect_success "$VM_CRASH"

# Leave behind exactly what an agent killed part-way through a teardown leaves:
# a meta.json that says `deleting`, next to a qemu process that is still alive.
META="$(state_dir "$CRASH_ID")/meta.json"
meta_json="$(run_on "$H1" sudo cat "$META")"
[[ -n "$meta_json" ]] || fail "could not read $META"
printf '%s' "$meta_json" | jq '.status = "deleting"' | run_on "$H1" sudo tee "$META" >/dev/null
[[ "$(run_on "$H1" sudo cat "$META" | jq -r '.status')" == "deleting" ]] \
  || fail "could not stage the interrupted-teardown state in $META"
[[ "$(qemu_count "$CRASH_ID")" == "1" ]] || fail "$VM_CRASH: qemu gone before the replay"
pass "staged an interrupted teardown: meta.json says deleting, qemu still alive"

agent_start
wait_node_ready
assert_torn_down "$CRASH_ID" "$VM_CRASH after the interrupted-teardown replay"

run_on "$H1" sudo systemctl restart otherix-agent
wait_node_ready
sleep 20
assert_still_gone "$CRASH_ID" "$VM_CRASH after a further restart"

trap - EXIT
cleanup >/dev/null 2>&1 || true
echo
echo "${GREEN}=== vm-tombstone smoke PASSED ===${NC}"
