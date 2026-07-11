#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Node-drain smoke - drives `otherix node drain` across the three-node dev stack
# through the `otherix` CLI as a real operator, closing the real-agent coverage
# gap for the drain saga: the CP-side drain path is unit/e2e tested, but a full
# cordon + live-migrate-every-VM-off + leave-cordoned cycle across three REAL
# agents (qemu + live cutover) is only proven here.
#
# Two scenarios, both via the CLI:
#
#   1. evacuation (happy path)
#        create two small VMs (1 vcpu) pinned to node-1 -> running
#        node drain node-1 --wait
#        assert: both VMs now run on node-2/node-3 (a DIFFERENT node), survived
#                the live cutover (their in-guest serial heartbeat keeps climbing
#                on the destination, no reset), node-1 ends `cordoned`, and the
#                drain task ends `success`.
#
#   2. timeout / stuck
#        fill node-2 and node-3 so neither has room for a whole-node VM, then
#        create one whole-node VM (4 vcpus) pinned to node-1 -> running
#        node drain node-1 --timeout 60s --wait
#        assert: the drain task ends `failed` with remaining >= 1, node-1 ends
#                `cordoned`, and the stuck VM is STILL running on node-1 (drain
#                never stops or destroys a VM it cannot place).
#
# Drain flips the node to `draining`, live-migrates each VM onto an eligible
# node bounded by the drain budget, never stops/destroys a VM, and leaves the
# node `cordoned` when it finishes (emptied -> task success; timed out with VMs
# left -> task failed, VMs still running). The CLI renders the drain result
# (migrated / remaining / in_flight, plus any stuck VMs) from the task result.
#
# The in-guest heartbeat writes to the arch-correct serial device (ttyS0 on
# amd64, ttyAMA0 on arm64), which the agent captures into the VM's serial.log,
# mirroring the live-migration smokes. The dev stack is three KVM-accelerated
# Lima VMs (4 vcpus / 8 GiB each) under strict placement (cpu/memory overcommit
# 1.0), so a 4-vcpu VM occupies a whole node and the timeout scenario genuinely
# has nowhere to migrate to.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-node-drain   (or: bash dev/smoke/node-drain/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # drained node (creates are pinned here)
NODE2="node-2"
NODE3="node-3"
EVAC_A="${EVAC_A:-drain-evac-a}"              # happy-path VM #1 (1 vcpu, must migrate off)
EVAC_B="${EVAC_B:-drain-evac-b}"              # happy-path VM #2 (1 vcpu, must migrate off)
FILL_2="${FILL_2:-drain-fill-2}"              # node-2 filler (whole node) for the timeout scenario
FILL_3="${FILL_3:-drain-fill-3}"              # node-3 filler (whole node) for the timeout scenario
STUCK_VM="${STUCK_VM:-drain-stuck}"           # the un-placeable whole-node VM on node-1
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel
DRAIN_WAIT="${DRAIN_WAIT:-600}"               # seconds for the evacuation drain task -> terminal
PHASE_WAIT="${PHASE_WAIT:-120}"               # seconds to wait for the CP-projected status.phase / node
NODE_WAIT="${NODE_WAIT:-120}"                 # seconds to wait for the node status to settle (cordoned)
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
TIMEOUT_BUDGET="${TIMEOUT_BUDGET:-60s}"       # drain budget for the timeout scenario
TIMEOUT_WAIT="${TIMEOUT_WAIT:-300}"           # seconds the CLI blocks on the timeout drain task

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
# Progress/status lines go to stderr so they never leak into a $(...) capture.
# Several helpers below print their RESULT to stdout (e.g. wait_heartbeat prints
# the counter the caller stores in a baseline); a status line emitted on stdout
# would be captured alongside that value and corrupt the later arithmetic.
pass() { echo "${GREEN}PASS${NC} $*" >&2; }
info() { echo "${YEL}..${NC} $*" >&2; }
fail() { echo "${RED}FAIL${NC} $*" >&2; echo "OTHERIX_SMOKE_FAIL"; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME -> prints the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# vm_node NAME -> prints the VM's current node name ("" if unscheduled/gone). The
# CP keeps .node = current_node_id and only repoints it after a migration cutover,
# so this is the placement signal.
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // ""' 2>/dev/null || true; }

# vm_id NAME -> prints the VM uuid ("" if absent)
vm_id() { otx vm get "$1" --output json 2>/dev/null | jq -r '.id // ""' 2>/dev/null || true; }

# node_status NAME -> prints the CP node status ("" on error)
node_status() { otx node get "$1" --output json 2>/dev/null | jq -r '.status' 2>/dev/null || true; }

# assert_phase NAME WANT [TIMEOUT] -> poll status.phase until it equals WANT
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

# assert_node_status NAME WANT [TIMEOUT] -> poll the node status until it equals WANT
assert_node_status() {
  local name="$1" want="$2" to="${3:-$NODE_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(node_status "$name")"
    [[ "$got" == "$want" ]] && { pass "$name status=$want"; return 0; }
    sleep 2
  done
  fail "$name status: want '$want' got '${got:-none}' after ${to}s"
}

# heartbeat_max HANDLE STATE VMID -> the maximum heartbeat counter <n> seen in the
# VM's serial.log on the node identified by HANDLE/STATE, or empty when no
# heartbeat line is present. Parses "OTHERIX_DRAIN_HB <n> <epoch>" lines and takes
# the largest <n>. Append-only log -> this is monotonic on a single node.
heartbeat_max() {
  local handle="$1" state="$2" id="$3" out
  out="$(run_on "$handle" sudo grep -oE 'OTHERIX_DRAIN_HB [0-9]+' \
      "${state}/vms/${id}/serial.log" 2>/dev/null)" || true
  [[ -n "$out" ]] || { printf ''; return 0; }
  printf '%s\n' "$out" | awk '{print $2}' | sort -n | tail -1
}

# wait_heartbeat HANDLE STATE VMID -> block until at least one heartbeat line
# appears in the VM's serial.log on that node, then print the max counter seen.
# Returns non-zero on timeout.
wait_heartbeat() {
  local handle="$1" state="$2" id="$3" deadline mx; deadline=$(( SECONDS + GUEST_WAIT ))
  info "waiting for the in-guest heartbeat on $handle (<= ${GUEST_WAIT}s)"
  while (( SECONDS < deadline )); do
    mx="$(heartbeat_max "$handle" "$state" "$id")"
    [[ -n "$mx" ]] && { printf '%s' "$mx"; return 0; }
    sleep 5
  done
  echo "--- serial tail ($handle) ---" >&2
  run_on "$handle" sudo tail -40 "${state}/vms/${id}/serial.log" 2>/dev/null >&2 || true
  return 1
}

# wait_heartbeat_above HANDLE STATE VMID BASE -> block until the heartbeat counter
# on that node climbs strictly ABOVE BASE, then print the counter. The live
# cutover carries the SAME long-lived guest process across the move, so its
# monotonic counter CONTINUES on the destination node; a reboot or a stopped
# guest would never advance it there. Returns non-zero on timeout.
wait_heartbeat_above() {
  local handle="$1" state="$2" id="$3" base="$4" deadline mx; deadline=$(( SECONDS + GUEST_WAIT ))
  info "waiting for the heartbeat on $handle to climb above ${base} (<= ${GUEST_WAIT}s)"
  while (( SECONDS < deadline )); do
    mx="$(heartbeat_max "$handle" "$state" "$id")"
    [[ -n "$mx" ]] && (( mx > base )) && { printf '%s' "$mx"; return 0; }
    sleep 3
  done
  echo "--- serial tail ($handle) ---" >&2
  run_on "$handle" sudo tail -40 "${state}/vms/${id}/serial.log" 2>/dev/null >&2 || true
  return 1
}

# wait_cinit_done HANDLE STATE VMID -> block until the end-of-cloud-init marker
# (OTHERIX_CINIT_DONE, emitted as the final runcmd line at cloud-final) appears in
# the VM's serial.log on that node. Gating on this before the drain guarantees the
# guest is fully initialised (not mid SSH-keygen) when the live cutover moves it,
# so a co-located destination under CPU contention does not stall a half-booted
# guest. Returns non-zero on timeout.
wait_cinit_done() {
  local handle="$1" state="$2" id="$3" deadline n; deadline=$(( SECONDS + GUEST_WAIT ))
  info "waiting for cloud-init to finish on $handle (OTHERIX_CINIT_DONE, <= ${GUEST_WAIT}s)"
  while (( SECONDS < deadline )); do
    n="$(run_on "$handle" sudo grep -c 'OTHERIX_CINIT_DONE' \
        "${state}/vms/${id}/serial.log" 2>/dev/null)" || true
    [[ "$n" =~ ^[0-9]+$ ]] && (( n > 0 )) && return 0
    sleep 5
  done
  echo "--- serial tail ($handle) ---" >&2
  run_on "$handle" sudo tail -40 "${state}/vms/${id}/serial.log" 2>/dev/null >&2 || true
  return 1
}

# handle_for NODE / state_for NODE -> the exec handle / state root for a node name.
handle_for() {
  case "$1" in node-1) smoke_handle 1 ;; node-2) smoke_handle 2 ;; node-3) smoke_handle 3 ;; esac
}
state_for() {
  case "$1" in node-1) smoke_state 1 ;; node-2) smoke_state 2 ;; node-3) smoke_state 3 ;; esac
}

