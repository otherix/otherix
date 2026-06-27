#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Node-placement-pressure smoke - proves the scheduler refuses to place VMs on a
# node whose storage pool is under disk pressure, drives the whole flow through
# the `otherix` CLI as a real operator, and closes the real-agent coverage gap:
# the exclusion is unit/e2e tested against the store, but the end-to-end chain
# (agent measures the pool filesystem -> heartbeat/scan -> CP sets disk_pressure
# -> scheduler excludes the pool -> VM lands elsewhere or stays pending) is only
# proven here against three REAL agents.
#
# Pressure is derived from a real measurement: the agent runs statfs on the pool
# filesystem during a pool scan and reports capacity/available; the CP flags
# disk_pressure when available drops below the configured threshold (15% in the
# default config). The smoke induces that condition deterministically by
# reserving blocks on ONE node's pool filesystem with fallocate (instant, no
# zeroing) so that the free space lands between the pool threshold (15%) and the
# system-disk threshold (10%) - only the pool reports pressure, the node stays
# healthy. Removing the file restores the free space and the next scan clears the
# condition.
#
# Two scenarios, both via the CLI:
#
#   1. selective avoidance
#        fill node-2's pool below the 15% threshold -> wait for the scan to set
#        disk_pressure on node-2's pool instance (node-1 / node-3 stay clear)
#        assert: a no-hint VM lands on node-1 or node-3, never node-2; and a VM
#                pinned to node-2 with --node stays `pending` with reason
#                `pool_not_on_node` - the scheduler refuses, the VM is never
#                placed on the pressured node and is never silently failed.
#
#   2. cluster-wide pressure + recovery
#        fill node-1 and node-3 too -> every pool under pressure
#        assert: a no-hint VM stays `pending` with reason `node_pressure` (it is
#                not placed and not failed); then remove every fill file, wait
#                for the scans to clear the pressure, and assert the SAME pending
#                VM converges to `running` on its own (the scheduler retries
#                forever and binds it once a pool is eligible again).
#
# VM placement never returns a synchronous error for "no eligible node"; create
# is admission-only (201 + phase=pending) and the scheduler projects the outcome
# onto status.reason / status.phase. So the assertions poll `vm get` rather than
# a create exit code - the scheduler failing toward inaction (pending, not
# destroyed) is exactly the behaviour under test.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-node-placement-pressure   (or: bash dev/smoke/node-placement-pressure/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"                                   # the node driven under pressure in scenario 1
NODE3="node-3"
AVOID_VM="${AVOID_VM:-pressure-avoid}"           # no-hint VM that must dodge the pressured node
PINNED_VM="${PINNED_VM:-pressure-pinned}"        # VM pinned to the pressured node (must stay pending)
CLUSTER_VM="${CLUSTER_VM:-pressure-cluster}"     # no-hint VM created while every pool is pressured
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"     # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
POOL_PATH="${POOL_PATH:-/var/lib/otherix/pools/default}"  # dev allowed_path_prefixes default
FILL_NAME=".smoke-pressure-fill.bin"             # reserved-blocks file on the pool filesystem
FREE_DIVISOR="${FREE_DIVISOR:-8}"                # leave capacity/8 (12.5%) free: below 15%, above 10%
CREATE_WAIT="${CREATE_WAIT:-600}"                # seconds for a VM to reach running (incl. cold image fetch)
PRESSURE_WAIT="${PRESSURE_WAIT:-200}"            # seconds to wait for a pool scan to set/clear pressure
REASON_WAIT="${REASON_WAIT:-120}"               # seconds to wait for the scheduler to project a status.reason
PLACE_WAIT="${PLACE_WAIT:-120}"                  # seconds to wait for a no-hint VM to settle on a node

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
# Progress lines go to stderr so they never leak into a $(...) capture.
pass() { echo "${GREEN}PASS${NC} $*" >&2; }
info() { echo "${YEL}..${NC} $*" >&2; }
fail() { echo "${RED}FAIL${NC} $*" >&2; echo "OTHERIX_SMOKE_FAIL"; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME / vm_node NAME / vm_reason NAME -> the CP-projected status fields
# ("" when the VM is absent). The scheduler writes status.reason while the VM is
# unscheduled; it clears once the VM is bound and running.
vm_phase()  { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase  // ""' 2>/dev/null || true; }
vm_node()   { otx vm get "$1" --output json 2>/dev/null | jq -r '.node          // ""' 2>/dev/null || true; }
vm_reason() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.reason // ""' 2>/dev/null || true; }

# node_status NAME -> the CP node status ("" on error)
node_status() { otx node get "$1" --output json 2>/dev/null | jq -r '.status' 2>/dev/null || true; }

# pool_pressure NODE -> "active" when the default pool instance on NODE reports
# disk_pressure, "clear" when it does not, "" when the row is absent.
pool_pressure() {
  otx pool get default --output json 2>/dev/null \
    | jq -r --arg n "$1" \
        '.instances[]? | select(.node==$n) | (if .disk_pressure then "active" else "clear" end)' \
        2>/dev/null || true
}

# handle_for NODE -> the platform exec handle (Lima VM name / netns name).
handle_for() {
  case "$1" in node-1) smoke_handle 1 ;; node-2) smoke_handle 2 ;; node-3) smoke_handle 3 ;; esac
}

