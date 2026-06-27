#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# DHCP-overlay teardown smoke - proves that repeatedly creating and deleting a
# dhcp-enabled overlay network never wedges the agent's network reconciler.
#
# The bug this guards against: deleting a dhcp-enabled overlay deregistered the
# per-node DHCP responder, whose read-loop was a bare blocking Recvfrom on a raw
# AF_PACKET socket with no deadline. On a quiet bridge (no VM, no DHCP traffic)
# it parked forever, and closing the fd could not unblock it, so teardown
# (DeregisterNetwork -> stopServer -> <-done) hung and permanently darkened ALL
# network reconciliation on that agent. The user-visible symptom was that after
# a few create/delete cycles a fresh `network create` never materialised
# (status.nodes stayed []), and the otvb<vni> bridge never appeared.
#
# What it proves, end to end, as a real operator (CLI + manifest):
#   - `otherix create -f` a dhcp overlay -> it reconciles ready on BOTH nodes and
#     the otvb<vni> bridge materialises (the DHCP responder socket is open);
#   - `otherix network delete` -> the otvb<vni> bridge is torn down on both nodes;
#   - repeated across several cycles, then a final create still reconciles ready.
#     On the pre-fix agent the FIRST delete wedges the reconciler, so the next
#     create's wait-ready (and bridge-present check) times out -> the smoke fails.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make local-dev-start
# Both nodes ready and otwg0 up (overlay networks ride the WireGuard underlay).
#
# Usage: make smoke-dhcp-overlay-teardown
#   (or: bash dev/smoke/dhcp-overlay-teardown/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"
NET="dhcp-teardown"                       # must match metadata.name in the manifest
CYCLES="${CYCLES:-3}"                      # create/delete repetitions before the final create
READY_TIMEOUT="${READY_TIMEOUT:-90}"      # seconds to wait for per-node reconcile -> ready
GONE_TIMEOUT="${GONE_TIMEOUT:-90}"        # seconds to wait for the otvb<vni> bridge to disappear

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx()   { "$OTX" "$@"; }
invm1() { run_on "$SMOKE_HANDLE_1" "$@"; }
invm2() { run_on "$SMOKE_HANDLE_2" "$@"; }

# net_ready_both NET -> 0 when the network reconciled to "ready" on both nodes.
net_ready_both() {
  local net="$1" n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx network get "$net" --output json 2>/dev/null \
        | jq -r --arg n "$n" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
        == "ready" ]] || return 1
  done
  return 0
}

# wait_ready_both NET -> poll net_ready_both until the deadline; fail on timeout.
# This is the teeth: on a wedged reconciler status.nodes never reaches ready.
wait_ready_both() {
  local net="$1" deadline=$(( SECONDS + READY_TIMEOUT ))
  while (( SECONDS < deadline )); do
    net_ready_both "$net" && return 0
    sleep 3
  done
  otx network get "$net" --output json 2>/dev/null | jq -c '.status.nodes' >&2 || true
  fail "$net did not reconcile ready on both nodes within ${READY_TIMEOUT}s (reconciler wedged?)"
}

# bridge_present HANDLE IFACE -> 0 if the link exists on that node.
bridge_present() { run_on "$1" ip -o link show "$2" >/dev/null 2>&1; }

# wait_bridge_gone HANDLE IFACE -> poll until the link is gone on that node; fail
# on timeout. A wedged reconciler also never tears the bridge down.
wait_bridge_gone() {
  local handle="$1" iface="$2" deadline=$(( SECONDS + GONE_TIMEOUT ))
  while (( SECONDS < deadline )); do
    bridge_present "$handle" "$iface" || return 0
    sleep 3
  done
  fail "$iface still present on $handle after delete within ${GONE_TIMEOUT}s (teardown wedged?)"
}

cleanup() {
  echo "--- cleanup ---"
  otx network delete "$NET" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== dhcp-overlay-teardown smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
# best-effort delete-first so a stale leftover from a prior run does not 409
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "CP up, both nodes ready"

# --- repeated create/delete cycles -------------------------------------
# Each cycle: create the dhcp overlay, prove it reconciled ready on both nodes
# AND the otvb<vni> bridge (hence the open, parked DHCP responder socket) exists,
# then delete it and prove the bridge is torn down on both nodes. A wedge on any
# delete shows up as a timeout on the NEXT cycle's create.
for (( i = 1; i <= CYCLES; i++ )); do
  echo "=== cycle ${i}/${CYCLES}: create + delete dhcp overlay ==="
  otx create -f "${SCRIPT_DIR}/manifests/overlay-dhcp.yaml" || fail "cycle ${i}: create -f failed"
  wait_ready_both "$NET"
  VNI="$(otx network get "$NET" --output json | jq -r '.vni')"
  [[ "$VNI" =~ ^[0-9]+$ ]] || fail "cycle ${i}: no vni on $NET after create"
  BR="otb${VNI}"
  bridge_present "$SMOKE_HANDLE_1" "$BR" || fail "cycle ${i}: $BR absent on $NODE1 after reconcile ready"
  bridge_present "$SMOKE_HANDLE_2" "$BR" || fail "cycle ${i}: $BR absent on $NODE2 after reconcile ready"
  pass "cycle ${i}: $NET ready on both nodes, $BR present (DHCP responder socket open + parked)"

  # Let the responder's recv settle into its blocking Recvfrom on the now-quiet
  # bridge - this is the precise state that wedged teardown before the fix.
  sleep 2

  otx network delete "$NET" --force || fail "cycle ${i}: network delete failed"
  wait_bridge_gone "$SMOKE_HANDLE_1" "$BR"
  wait_bridge_gone "$SMOKE_HANDLE_2" "$BR"
  pass "cycle ${i}: $NET deleted, $BR torn down on both nodes (teardown did not wedge)"
done

# --- final create: the reconciler must still be alive -------------------
# This is the assertion the pre-fix agent fails: after the cycles above wedged
# the reconciler, this create never materialised.
echo "=== final create: reconciler still alive after ${CYCLES} teardown cycles ==="
otx create -f "${SCRIPT_DIR}/manifests/overlay-dhcp.yaml" || fail "final create -f failed"
wait_ready_both "$NET"
VNI="$(otx network get "$NET" --output json | jq -r '.vni')"
BR="otb${VNI}"
bridge_present "$SMOKE_HANDLE_1" "$BR" || fail "final: $BR absent on $NODE1"
bridge_present "$SMOKE_HANDLE_2" "$BR" || fail "final: $BR absent on $NODE2"
pass "final $NET ready on both nodes, $BR present (network reconciler healthy after repeated dhcp teardown)"

otx network delete "$NET" --force >/dev/null 2>&1 || true
trap - EXIT
echo
echo "${GREEN}=== dhcp-overlay-teardown smoke PASSED ===${NC}"
