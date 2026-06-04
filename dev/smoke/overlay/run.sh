#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Networking N3b overlay materialization smoke - proves the agent materializes
# a type=overlay network on both nodes and that the VXLAN datapath traverses the
# encrypted WireGuard underlay cross-node.
#
# It exercises, end to end:
#   - operator CLI (primary): `otherix network create --type overlay` allocates a
#     VNI and the otb<vni> bridge name;
#   - kernel cross-check on BOTH nodes: the otvx<vni> VTEP is bound to the node's
#     otwg0 overlay IP (NOT 127.0.0.1 - this is the fail-closed proof), learning
#     off, dstport 4789, inner MTU 1390; otwg0 MTU is 1440; the otb<vni> bridge
#     is up at MTU 1390 with the VTEP enslaved;
#   - datapath: a TEMPORARY manual FDB entry (peer MAC -> peer otwg0 IP) + a
#     throwaway overlay-subnet IP on each otb<vni> lets node-1 ping node-2 across
#     the VXLAN. This is N3b scaffolding (torn down after); automated per-MAC FDB
#     and VM-to-VM ping are N3c.
#
# PREREQUISITES: a seeded two-node dev stack:
#   make local-dev-start
# CP + agent binaries MUST be built from the current tree.
#
# Usage: make smoke-overlay   (or: bash dev/smoke/overlay/run.sh)

set -euo pipefail

OTX="${OTX:-./bin/otherix}"
VM1="${VM1:-otherix-dev-1}"
VM2="${VM2:-otherix-dev-2}"
NODE1="${NODE1:-node-1}"
NODE2="${NODE2:-node-2}"
WG_IFACE="otwg0"
SUBNET="${SUBNET:-10.60.0.0/24}"
TEST_IP1="${TEST_IP1:-10.60.0.1}"
TEST_IP2="${TEST_IP2:-10.60.0.2}"
DEADLINE="${DEADLINE:-90}"   # seconds to wait for both nodes to materialize the overlay

RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx()   { "$OTX" "$@"; }
invm1() { limactl shell "$VM1" -- "$@"; }
invm2() { limactl shell "$VM2" -- "$@"; }

# overlay_ip NODE -> prints the node's otwg0 overlay host IP (no /prefix)
overlay_ip() { otx node get "$1" --output json 2>/dev/null | jq -r '.wireguard.overlay_ip' | cut -d/ -f1; }

# --- preconditions -----------------------------------------------------
echo "=== overlay smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v limactl >/dev/null || fail "limactl is required"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
OVL1="$(overlay_ip "$NODE1")"; OVL2="$(overlay_ip "$NODE2")"
[[ "$OVL1" == 10.42.* && "$OVL2" == 10.42.* ]] || fail "otwg0 overlay IPs not in 10.42/16 ($OVL1, $OVL2)"
pass "CP up; $NODE1=$OVL1, $NODE2=$OVL2 ready"

# --- step 1: operator creates the overlay (CLI) ------------------------
echo "=== step 1: operator creates a type=overlay network ==="
NAME="ov-smoke-$$"
created="$(otx network create "$NAME" --type overlay --subnet "$SUBNET" --output json)" \
  || fail "network create failed"
VNI="$(echo "$created" | jq -r '.vni')"
BR="$(echo "$created" | jq -r '.bridge_name')"
NETID="$(echo "$created" | jq -r '.id')"
[[ "$VNI" =~ ^[0-9]+$ ]] || fail "no vni in create response: $created"
[[ "$BR" == "otb$VNI" ]] || fail "bridge_name=$BR, want otb$VNI"
VTEP="otvx$VNI"
pass "overlay created: vni=$VNI bridge=$BR"

