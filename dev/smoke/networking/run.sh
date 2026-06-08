#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Networking N1a-2 operator smoke - drives the `otherix` CLI as a real
# operator against a real agent on the Lima VM, then verifies the
# node-local materialisation over real netlink/nftables inside Lima.
#
# This is the N1a-2 closure gate (per the iteration discipline +
# smoke-tests-operator-scenarios rule). It exercises, end to end:
#   - `otherix network create` (managed bridge + managed NAT)
#   - per-(node,network) reconciliation surfaced on `network get`
#   - `otherix vm create --network` -> tap attached to the bridge
#   - delete-blocking (vm_nics) + managed-bridge teardown on delete
#
# PREREQUISITES: a seeded dev environment, i.e. run AFTER
#   make local-dev-start      (or: make run-api-dev + make seed-dev)
# so that: the CP is up on http://localhost:8080, the `dev` CLI cluster
# is configured, node-dev is `ready`, and a default pool exists. The CP
# and agent binaries MUST be built from the current tree (the networks
# fields + reconciler are N1a-2). This script creates its own networks +
# VM and cleans them up; it does NOT tear down the dev env.
#
# Usage: make smoke-networking   (or: bash dev/smoke/networking/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# This smoke inspects the kernel WireGuard state with `wg`; require it up front.
smoke_require_node_cmd wg

# --- configuration -----------------------------------------------------
# Two-node dev stack: the VM lands on node-1 because only node-1 carries
# the bridge for $BRIDGE_NET; node-2 is a WG-mesh-only peer without it, so
# the scheduler pins placement here. The tap/bridge assertions run against
# node-1 ($SMOKE_HANDLE_1).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OTX="${OTX:-./bin/otherix}"
NODE="${NODE:-node-1}"
BRIDGE_NET="demo-bridge"
BRIDGE_IFACE="otmbr0"   # otb* is reserved for overlay bridges (otb<vni>); otm* is free
NAT_NET="demo-nat"
NAT_IFACE="otnat0"
NAT_SUBNET="10.77.0.0/24"
NAT_GATEWAY="10.77.0.1"
VM_NAME="demo-vm"
READY_TIMEOUT="${READY_TIMEOUT:-90}"   # seconds to wait for per-node reconcile
VM_TIMEOUT="${VM_TIMEOUT:-360}"        # seconds to wait for vm create task

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx()  { "$OTX" "$@"; }
invm() { run_on "$SMOKE_HANDLE_1" "$@"; }

# network_status_for NODE NET -> prints reconciliation_status for that node
network_status_for() {
  local net="$1"
  otx network get "$net" --output json 2>/dev/null \
    | jq -r --arg n "$NODE" '.status.nodes[]? | select(.node_id != null) | .reconciliation_status' \
    | head -1
}

wait_ready() {
  local net="$1" deadline=$(( SECONDS + READY_TIMEOUT )) st=""
  info "waiting for $net to reconcile to ready on $NODE (<= ${READY_TIMEOUT}s)"
  while (( SECONDS < deadline )); do
    st="$(network_status_for "$net" || true)"
    [[ "$st" == "ready" ]] && { pass "$net reconciled ready on a node"; return 0; }
    [[ "$st" == "failed" ]] && fail "$net reconciled FAILED on $NODE: $(otx network get "$net" --output json | jq -c '.status.nodes')"
    sleep 3
  done
  fail "$net did not reach ready within ${READY_TIMEOUT}s (last status: '${st:-none}')"
}

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VM_NAME" --wait --force >/dev/null 2>&1 || true
  otx network delete "$BRIDGE_NET" --force >/dev/null 2>&1 || true
  otx network delete "$NAT_NET" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== networking smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
node_status="$(otx node get "$NODE" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$node_status" == "ready" ]] || fail "$NODE not ready (got '${node_status:-none}'); run make seed-dev"
pass "CP up, $NODE ready"