# fill_pool NODE -> reserve blocks on NODE's pool filesystem so free space drops
# to capacity/FREE_DIVISOR (12.5%), below the 15% pool-disk threshold but above
# the 10% system-disk threshold. fallocate reserves blocks instantly without
# writing zeros; dd is the fallback for a filesystem that rejects fallocate. The
# fill is computed from the live df numbers, so it never exceeds the available
# space (it always leaves the target free margin behind).
fill_pool() {
  local node="$1" h dims size avail target fill fp
  h="$(handle_for "$node")"; fp="${POOL_PATH}/${FILL_NAME}"
  dims="$(run_on "$h" sh -c "df -B1 '${POOL_PATH}' | tail -1" 2>/dev/null | awk '{print $2, $4}')" \
    || fail "could not read df for ${POOL_PATH} on ${node}"
  read -r size avail <<<"$dims"
  [[ "$size" =~ ^[0-9]+$ && "$avail" =~ ^[0-9]+$ ]] || fail "unexpected df output on ${node}: '${dims}'"
  target=$(( size / FREE_DIVISOR ))
  fill=$(( avail - target ))
  if (( fill <= 0 )); then
    fail "${node} pool already below the target free margin (avail=${avail}, target=${target}) - cannot stage pressure"
  fi
  info "reserving $(( fill / 1024 / 1024 )) MiB on ${node}:${POOL_PATH} (cap=$(( size/1024/1024/1024 ))GiB, leaving $(( target/1024/1024/1024 ))GiB free)"
  run_on "$h" sudo fallocate -l "$fill" "$fp" 2>/dev/null \
    || run_on "$h" sudo dd if=/dev/zero of="$fp" bs=1048576 count=$(( fill / 1048576 )) status=none \
    || fail "could not reserve space on ${node}:${fp}"
}

# clear_pool NODE -> remove the fill file (best-effort), restoring free space.
clear_pool() {
  local node="$1" h; h="$(handle_for "$node")"
  run_on "$h" sudo rm -f "${POOL_PATH}/${FILL_NAME}" 2>/dev/null || true
}

# wait_pool_pressure NODE WANT -> poll the pool instance until its pressure state
# equals WANT ("active" | "clear"). Pool disk pressure needs a single scan
# observation to set or clear (consecutive_required=1), so this rides out one
# scan interval plus jitter.
wait_pool_pressure() {
  local node="$1" want="$2" deadline got
  deadline=$(( SECONDS + PRESSURE_WAIT ))
  while (( SECONDS < deadline )); do
    got="$(pool_pressure "$node")"
    [[ "$got" == "$want" ]] && { pass "${node} pool disk_pressure=${want}"; return 0; }
    sleep 5
  done
  fail "${node} pool disk_pressure: want '${want}' got '${got:-none}' after ${PRESSURE_WAIT}s (scan did not converge)"
}

