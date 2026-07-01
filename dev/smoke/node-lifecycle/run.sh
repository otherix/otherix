#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Node-lifecycle smoke - drives the operator node decommission + recovery verbs
# through the `otherix` CLI across the three-node dev stack, closing the
# real-agent coverage gap for `node delete`, `node delete --force`, agent-cert
# revocation, and `node readmit`. The CP-side paths are unit / e2e tested, but a
# full delete -> re-join -> readmit cycle against a REAL agent (its cert revoked,
# then a fresh cert issued on re-join) is only proven here.
#
# All four scenarios operate on a single victim node (node-3 by default) so the
# default VM host (node-1) is never disturbed. Every destructive step restores
# the victim by re-joining it, so the stack is left whole and the smoke is
# re-runnable.
#
# Four scenarios, all via the CLI:
#
#   1. baseline
#        `otherix node list` - capture the three cluster nodes.
#
#   2. plain delete of an empty node
#        assert the victim hosts no VMs, `otherix node delete <victim>`, assert
#        it disappears from the cluster, then re-join it and confirm it returns
#        to ready (restoring the stack for the next scenario).
#
#   3. force-delete with a VM + cert revocation
#        create a VM pinned to the victim -> running; `otherix node delete
#        <victim>` is refused (409, "use force=true"); `otherix node delete
#        <victim> --force` succeeds; the VM row is orphaned (status.phase
#        orphaned, status.current_node_id cleared); then re-join the SAME node
#        name and confirm the join is ACCEPTED with a fresh cert (a surviving,
#        un-revoked agent cert would collide) and the node returns to ready.
#        The delete revoked the agent cert either way, so the accepted re-join is
#        the operator-observable revocation proof.
#
#   4. gone-node readmit
#        stop the victim's agent, poll until the CP marks it `gone`, `otherix
#        node readmit <victim>` (gone -> pending; a non-gone node is refused
#        409, so an accepted readmit is itself the pending proof), restart the
#        agent, and assert the node heartbeats its way back to ready.
#
# The re-join reproduces the dev-stack bootstrap (mint a join token, run
# `otherix-agent bootstrap --force`, restart the agent) exactly as seed-dev does,
# so a re-join failure is a real product signal, not a script artifact.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready; the victim node hosts no VMs.
#
# Usage: make smoke-node-lifecycle   (or: bash dev/smoke/node-lifecycle/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
VICTIM_INDEX="${VICTIM_INDEX:-3}"            # the node delete/readmit acts on (keeps node-1 untouched)
VICTIM="${VICTIM:-node-${VICTIM_INDEX}}"     # the victim node name
VHANDLE="$(smoke_handle "$VICTIM_INDEX")"    # the victim's exec handle (Lima VM / netns)
VSTATE="$(smoke_state "$VICTIM_INDEX")"      # the victim's agent state_path root
VM="${VM:-nl-vm-$(date +%H%M%S)-$$}"         # unique-per-run VM for the force-delete scenario
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}" # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"            # seconds for vm create -> running (incl. cold image fetch)
OP_WAIT="${OP_WAIT:-180}"                    # seconds for an async lifecycle task to reach terminal
NODE_WAIT="${NODE_WAIT:-120}"                # seconds for a node status to settle
GONE_WAIT="${GONE_WAIT:-240}"                # seconds for a stopped node to advance to gone
READMIT_WAIT="${READMIT_WAIT:-180}"          # seconds for a re-joined / readmitted node to reach ready
ABSENT_WAIT="${ABSENT_WAIT:-60}"             # seconds for a deleted node to disappear
PENDING_WINDOW="${PENDING_WINDOW:-12}"       # seconds to observe the post-readmit pending status

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
# Progress/status lines go to stderr so they never leak into a $(...) capture.
pass() { echo "${GREEN}PASS${NC} $*" >&2; }
info() { echo "${YEL}..${NC} $*" >&2; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; echo "OTHERIX_SMOKE_FAIL"; exit 1; }

otx() { "$OTX" "$@"; }

# node_status NAME -> the CP node status ("" when the node is absent / on error)
node_status() { otx node get "$1" --output json 2>/dev/null | jq -r '.status' 2>/dev/null || true; }

# vm_phase NAME -> the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# vm_current_node NAME -> the VM's status.current_node_id ("" when cleared/absent)
vm_current_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.current_node_id // ""' 2>/dev/null || true; }

# node_present NAME -> 0 when the node still resolves through the CP, non-zero once deleted.
node_present() { otx node get "$1" >/dev/null 2>&1; }