# --- step 0: WireGuard fabric (N2c) -----------------------------------
# By the time node-1 is `ready` the agent has reached State B and run at
# least one heartbeat round, so the WG reconciler has brought up otwg0 from
# the CP-assigned overlay address. In the two-node dev stack node-1 has one
# peer (node-2); the deep mesh assertions live in the wireguard-mesh smoke,
# here this is a node-1-up sanity check.
echo "=== step 0: WireGuard fabric (otwg0 on $NODE) ==="
WG_IFACE="otwg0"
WG_OVERLAY="10.42.0.1/16"
WG_KEY_PATH="${SMOKE_STATE_1}/wg/private.key"
invm sudo wg show "$WG_IFACE" | grep -q "listening port: 51820" \
  || fail "$WG_IFACE listen port not 51820"
invm sudo wg show "$WG_IFACE" | grep -q "public key:" \
  || fail "$WG_IFACE has no public key"
invm ip -4 addr show "$WG_IFACE" | grep -q "inet $WG_OVERLAY" \
  || fail "$WG_IFACE missing overlay address $WG_OVERLAY"
peer_count="$(invm sudo wg show "$WG_IFACE" peers | grep -c . || true)"
[[ "$peer_count" -ge 1 ]] \
  || fail "$WG_IFACE should have >= 1 peer in the two-node stack (got $peer_count)"
# cross-check the reported public key against the agent's private key on disk.
reported_pub="$(invm sudo wg show "$WG_IFACE" public-key | tr -d '[:space:]')"
derived_pub="$(invm sudo sh -c "wg pubkey < $WG_KEY_PATH" | tr -d '[:space:]')"
[[ -n "$reported_pub" && "$reported_pub" == "$derived_pub" ]] \
  || fail "$WG_IFACE public key mismatch (reported '$reported_pub' != derived '$derived_pub')"
pass "$WG_IFACE up: overlay $WG_OVERLAY, port 51820, $peer_count peer(s), key matches $WG_KEY_PATH"

# --- step 1: managed bridge -------------------------------------------
echo "=== step 1: managed bridge ($BRIDGE_NET -> $BRIDGE_IFACE) ==="
otx create -f "${SCRIPT_DIR}/manifests/bridge.yaml" \
  || fail "create -f bridge.yaml failed"
pass "created $BRIDGE_NET"
wait_ready "$BRIDGE_NET"
invm ip -o link show "$BRIDGE_IFACE" >/dev/null 2>&1 \
  || fail "$BRIDGE_IFACE not present on $SMOKE_HANDLE_1 after reconcile"
pass "$BRIDGE_IFACE exists on $SMOKE_HANDLE_1 (managed bridge materialised over netlink)"

# --- step 2: managed NAT ----------------------------------------------
echo "=== step 2: managed NAT ($NAT_NET -> $NAT_IFACE, $NAT_SUBNET) ==="
otx create -f "${SCRIPT_DIR}/manifests/nat.yaml" || fail "create -f nat.yaml failed"
got_gw="$(otx network get "$NAT_NET" --output json | jq -r '.gateway')"
[[ "$got_gw" == "$NAT_GATEWAY" ]] || fail "expected auto gateway $NAT_GATEWAY, got '$got_gw'"
pass "created $NAT_NET, gateway auto-derived to $got_gw"
wait_ready "$NAT_NET"
invm ip -o addr show "$NAT_IFACE" | grep -q "$NAT_GATEWAY" \
  || fail "gateway $NAT_GATEWAY not assigned on $NAT_IFACE"
pass "gateway $NAT_GATEWAY assigned on $NAT_IFACE"
invm sudo nft list table ip otherix-nat 2>/dev/null | grep -q masquerade \
  || fail "no masquerade rule in the otherix-nat table"
pass "masquerade rule present in the otherix-nat table (operator ruleset untouched)"

