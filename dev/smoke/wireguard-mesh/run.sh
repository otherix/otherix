#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Networking N2c-2 cross-agent WireGuard mesh smoke - proves a real
# cross-host WG handshake between two agents on two Lima VMs.
#
# It exercises, end to end:
#   - operator CLI (primary): `otherix node get` surfaces each node's WG
#     fabric block and shows the OTHER node as an established peer;
#   - kernel cross-check: `wg show otwg0 latest-handshakes` on each node
#     shows a recent, non-zero handshake against the other's public key
#     (a real tunnel, not just CP bookkeeping);
#   - datapath: ping the other node's overlay IP across otwg0 (real
#     traffic over WG crypto-routing, allowed_ips /32).
#
# PREREQUISITES: a seeded two-node dev stack, i.e. run AFTER
#   make local-dev-start
# so the CP is up on http://localhost:8080, the `dev` CLI cluster is
# configured, and node-1 (otherix-dev-1) + node-2 (otherix-dev-2) are
# both `ready` with otwg0 up. CP + agent binaries MUST be built from the
# current tree (the WG handshake observation is N2c-2).
#
# Usage: make smoke-wireguard-mesh   (or: bash dev/smoke/wireguard-mesh/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="${NODE1:-node-1}"
NODE2="${NODE2:-node-2}"
WG_IFACE="otwg0"
MESH_TIMEOUT="${MESH_TIMEOUT:-150}"   # seconds to wait for the handshake + heartbeat convergence

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx()   { "$OTX" "$@"; }
invm1() { run_on "$SMOKE_HANDLE_1" "$@"; }
invm2() { run_on "$SMOKE_HANDLE_2" "$@"; }

# wg_field NODE JQ_PATH -> prints the requested field from the node's WG block
wg_field() {
  local node="$1" path="$2"
  otx node get "$node" --output json 2>/dev/null | jq -r "$path"
}

# --- preconditions -----------------------------------------------------
echo "=== wireguard-mesh smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
pass "CP up, $NODE1 + $NODE2 ready"

# --- step 1: otwg0 up on both, overlay IPs match the CP allocation -----
echo "=== step 1: otwg0 up on both nodes (overlay IPs from the CP) ==="
OVL1_CIDR="$(wg_field "$NODE1" '.wireguard.overlay_ip')"
OVL2_CIDR="$(wg_field "$NODE2" '.wireguard.overlay_ip')"
[[ "$OVL1_CIDR" == 10.42.* ]] || fail "$NODE1 overlay_ip not in 10.42/16 (got '${OVL1_CIDR}')"
[[ "$OVL2_CIDR" == 10.42.* ]] || fail "$NODE2 overlay_ip not in 10.42/16 (got '${OVL2_CIDR}')"
[[ "$OVL1_CIDR" != "$OVL2_CIDR" ]] || fail "both nodes share overlay_ip ${OVL1_CIDR}"
OVL1_IP="${OVL1_CIDR%/*}"; OVL2_IP="${OVL2_CIDR%/*}"
invm1 ip -4 addr show "$WG_IFACE" | grep -q "inet $OVL1_CIDR" \
  || fail "$SMOKE_HANDLE_1 $WG_IFACE missing overlay address $OVL1_CIDR"
invm2 ip -4 addr show "$WG_IFACE" | grep -q "inet $OVL2_CIDR" \
  || fail "$SMOKE_HANDLE_2 $WG_IFACE missing overlay address $OVL2_CIDR"
pass "$WG_IFACE up: $NODE1=$OVL1_CIDR, $NODE2=$OVL2_CIDR"

# --- step 2: operator CLI shows the mesh established (primary) ----------
# Each node's WG block must list the OTHER node as an established peer. Poll
# until both directions converge (handshake + heartbeat round-trip).
echo "=== step 2: operator CLI - each node sees the other established ==="
# peer_established SELF PEER -> prints "true" when SELF's WG block lists PEER as
# established. Inlines jq with --arg (wg_field takes a single jq path only).
peer_established() {
  otx node get "$1" --output json 2>/dev/null \
    | jq -r --arg n "$2" '.wireguard.peers[]? | select(.node_name==$n) | .established'
}
established_both() {
  [[ "$(peer_established "$NODE1" "$NODE2")" == "true" \
     && "$(peer_established "$NODE2" "$NODE1")" == "true" ]]
}
info "waiting for the mesh to establish both directions (<= ${MESH_TIMEOUT}s)"
deadline=$(( SECONDS + MESH_TIMEOUT ))
while (( SECONDS < deadline )); do
  if established_both; then
    pass "$NODE1 <-> $NODE2 established (via otherix node get)"
    break
  fi
  sleep 5
done
if ! established_both; then
  echo "--- $NODE1 wireguard block ---"; otx node get "$NODE1" --output json | jq '.wireguard'
  echo "--- $NODE2 wireguard block ---"; otx node get "$NODE2" --output json | jq '.wireguard'
  fail "mesh did not establish both directions within ${MESH_TIMEOUT}s"
fi

# --- step 3: kernel cross-check (real tunnel, not CP bookkeeping) -------
echo "=== step 3: kernel handshake cross-check (wg show latest-handshakes) ==="
PUB1="$(wg_field "$NODE1" '.wireguard.public_key')"
PUB2="$(wg_field "$NODE2" '.wireguard.public_key')"
[[ -n "$PUB1" && "$PUB1" != "null" ]] || fail "$NODE1 has no public_key in its WG block"
[[ -n "$PUB2" && "$PUB2" != "null" ]] || fail "$NODE2 has no public_key in its WG block"

# handshake_age VM PEERPUB -> prints seconds since the last handshake with PEERPUB
# (the `latest-handshakes` value is a UNIX timestamp; 0 means never).
handshake_recent() {
  local who="$1" peerpub="$2" ts now
  ts="$($who sudo wg show "$WG_IFACE" latest-handshakes | awk -v k="$peerpub" '$1==k {print $2}')"
  [[ -n "$ts" && "$ts" != "0" ]] || return 1
  now="$($who date +%s)"
  (( now - ts < 180 ))
}
handshake_recent invm1 "$PUB2" || fail "$SMOKE_HANDLE_1 has no recent handshake with $NODE2 ($PUB2)"
handshake_recent invm2 "$PUB1" || fail "$SMOKE_HANDLE_2 has no recent handshake with $NODE1 ($PUB1)"
pass "real WG handshake in the kernel both directions (latest-handshakes < 180s)"

# --- step 4: overlay datapath (ping across otwg0) ----------------------
echo "=== step 4: overlay datapath - ping across otwg0 ==="
invm1 ping -c 3 -W 2 "$OVL2_IP" >/dev/null 2>&1 \
  || fail "$SMOKE_HANDLE_1 cannot ping $NODE2 overlay $OVL2_IP across $WG_IFACE"
pass "$NODE1 -> $NODE2 overlay ping ($OVL1_IP -> $OVL2_IP)"
invm2 ping -c 3 -W 2 "$OVL1_IP" >/dev/null 2>&1 \
  || fail "$SMOKE_HANDLE_2 cannot ping $NODE1 overlay $OVL1_IP across $WG_IFACE"
pass "$NODE2 -> $NODE1 overlay ping ($OVL2_IP -> $OVL1_IP)"

echo
echo "${GREEN}=== wireguard-mesh smoke PASSED ===${NC}"