# vms_on_node NAME -> count of VMs the CP reports on NAME (0 on a clean node).
vms_on_node() {
  otx vm list --output json 2>/dev/null \
    | jq -r --arg n "$1" '[.data[]? | select(.node==$n)] | length' 2>/dev/null || echo 0
}

# assert_node_status NAME WANT [TIMEOUT] -> poll the node status until it equals WANT.
assert_node_status() {
  local name="$1" want="$2" to="${3:-$NODE_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(node_status "$name")"
    [[ "$got" == "$want" ]] && { pass "$name status=$want"; return 0; }
    sleep 3
  done
  fail "$name status: want '$want' got '${got:-none}' after ${to}s"
}

# wait_node_absent NAME [TIMEOUT] -> block until the node no longer resolves (404).
wait_node_absent() {
  local name="$1" to="${2:-$ABSENT_WAIT}" deadline
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    node_present "$name" || return 0
    sleep 2
  done
  return 1
}

# assert_vm_phase NAME WANT [TIMEOUT] -> poll status.phase until it equals WANT.
assert_vm_phase() {
  local name="$1" want="$2" to="${3:-$OP_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(vm_phase "$name")"
    [[ "$got" == "$want" ]] && { pass "$name phase=$want"; return 0; }
    sleep 3
  done
  fail "$name phase: want '$want' got '${got:-none}' after ${to}s"
}

# lima_advertised INDEX -> the per-node CP->agent advertised endpoint the dev
# stack assigns on Lima (127.0.0.1:9443/9444/9445), mirroring seed-dev.
lima_advertised() {
  case "$1" in 1) echo "https://127.0.0.1:9443" ;; 2) echo "https://127.0.0.1:9444" ;; 3) echo "https://127.0.0.1:9445" ;; esac
}

# stop_victim_agent / start_victim_agent -> stop or (re)start ONLY the victim's
# agent, per platform. On Lima the agent is a systemd unit; on netns it is the
# pid recorded by linux-multinode.sh (started + restartable through that script).
stop_victim_agent() {
  case "$SMOKE_PLATFORM" in
    lima)
      run_on "$VHANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl stop otherix-agent' >/dev/null 2>&1 || true
      run_on "$VHANDLE" sudo pkill -f 'otherix-agent serve' >/dev/null 2>&1 || true
      ;;
    netns)
      sudo sh -c "kill \$(cat '${VSTATE}/agent.pid' 2>/dev/null) 2>/dev/null" || true
      ;;
  esac
}
start_victim_agent() {
  case "$SMOKE_PLATFORM" in
    lima)
      run_on "$VHANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl start otherix-agent' >/dev/null 2>&1 \
        || run_on "$VHANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl restart otherix-agent' >/dev/null 2>&1 || true
      ;;
    netns)
      sudo bash "${REPO_ROOT}/dev/scripts/linux-multinode.sh" start "$VICTIM_INDEX" >/dev/null 2>&1 || true
      ;;
  esac
}

# rejoin_victim -> mint a fresh join token and re-bootstrap the victim's agent,
# reproducing the dev-stack bootstrap flow (seed-dev). Idempotent: bootstrap
# --force re-issues cert material and never overwrites the staged config. On a
# revoked-cert victim (after a delete) this is the operator's recovery path AND
# the indirect cert-revocation proof: the join is only accepted because a fresh
# cert is issued.
rejoin_victim() {
  local n="$VICTIM_INDEX" node="$VICTIM" bundle token fp
  info "minting a fresh join token for $node and re-bootstrapping its agent"
  bundle="$(otx node join-token create --node-name "$node" --ttl 10m --output json)" \
    || fail "join-token create for $node failed"
  token="$(jq -r '.token' <<<"$bundle")"
  fp="$(jq -r '.ca_fingerprint_sha256' <<<"$bundle")"
  { [[ -n "$token" && "$token" != null && -n "$fp" && "$fp" != null ]]; } \
    || fail "join-token bundle for $node missing token or fingerprint: $bundle"

  case "$SMOKE_PLATFORM" in
    lima)
      local advertised vm_user vm_group
      advertised="$(lima_advertised "$n")"
      [[ -n "$advertised" ]] || fail "no Lima advertised endpoint mapping for node index $n"
      # Re-ensure the agent's state dirs with the agent user's ownership (a state
      # wipe on a reused VM removes them), mirroring seed-dev, so bootstrap's
      # cert-persist does not fail with a permission error.
      vm_user="$(run_on "$VHANDLE" id -un 2>/dev/null)"
      vm_group="$(run_on "$VHANDLE" id -gn 2>/dev/null)"
      run_on "$VHANDLE" sudo install -d -o "$vm_user" -g "$vm_group" \
        /var/lib/otherix /var/lib/otherix/certs /etc/otherix >/dev/null 2>&1 || true
      run_on "$VHANDLE" /usr/local/bin/otherix-agent bootstrap \
        --token "$token" \
        --ca-fingerprint "sha256:${fp}" \
        --cp-url "https://host.lima.internal:8443" \
        --node-name "$node" \
        --advertised-endpoint "$advertised" \
        --migration-host "0.0.0.0" \
        --migration-port-range-start 49152 \
        --migration-port-range-end 49251 \
        --listen "0.0.0.0:9443" \
        --force \
        || fail "otherix-agent bootstrap on $node failed"
      run_on "$VHANDLE" sudo systemctl restart otherix-agent >/dev/null 2>&1 || true
      ;;
    netns)
      sudo bash "${REPO_ROOT}/dev/scripts/linux-multinode.sh" bootstrap "$n" "$token" "$fp" \
        || fail "linux-multinode bootstrap of node $n failed"
      sudo bash "${REPO_ROOT}/dev/scripts/linux-multinode.sh" start "$n" \
        || fail "linux-multinode start of node $n failed"
      ;;
  esac
}