# --- step 2: both nodes materialize the device (poll) ------------------
echo "=== step 2: both nodes materialize VTEP+bridge (fail-closed attrs) ==="
# assert_node VM OVL_IP -> verifies the device invariants in the VM
assert_node() {
  local vm="$1" ovl="$2"
  limactl shell "$vm" -- ip -d link show "$VTEP" 2>/dev/null | grep -q "vxlan id $VNI " || return 1
  limactl shell "$vm" -- ip -d link show "$VTEP" | grep -q "local $ovl " || return 1
  limactl shell "$vm" -- ip -d link show "$VTEP" | grep -q "dstport 4789" || return 1
  limactl shell "$vm" -- ip -d link show "$VTEP" | grep -q "nolearning" || return 1
  limactl shell "$vm" -- ip link show "$VTEP" | grep -q "mtu 1390" || return 1
  limactl shell "$vm" -- ip link show "$WG_IFACE" | grep -q "mtu 1440" || return 1
  limactl shell "$vm" -- ip link show "$BR" | grep -Eq "mtu 1390.*state (UP|UNKNOWN)" || return 1
  limactl shell "$vm" -- ip -o link show master "$BR" | grep -q "$VTEP" || return 1
  return 0
}
info "waiting for both nodes to materialize (<= ${DEADLINE}s)"
deadline=$(( SECONDS + DEADLINE )); ok=0
while (( SECONDS < deadline )); do
  if assert_node "$VM1" "$OVL1" && assert_node "$VM2" "$OVL2"; then ok=1; break; fi
  sleep 5
done
(( ok == 1 )) || { invm1 ip -d link show "$VTEP" || true; fail "overlay not materialized on both nodes within ${DEADLINE}s"; }
pass "both nodes: $VTEP bound to otwg0, learning off, mtu 1390; otwg0 mtu 1440; $BR up; VTEP enslaved"

# --- step 3: manual-FDB cross-node datapath (throwaway scaffolding) -----
echo "=== step 3: manual-FDB cross-node VXLAN datapath ==="
MAC1="$(invm1 cat /sys/class/net/"$BR"/address)"
MAC2="$(invm2 cat /sys/class/net/"$BR"/address)"
cleanup() {
  invm1 sudo ip neigh del "$TEST_IP2" dev "$BR" 2>/dev/null || true
  invm2 sudo ip neigh del "$TEST_IP1" dev "$BR" 2>/dev/null || true
  invm1 sudo ip addr del "$TEST_IP1/24" dev "$BR" 2>/dev/null || true
  invm2 sudo ip addr del "$TEST_IP2/24" dev "$BR" 2>/dev/null || true
  invm1 sudo bridge fdb del "$MAC2" dev "$VTEP" dst "$OVL2" 2>/dev/null || true
  invm2 sudo bridge fdb del "$MAC1" dev "$VTEP" dst "$OVL1" 2>/dev/null || true
}
trap cleanup EXIT
invm1 sudo ip addr add "$TEST_IP1/24" dev "$BR"
invm2 sudo ip addr add "$TEST_IP2/24" dev "$BR"
# Unicast-only datapath proof: a controller-authoritative FDB entry (peer bridge
# MAC -> peer otwg0 IP) plus a static ARP/neigh entry, so the ping needs no
# broadcast (VXLAN learning is off and there is no BUM/flood entry until N3c).
invm1 sudo bridge fdb append "$MAC2" dev "$VTEP" dst "$OVL2"
invm2 sudo bridge fdb append "$MAC1" dev "$VTEP" dst "$OVL1"
invm1 sudo ip neigh replace "$TEST_IP2" lladdr "$MAC2" dev "$BR" nud permanent
invm2 sudo ip neigh replace "$TEST_IP1" lladdr "$MAC1" dev "$BR" nud permanent
invm1 ping -c 3 -W 2 "$TEST_IP2" >/dev/null 2>&1 \
  || fail "$NODE1 cannot ping $NODE2 across the overlay ($TEST_IP1 -> $TEST_IP2)"
pass "cross-node VXLAN datapath: $TEST_IP1 -> $TEST_IP2 over otwg0"
cleanup; trap - EXIT

# --- step 4: operator deletes the overlay; devices removed -------------
echo "=== step 4: operator deletes the overlay; teardown on both nodes ==="
otx network delete "$NETID" --force >/dev/null || fail "network delete failed"
deadline=$(( SECONDS + DEADLINE )); gone=0
while (( SECONDS < deadline )); do
  if ! invm1 ip link show "$VTEP" >/dev/null 2>&1 && ! invm2 ip link show "$VTEP" >/dev/null 2>&1 \
     && ! invm1 ip link show "$BR" >/dev/null 2>&1 && ! invm2 ip link show "$BR" >/dev/null 2>&1; then
    gone=1; break
  fi
  sleep 5
done
(( gone == 1 )) || fail "overlay devices not torn down on both nodes within ${DEADLINE}s"
pass "$VTEP + $BR removed on both nodes"

echo
echo "${GREEN}=== overlay smoke PASSED ===${NC}"