# --- the heartbeat cloud-config (every-second serial heartbeat) --------
# A first-boot runcmd writes a heartbeat script and launches it DETACHED (setsid
# + nohup) so it survives cloud-init exiting. The loop writes an incrementing
# "OTHERIX_DRAIN_HB <n> <epoch>" line to the arch-correct serial device (ttyS0 on
# amd64, ttyAMA0 on arm64) once a second; the agent captures that console into
# serial.log. Crucially the loop is ONE long-lived process: the drain saga
# live-migrates the guest, which preserves the process (and its monotonic
# counter) across the move, so the counter CONTINUES climbing on the destination
# node. That continuity is the liveness + survival proof - a reboot would reset
# the counter to 0 and a stopped guest would never advance it. Quoted heredoc ->
# the shell vars stay literal for the guest.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /usr/local/bin/otherix-drain-hb.sh
    permissions: '0755'
    content: |
      #!/bin/sh
      SC=/dev/ttyS0
      [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0
      i=0
      while :; do
        echo "OTHERIX_DRAIN_HB $i $(date -u +%s)" > "$SC"
        i=$((i+1))
        sleep 1
      done
runcmd:
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-drain-hb.sh >/dev/null 2>&1 < /dev/null &" ]
  # The LAST runcmd line (runcmd runs once, at cloud-final) marks first-boot
  # cloud-init fully finished. The smoke gates on this BEFORE draining so the live
  # cutover moves a steady-state guest, never one whose cloud-init is still mid
  # SSH-keygen and contending for CPU on a co-located destination. Emitted to the
  # arch-correct serial device the same way the heartbeat is, so the agent
  # captures it into the same serial.log.
  - [ sh, -c, "SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; echo \"OTHERIX_CINIT_DONE $(hostname)\" > \"$SC\"" ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- cleanup -----------------------------------------------------------
ALL_VMS=("$EVAC_A" "$EVAC_B" "$FILL_2" "$FILL_3" "$STUCK_VM")

cleanup() {
  echo "--- cleanup ---"
  local v
  for v in "${ALL_VMS[@]}"; do
    otx vm delete "$v" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
  done
  # leave the drained node usable for the next smoke: uncordon node-1.
  otx node uncordon "$NODE1" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== node-drain smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

# All three nodes must be ready: node-1 hosts the VMs to evacuate, node-2/node-3
# are the migration targets (and the fillers for the timeout scenario).
for n in "$NODE1" "$NODE2" "$NODE3"; do
  st="$(node_status "$n")"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start for a three-node stack"
done

# default DISK pool reconciled on every node (creates + cutovers bind to it).
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
for n in "$NODE1" "$NODE2" "$NODE3"; do
  deadline=$(( SECONDS + 60 )); ok=0
  while (( SECONDS < deadline )); do pool_ready "$n" && { ok=1; break; }; sleep 3; done
  (( ok == 1 )) || fail "default disk pool not ready on $n within 60s (CP auto-provision)"
done
pass "CP up (${CP_VERSION}); node-1/node-2/node-3 ready; default disk pool ready on all three"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers
# the precondition uncordon may have left node-1 cordoned from a prior cleanup;
# the drain scenarios cordon it themselves, so re-arm it ready here.
otx node uncordon "$NODE1" >/dev/null 2>&1 || true

# =======================================================================
# scenario 1: evacuation (happy path)
# =======================================================================
echo
echo "=== scenario 1: evacuation - drain node-1, both VMs migrate to node-2/node-3 ==="

# --- 1a: create two small VMs pinned to node-1 -> running + heartbeat ---
N1_HANDLE="$(handle_for "$NODE1")"; N1_STATE="$(state_for "$NODE1")"
for v in "$EVAC_A" "$EVAC_B"; do
  info "creating $v on $NODE1 (1 vcpu)"
  printf '%s' "$CLOUD_INIT" | otx vm create "$v" \
    --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
    --vcpus 1 --memory-mib 1024 --user-data - \
    --wait --wait-timeout "${CREATE_WAIT}s" \
    || fail "vm create $v did not reach running within ${CREATE_WAIT}s"
  assert_phase "$v" running
  [[ "$(vm_node "$v")" == "$NODE1" ]] || fail "$v did not land on $NODE1 (got '$(vm_node "$v")')"
done
EVAC_A_ID="$(vm_id "$EVAC_A")"; EVAC_B_ID="$(vm_id "$EVAC_B")"
[[ "$EVAC_A_ID" =~ ^[0-9a-f-]{36}$ && "$EVAC_B_ID" =~ ^[0-9a-f-]{36}$ ]] \
  || fail "could not resolve evac VM ids ($EVAC_A_ID / $EVAC_B_ID)"
# wait until each guest's in-guest heartbeat is running on node-1, and record a
# pre-drain baseline. After the drain we require the counter to climb ABOVE this
# on the destination node, proving the SAME guest process survived the cutover.
A_BASE="$(wait_heartbeat "$N1_HANDLE" "$N1_STATE" "$EVAC_A_ID")" \
  || fail "$EVAC_A heartbeat never started on $NODE1 within ${GUEST_WAIT}s"
B_BASE="$(wait_heartbeat "$N1_HANDLE" "$N1_STATE" "$EVAC_B_ID")" \
  || fail "$EVAC_B heartbeat never started on $NODE1 within ${GUEST_WAIT}s"
pass "$EVAC_A + $EVAC_B running on $NODE1 with live heartbeats (baselines $A_BASE / $B_BASE)"

# Gate the drain on first-boot cloud-init FULLY completing on each evac VM. Without
# this the drain can live-migrate a guest whose cloud-init is still running; on a
# co-located destination under CPU contention that half-booted guest stalls. Moving
# a steady-state guest keeps the cutover (and the heartbeat-continuation proof
# below) robust regardless of where the scheduler places the two evac VMs.
wait_cinit_done "$N1_HANDLE" "$N1_STATE" "$EVAC_A_ID" \
  || fail "$EVAC_A cloud-init did not finish on $NODE1 within ${GUEST_WAIT}s (guest-boot problem)"
wait_cinit_done "$N1_HANDLE" "$N1_STATE" "$EVAC_B_ID" \
  || fail "$EVAC_B cloud-init did not finish on $NODE1 within ${GUEST_WAIT}s (guest-boot problem)"
pass "$EVAC_A + $EVAC_B finished first-boot cloud-init on $NODE1 (steady-state before drain)"

# --- 1b: drain node-1 (blocking) ---------------------------------------
echo "=== scenario 1: otherix node drain $NODE1 --wait ==="
DRAIN_OUT="$(otx node drain "$NODE1" --wait --wait-timeout "${DRAIN_WAIT}s" 2>&1)" \
  || { echo "$DRAIN_OUT"; fail "node drain $NODE1 (evacuation) returned an error"; }
echo "$DRAIN_OUT"
# the rendered result line is "drain node-1: <status>"; require success.
echo "$DRAIN_OUT" | grep -qE "^drain ${NODE1}: success" \
  || fail "evacuation drain did not end success; CLI output above"
pass "evacuation drain task ended success"

# --- 1c: both VMs migrated off node-1, still running -------------------
# A successful drain means every cutover finished; poll the CP placement to ride
# out any brief projection lag before reading the destination node.
wait_off_node1() {
  local name="$1" deadline got; deadline=$(( SECONDS + PHASE_WAIT ))
  while (( SECONDS < deadline )); do
    got="$(vm_node "$name")"
    [[ -n "$got" && "$got" != "$NODE1" ]] && { printf '%s' "$got"; return 0; }
    sleep 2
  done
  fail "$name still on $NODE1 after a successful drain (node='${got:-none}')"
}
A_NODE="$(wait_off_node1 "$EVAC_A")"
B_NODE="$(wait_off_node1 "$EVAC_B")"
pass "$EVAC_A -> $A_NODE, $EVAC_B -> $B_NODE (both off $NODE1)"
assert_phase "$EVAC_A" running
assert_phase "$EVAC_B" running

# --- 1d: node-1 left cordoned ------------------------------------------
assert_node_status "$NODE1" cordoned
pass "$NODE1 left cordoned after a successful evacuation"

# --- 1e: prove the migrated guests survived the live cutover -----------
# The drain saga live-migrates each guest: the SAME qemu process moves to the
# destination, so the in-guest heartbeat counter CONTINUES climbing there (above
# the node-1 baseline) without resetting. A reboot would reset it to 0; a stopped
# guest would never advance it. This is the real "still running and survived"
# proof, no reboot needed.
for pair in "$EVAC_A:$EVAC_A_ID:$A_NODE:$A_BASE" "$EVAC_B:$EVAC_B_ID:$B_NODE:$B_BASE"; do
  IFS=: read -r v id nn base <<<"$pair"
  h="$(handle_for "$nn")"; s="$(state_for "$nn")"
  HB="$(wait_heartbeat_above "$h" "$s" "$id" "$base")" \
    || fail "$v heartbeat did not continue above $base on $nn within ${GUEST_WAIT}s (guest did not survive the cutover)"
  pass "$v survived live migration onto $nn (heartbeat climbed $base -> $HB, no reset)"
done

# --- 1f: tidy up scenario 1 so the cluster is clean for scenario 2 -----
echo "=== scenario 1: cleanup evac VMs, uncordon $NODE1 ==="
otx vm delete "$EVAC_A" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx vm delete "$EVAC_B" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx node uncordon "$NODE1" >/dev/null 2>&1 || fail "could not uncordon $NODE1 for scenario 2"
assert_node_status "$NODE1" ready
pass "evacuation scenario complete; cluster reset for the timeout scenario"

# =======================================================================
# scenario 2: timeout / stuck
# =======================================================================
echo
echo "=== scenario 2: timeout - a whole-node VM on node-1 cannot fit anywhere else ==="

# --- 2a: fill node-2 and node-3 with whole-node VMs --------------------
# Each dev node has 4 vcpus under strict overcommit, so a 4-vcpu VM consumes a
# whole node. Filling node-2 and node-3 leaves NO node able to host another
# 4-vcpu VM, so the drain VM on node-1 has no eligible target.
for pair in "$FILL_2:$NODE2" "$FILL_3:$NODE3"; do
  IFS=: read -r v nn <<<"$pair"
  info "creating filler $v on $nn (4 vcpus, whole node)"
  printf '%s' "$CLOUD_INIT" | otx vm create "$v" \
    --image-url "$IMAGE_URL" --arch "$ARCH" --node "$nn" \
    --vcpus 4 --memory-mib 2048 --user-data - \
    --wait --wait-timeout "${CREATE_WAIT}s" \
    || fail "filler vm create $v on $nn did not reach running within ${CREATE_WAIT}s"
  assert_phase "$v" running
  [[ "$(vm_node "$v")" == "$nn" ]] || fail "filler $v did not land on $nn (got '$(vm_node "$v")')"
done
pass "$FILL_2 fills $NODE2, $FILL_3 fills $NODE3 (no room for another whole-node VM)"

# --- 2b: create the un-placeable whole-node VM on node-1 ---------------
info "creating $STUCK_VM on $NODE1 (4 vcpus, whole node)"
printf '%s' "$CLOUD_INIT" | otx vm create "$STUCK_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 4 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $STUCK_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$STUCK_VM" running
[[ "$(vm_node "$STUCK_VM")" == "$NODE1" ]] || fail "$STUCK_VM did not land on $NODE1"
pass "$STUCK_VM running on $NODE1 - too large to fit node-2 or node-3"

# --- 2c: drain node-1 with a short budget; expect timeout -> failed ----
echo "=== scenario 2: otherix node drain $NODE1 --timeout $TIMEOUT_BUDGET --wait ==="
DRAIN_OUT="$(otx node drain "$NODE1" --timeout "$TIMEOUT_BUDGET" --wait --wait-timeout "${TIMEOUT_WAIT}s" 2>&1)" \
  || { echo "$DRAIN_OUT"; fail "node drain $NODE1 (timeout scenario) returned an error"; }
echo "$DRAIN_OUT"

# the drain must end FAILED (budget expired with a VM still on the node).
echo "$DRAIN_OUT" | grep -qE "^drain ${NODE1}: failed" \
  || fail "timeout drain did not end failed; CLI output above"
# remaining must be >= 1 (the stuck VM stayed on the node). grep -oE is portable
# across GNU/BSD; a sed \+ quantifier is not.
REMAINING="$(echo "$DRAIN_OUT" | grep -oE 'remaining=[0-9]+' | head -1 | cut -d= -f2)"
[[ "$REMAINING" =~ ^[0-9]+$ ]] || fail "could not parse remaining= from drain output"
(( REMAINING >= 1 )) || fail "timeout drain reported remaining=$REMAINING, want >= 1"
pass "timeout drain task ended failed with remaining=$REMAINING"

# --- 2d: node-1 still cordoned, stuck VM still running on node-1 -------
assert_node_status "$NODE1" cordoned
pass "$NODE1 left cordoned after the timed-out drain"

# the drain never stops or destroys a VM it cannot place.
[[ "$(vm_node "$STUCK_VM")" == "$NODE1" ]] \
  || fail "$STUCK_VM left $NODE1 unexpectedly (node='$(vm_node "$STUCK_VM")') - drain must not move an un-placeable VM"
assert_phase "$STUCK_VM" running
pass "$STUCK_VM is STILL running on $NODE1 - drain never stopped or destroyed it"

# =======================================================================
# done
# =======================================================================
echo "=== final cleanup ==="
for v in "${ALL_VMS[@]}"; do
  otx vm delete "$v" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
done
otx node uncordon "$NODE1" >/dev/null 2>&1 || true
pass "best-effort cleanup done; $NODE1 uncordoned"

trap - EXIT
echo
echo "${GREEN}=== node-drain smoke PASSED ===${NC}"
echo "  scenario 1: drain $NODE1 -> $EVAC_A on $A_NODE, $EVAC_B on $B_NODE, node cordoned, task success"
echo "  scenario 2: drain $NODE1 (timeout) -> task failed remaining=$REMAINING, $STUCK_VM still running on $NODE1, node cordoned"
echo "OTHERIX_SMOKE_PASS"