# wait_vm_reason NAME WANT -> poll until the VM is pending with status.reason WANT.
wait_vm_reason() {
  local name="$1" want="$2" deadline ph rsn
  deadline=$(( SECONDS + REASON_WAIT ))
  while (( SECONDS < deadline )); do
    ph="$(vm_phase "$name")"; rsn="$(vm_reason "$name")"
    if [[ "$ph" == "running" ]]; then
      fail "${name} reached running but should have stayed pending (reason want '${want}')"
    fi
    [[ "$rsn" == "$want" ]] && { pass "${name} pending with reason=${want}"; return 0; }
    sleep 3
  done
  fail "${name} status.reason: want '${want}' got '${rsn:-none}' (phase '${ph:-none}') after ${REASON_WAIT}s"
}

# wait_vm_running_off NAME EXCLUDED -> poll until NAME is running on a node that
# is NOT EXCLUDED; fail if it lands on EXCLUDED or never settles.
wait_vm_running_off() {
  local name="$1" excluded="$2" deadline ph nd
  deadline=$(( SECONDS + CREATE_WAIT ))
  while (( SECONDS < deadline )); do
    ph="$(vm_phase "$name")"; nd="$(vm_node "$name")"
    if [[ "$ph" == "running" && -n "$nd" ]]; then
      [[ "$nd" != "$excluded" ]] && { pass "${name} running on ${nd} (avoided ${excluded})"; return 0; }
      fail "${name} landed on the pressured node ${excluded} - the scheduler did not exclude it"
    fi
    sleep 4
  done
  fail "${name} did not reach running off ${excluded} within ${CREATE_WAIT}s (phase '${ph:-none}', node '${nd:-none}')"
}

# wait_vm_running NAME -> poll until NAME is running on any node.
wait_vm_running() {
  local name="$1" deadline ph nd
  deadline=$(( SECONDS + CREATE_WAIT ))
  while (( SECONDS < deadline )); do
    ph="$(vm_phase "$name")"; nd="$(vm_node "$name")"
    [[ "$ph" == "running" && -n "$nd" ]] && { pass "${name} running on ${nd}"; return 0; }
    sleep 4
  done
  fail "${name} did not reach running within ${CREATE_WAIT}s (phase '${ph:-none}')"
}

# --- cleanup -----------------------------------------------------------
ALL_VMS=("$AVOID_VM" "$PINNED_VM" "$CLUSTER_VM")