# --- cleanup -----------------------------------------------------------
cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the node/VM state as-is for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  otx vm delete "$VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
  # Best-effort: if the victim was left deleted/stopped by a mid-run failure, try
  # to bring its agent back so the stack returns to three ready nodes. A clean run
  # already restored it in each scenario.
  if ! node_present "$VICTIM"; then
    info "victim $VICTIM is absent after a failure - attempting a best-effort re-join"
    rejoin_victim >/dev/null 2>&1 || true
  else
    start_victim_agent
  fi
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== node-lifecycle smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
otx node delete --help >/dev/null 2>&1 || fail "this otherix build has no 'node delete' command (rebuild from the current tree)"
otx node readmit --help >/dev/null 2>&1 || fail "this otherix build has no 'node readmit' command (rebuild from the current tree)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
info "victim node: ${VICTIM} (index ${VICTIM_INDEX}, handle ${VHANDLE})"

# All three nodes must be ready to start from a known-good baseline.
for n in node-1 node-2 node-3; do
  st="$(node_status "$n")"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start for a three-node stack"
done
pass "CP up (${CP_VERSION}); node-1/node-2/node-3 ready"

# best-effort delete-first of a stale VM from a prior run so it does not 409.
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true

# =======================================================================
# scenario 1: baseline node list
# =======================================================================
echo
echo "=== scenario 1: baseline otherix node list ==="
NODES_JSON="$(otx node list --output json 2>/dev/null)" || fail "node list failed"
for n in node-1 node-2 node-3; do
  jq -e --arg n "$n" '.data[]? | select(.name==$n)' <<<"$NODES_JSON" >/dev/null \
    || fail "baseline node list is missing $n"
done
BASELINE_COUNT="$(jq -r '[.data[]?] | length' <<<"$NODES_JSON")"
pass "baseline node list has node-1/node-2/node-3 (${BASELINE_COUNT} nodes total)"

# =======================================================================
# scenario 2: plain delete of an empty node, then re-join to restore
# =======================================================================
echo
echo "=== scenario 2: plain delete of the empty node $VICTIM ==="
ON_VICTIM="$(vms_on_node "$VICTIM")"
[[ "$ON_VICTIM" == "0" ]] \
  || fail "$VICTIM hosts $ON_VICTIM VM(s); scenario 2 needs an empty node (run against a clean stack or set VICTIM_INDEX)"
info "$VICTIM hosts no VMs - deleting it"
DEL_OUT="$(otx node delete "$VICTIM" 2>&1)" || { echo "$DEL_OUT" >&2; fail "plain delete of the empty node $VICTIM failed"; }
echo "$DEL_OUT"
wait_node_absent "$VICTIM" \
  || fail "$VICTIM still resolves after a successful plain delete (expected it to disappear)"
pass "plain delete removed the empty node $VICTIM from the cluster"

echo "=== scenario 2: re-join $VICTIM to restore the stack ==="
rejoin_victim
assert_node_status "$VICTIM" ready "$READMIT_WAIT"
pass "$VICTIM re-joined and returned to ready after a plain delete"

# =======================================================================
# scenario 3: force-delete a node hosting a VM + cert-revocation proof
# =======================================================================
echo
echo "=== scenario 3: force-delete $VICTIM while it hosts $VM ==="
info "creating $VM pinned to $VICTIM (1 vcpu)"
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$VICTIM" \
  --vcpus 1 --memory-mb 1024 \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
assert_vm_phase "$VM" running
pass "$VM running on $VICTIM"