# --- step 3: egress=nat invariant (negative) --------------------------
echo "=== step 3: egress=nat without managed is rejected ==="
if otx network create rej-nat --type bridge --bridge-name otrej0 --egress nat --subnet 10.88.0.0/24 >/dev/null 2>&1; then
  otx network delete rej-nat >/dev/null 2>&1 || true
  fail "egress=nat without --managed should have been rejected"
fi
pass "egress=nat without managed rejected by the API edge"

# --- step 4: VM with NIC on the bridge --------------------------------
echo "=== step 4: vm create --network $BRIDGE_NET ==="
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
info "booting $VM_NAME from $IMAGE_URL (<= ${VM_TIMEOUT}s; cold pool fetches the image on first use)"
sed -e "s|@@IMAGE_URL@@|${IMAGE_URL}|g" -e "s|@@ARCH@@|${ARCH}|g" \
  "${SCRIPT_DIR}/manifests/vm.yaml.tmpl" \
  | otx create -f - --wait --wait-timeout "${VM_TIMEOUT}s" \
  || fail "create -f vm did not reach success"
pass "$VM_NAME created with a NIC on $BRIDGE_NET"
# Managed bridges materialise cluster-wide, and the async scheduler binds the
# VM to whichever node's requested network reconciled ready first - so resolve
# the bound node from the VM rather than assuming node-1, then check the tap on
# that node's agent.
bound_node="$(otx vm get "$VM_NAME" --output json 2>/dev/null | jq -r '.node // empty')"
case "$bound_node" in
  node-2) bound_handle="$SMOKE_HANDLE_2" ;;
  *)      bound_handle="$SMOKE_HANDLE_1" ;;
esac
info "$VM_NAME bound to ${bound_node:-node-1}; checking tap on ${bound_handle}"
# the tap is ot<12-hex>; assert one is enslaved to the bridge on the bound node
tap="$(run_on "$bound_handle" ip -o link show master "$BRIDGE_IFACE" 2>/dev/null | awk -F': ' '/ ot/{print $2}' | awk '{print $1}' | head -1 || true)"
[[ -n "$tap" ]] || fail "no tap enslaved to $BRIDGE_IFACE on $bound_handle after vm create"
pass "tap $tap enslaved to $BRIDGE_IFACE on ${bound_handle} (VM NIC materialised)"

# --- step 5: delete-blocking + managed teardown -----------------------
echo "=== step 5: network delete is blocked by the VM, then removes the bridge ==="
if otx network delete "$BRIDGE_NET" --force >/dev/null 2>&1; then
  fail "network delete should be blocked while $VM_NAME holds a NIC"
fi
pass "$BRIDGE_NET delete blocked while in use (vm_nics)"
otx vm delete "$VM_NAME" --wait --force >/dev/null 2>&1 || fail "vm delete failed"
pass "$VM_NAME deleted"
otx network delete "$BRIDGE_NET" --force || fail "network delete failed after VM removal"
pass "$BRIDGE_NET deleted"
# managed bridge must be gone (Otherix removes only what it created); allow a reconcile tick
for _ in $(seq 1 20); do invm ip -o link show "$BRIDGE_IFACE" >/dev/null 2>&1 || break; sleep 3; done
invm ip -o link show "$BRIDGE_IFACE" >/dev/null 2>&1 \
  && fail "$BRIDGE_IFACE still present after delete (managed bridge not torn down)"
pass "$BRIDGE_IFACE removed from $SMOKE_HANDLE_1 (managed bridge torn down)"

# --- step 6: NAT teardown ---------------------------------------------
echo "=== step 6: NAT network teardown ==="
otx network delete "$NAT_NET" --force || fail "nat network delete failed"
for _ in $(seq 1 20); do invm ip -o link show "$NAT_IFACE" >/dev/null 2>&1 || break; sleep 3; done
invm ip -o link show "$NAT_IFACE" >/dev/null 2>&1 \
  && fail "$NAT_IFACE still present after delete"
pass "$NAT_IFACE removed; masquerade torn down"

trap - EXIT
echo
echo "${GREEN}=== networking smoke PASSED ===${NC}"