cleanup() {
  echo "--- cleanup ---"
  local v n
  for v in "${ALL_VMS[@]}"; do
    otx vm delete "$v" --wait --force --wait-timeout "${CREATE_WAIT}s" >/dev/null 2>&1 || true
  done
  for n in "$NODE1" "$NODE2" "$NODE3"; do clear_pool "$n"; done
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== node-placement-pressure smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

for n in "$NODE1" "$NODE2" "$NODE3"; do
  st="$(node_status "$n")"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start for a three-node stack"
done

# default DISK pool reconciled on every node (placement binds to it).
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

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale VMs / fill files
# a stale fill file just removed must clear before we assert a clean baseline.
for n in "$NODE1" "$NODE2" "$NODE3"; do
  [[ "$(pool_pressure "$n")" != "active" ]] || wait_pool_pressure "$n" clear
done

# =======================================================================
# scenario 1: selective avoidance
# =======================================================================
echo
echo "=== scenario 1: a no-hint VM avoids the pressured node; a pinned VM stays pending ==="

# --- 1a: drive node-2's pool under the disk-pressure threshold ---------
fill_pool "$NODE2"
wait_pool_pressure "$NODE2" active
[[ "$(pool_pressure "$NODE1")" == "clear" ]] || fail "$NODE1 pool unexpectedly under pressure - baseline not clean"
[[ "$(pool_pressure "$NODE3")" == "clear" ]] || fail "$NODE3 pool unexpectedly under pressure - baseline not clean"
pass "$NODE2 pool under disk pressure; $NODE1 / $NODE3 pools clear"

# --- 1b: a no-hint VM must land on a healthy node, never node-2 --------
info "creating $AVOID_VM with no node hint (must avoid the pressured $NODE2)"
otx vm create "$AVOID_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" \
  --vcpus 1 --memory-mb 1024 \
  || fail "vm create $AVOID_VM was rejected at admission"
wait_vm_running_off "$AVOID_VM" "$NODE2"

# --- 1c: a VM pinned to node-2 must stay pending, never placed ---------
info "creating $PINNED_VM pinned to the pressured $NODE2 (must stay pending)"
otx vm create "$PINNED_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE2" \
  --vcpus 1 --memory-mb 1024 \
  || fail "vm create $PINNED_VM was rejected at admission"
wait_vm_reason "$PINNED_VM" "pool_not_on_node"
[[ "$(vm_node "$PINNED_VM")" != "$NODE2" ]] \
  || fail "$PINNED_VM was placed on the pressured $NODE2 despite the exclusion"
pass "$PINNED_VM stayed pending on the pressured $NODE2 (reason pool_not_on_node), never placed"

# clear the scenario-1 VMs before escalating to cluster-wide pressure.
otx vm delete "$AVOID_VM"  --wait --force --wait-timeout "${CREATE_WAIT}s" >/dev/null 2>&1 || true
otx vm delete "$PINNED_VM" --wait --force --wait-timeout "${CREATE_WAIT}s" >/dev/null 2>&1 || true
pass "selective-avoidance scenario complete"

# =======================================================================
# scenario 2: cluster-wide pressure + recovery
# =======================================================================
echo
echo "=== scenario 2: every pool under pressure -> VM stays pending; clearing recovers ==="

# --- 2a: drive the remaining pools under pressure too ------------------
fill_pool "$NODE1"
fill_pool "$NODE3"
wait_pool_pressure "$NODE1" active
wait_pool_pressure "$NODE3" active
# node-2 is still filled from scenario 1; confirm all three report pressure.
[[ "$(pool_pressure "$NODE2")" == "active" ]] || fail "$NODE2 pool pressure unexpectedly cleared"
pass "all three pools under disk pressure"

# --- 2b: a no-hint VM cannot be placed and must stay pending -----------
info "creating $CLUSTER_VM with no node hint (no eligible pool exists)"
otx vm create "$CLUSTER_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" \
  --vcpus 1 --memory-mb 1024 \
  || fail "vm create $CLUSTER_VM was rejected at admission"
wait_vm_reason "$CLUSTER_VM" "node_pressure"
[[ -z "$(vm_node "$CLUSTER_VM")" ]] \
  || fail "$CLUSTER_VM was bound to a node ($(vm_node "$CLUSTER_VM")) while every pool was pressured"
pass "$CLUSTER_VM stayed pending with reason node_pressure (not placed, not failed)"

# --- 2c: clear the pressure; the SAME pending VM must converge ---------
info "removing every fill file; the scheduler must bind $CLUSTER_VM once a pool is eligible again"
for n in "$NODE1" "$NODE2" "$NODE3"; do clear_pool "$n"; done
for n in "$NODE1" "$NODE2" "$NODE3"; do wait_pool_pressure "$n" clear; done
pass "all pools recovered (disk_pressure cleared)"
wait_vm_running "$CLUSTER_VM"
pass "$CLUSTER_VM converged to running after the pressure cleared - retry-forever placement works"

# =======================================================================
# done
# =======================================================================
echo "=== final cleanup ==="
for v in "${ALL_VMS[@]}"; do
  otx vm delete "$v" --wait --force --wait-timeout "${CREATE_WAIT}s" >/dev/null 2>&1 || true
done
for n in "$NODE1" "$NODE2" "$NODE3"; do clear_pool "$n"; done
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== node-placement-pressure smoke PASSED ===${NC}"
echo "  scenario 1: $NODE2 pool pressured -> no-hint VM avoided it, pinned VM stayed pending (pool_not_on_node)"
echo "  scenario 2: all pools pressured -> VM stayed pending (node_pressure), then converged to running on recovery"
echo "OTHERIX_SMOKE_PASS"