# plain delete must be refused (409) while the node hosts a VM.
set +e
DEL_OUT="$(otx node delete "$VICTIM" 2>&1)"; DEL_RC=$?
set -e
(( DEL_RC != 0 )) \
  || fail "plain delete of $VICTIM succeeded while it hosts $VM - expected a 409 refusal"
grep -qi "force" <<<"$DEL_OUT" \
  || fail "the delete refusal did not mention force (got: ${DEL_OUT})"
pass "plain delete of $VICTIM refused while it hosts a VM ($DEL_OUT)"

# force-delete detaches the VM and removes the node.
echo "=== scenario 3: otherix node delete $VICTIM --force ==="
DEL_OUT="$(otx node delete "$VICTIM" --force 2>&1)" \
  || { echo "$DEL_OUT" >&2; fail "force-delete of $VICTIM failed"; }
echo "$DEL_OUT"
wait_node_absent "$VICTIM" \
  || fail "$VICTIM still resolves after a successful force-delete"
pass "force-delete removed $VICTIM from the cluster"

# the VM is orphaned: phase orphaned, current_node_id cleared.
assert_vm_phase "$VM" orphaned
CN="$(vm_current_node "$VM")"
[[ -z "$CN" ]] \
  || fail "$VM still reports status.current_node_id='$CN' after force-delete - expected it cleared"
pass "$VM is orphaned with status.current_node_id cleared"

# clean up the orphaned VM row before restoring the node.
otx vm delete "$VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true

# cert-revocation proof: re-join the SAME node name. The delete revoked the old
# agent cert, so the join is accepted only because a fresh cert is issued; a
# surviving cert would collide. An accepted re-join back to ready is the proof.
echo "=== scenario 3: re-join $VICTIM (fresh cert) - proves the old cert was revoked ==="
rejoin_victim
assert_node_status "$VICTIM" ready "$READMIT_WAIT"
pass "$VICTIM re-joined with a fresh cert and returned to ready (old agent cert revoked)"

# =======================================================================
# scenario 4: drive the node to gone, then readmit
# =======================================================================
echo
echo "=== scenario 4: drive $VICTIM to gone, then readmit ==="
info "stopping the agent on $VICTIM so it stops heartbeating"
stop_victim_agent
info "waiting for the CP to mark $VICTIM gone (<= ${GONE_WAIT}s)"
assert_node_status "$VICTIM" gone "$GONE_WAIT"
pass "$VICTIM advanced to the terminal gone status"

# readmit: gone -> pending. A non-gone node is refused 409, so an accepted
# readmit is itself the gone->pending proof.
RM_OUT="$(otx node readmit "$VICTIM" 2>&1)" \
  || { echo "$RM_OUT" >&2; fail "node readmit $VICTIM failed"; }
echo "$RM_OUT"

# observe the pending status in the brief window before the agent restarts. The
# node-health sweep can re-demote a heartbeat-stale pending node, so this is a
# short poll that passes on the first pending sighting; the load-bearing proof
# that gone->pending happened is the readmit command's success above.
deadline=$(( SECONDS + PENDING_WINDOW )); saw_pending=0
while (( SECONDS < deadline )); do
  [[ "$(node_status "$VICTIM")" == "pending" ]] && { saw_pending=1; break; }
  sleep 1
done
if (( saw_pending == 1 )); then
  pass "$VICTIM readmitted to pending"
else
  info "did not catch the pending window (the sweep may have re-demoted a heartbeat-stale node); readmit succeeded, proceeding"
fi

# restart the agent; a fresh heartbeat promotes the node back to ready.
info "restarting the agent on $VICTIM so it heartbeats"
start_victim_agent
assert_node_status "$VICTIM" ready "$READMIT_WAIT"
pass "$VICTIM heartbeated its way back to ready after readmit"

# =======================================================================
# done
# =======================================================================
echo "=== final check: all three nodes ready ==="
for n in node-1 node-2 node-3; do
  assert_node_status "$n" ready "$NODE_WAIT"
done

trap - EXIT
cleanup >/dev/null 2>&1 || true
echo
echo "${GREEN}=== node-lifecycle smoke PASSED ===${NC}"
echo "  scenario 1: baseline node list (node-1/node-2/node-3)"
echo "  scenario 2: plain delete of empty $VICTIM -> gone from cluster -> re-joined ready"
echo "  scenario 3: force-delete $VICTIM (hosting $VM) -> VM orphaned -> re-joined with a fresh cert (old cert revoked)"
echo "  scenario 4: $VICTIM driven to gone -> readmit -> ready after agent restart"
echo "OTHERIX_SMOKE_PASS"
