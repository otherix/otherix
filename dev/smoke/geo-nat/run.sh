#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Geo NAT smoke - proves the lighthouse relay end to end on the Lima dev stack:
# two NAT'd hypervisor nodes and one public gateway node, with
#
#   1. the CP driving BOTH NAT'd nodes' control planes through the gateway's
#      /v1/connect-node splice (a VM reaches "running" on each NAT'd node, which
#      is impossible unless the create task, image materialisation, qemu launch
#      and QMP all traversed the splice and were reported back by heartbeat);
#   2. cross-site GUEST overlay traffic RELAYED hub-and-spoke through the gateway
#      (the two NAT'd nodes have NO direct WireGuard tunnel, so VM-A on node-1
#      reaching VM-B on node-2 can only go through node-3);
#   3. CP-mediated serial streaming (otherix vm logs) proxied through the splice
#      to a NAT'd node - kubectl-style CLI -> CP -> NAT'd agent, never a direct
#      node dial (the same bytes step 2's guest wrote, read back through the CP);
#   4. the down-path schedulability gate excluding a NAT'd node when its gateway
#      goes away, while the node's own status stays "ready".
#
# NAT emulation without netns: WireGuard cannot form a direct tunnel between two
# peers that BOTH advertise no endpoint, so clearing wireguard.advertised_endpoint
# on node-1 and node-2 forces all of their inter-site traffic through the public
# gateway - the functional essence of CGNAT, no nested namespaces required.
#
# Dev-stack fixture note (why node-2 is repatched): a real operator bootstraps a
# NAT'd node with an advertised CONTROL endpoint on the cluster-uniform control
# port (9443). The dev seed instead points each node's advertised endpoint at a
# DISTINCT per-node Lima host-forward port (node-1 :9443, node-2 :9444, node-3
# :9445) so the host can reach each node directly in the normal PUBLIC stack. The
# gateway splice validates the CP-supplied target against its own (uniform)
# control port, and every node's overlay control listener binds that same port,
# so a NAT'd target must advertise :9443. node-1 already does; node-2's CP-side
# advertised endpoint is repatched from :9444 to :9443 here - a one-field,
# reversible edit of the embedded dev etcd that emulates a correctly
# geo-bootstrapped node without a destructive delete + re-bootstrap. Only the port
# is used for the splice target; the host is never dialled for a NAT'd node.
#
# PREREQUISITES: a fresh three-node Lima dev stack built + deployed from the
# CURRENT tree (the co-located gateway must serve /v1/connect-node, which older
# agent binaries did not):
#   make local-dev-deploy      # rebuild + redeploy the fixed agent to all nodes
# (or make local-dev-cleanrestart after a make build). Then:
#   make smoke-geo-nat         # or: bash dev/smoke/geo-nat/run.sh
#
# The smoke reconfigures the live stack into the geo topology and restores it on
# exit (configs, gateway role, and the etcd patch are reverted); a
# local-dev-cleanrestart returns to a pristine public stack in any case.

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"; NODE2="node-2"; GW="node-3"   # node-1/2 NAT'd, node-3 public gateway
NET="geonat-smoke"                            # fixed; delete-firsted on start
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CONTROL_PORT="9443"                           # cluster-uniform agent control port
GW_INGRESS_PORT="9444"                         # co-located gateway ingress listener port
ETCD="${ETCD:-http://127.0.0.1:2379}"          # embedded dev etcd (plain HTTP, dev only)

CONVERGE_WAIT="${CONVERGE_WAIT:-180}"          # mesh + relay wiring
RELAY_PING_WAIT="${RELAY_PING_WAIT:-60}"       # node-level overlay ping through the relay
VM_WAIT="${VM_WAIT:-420}"                       # per-VM create (incl. cold image fetch)
PING_WAIT="${PING_WAIT:-720}"                   # guest OVERLAY_PING_OK sentinel
GATE_WAIT="${GATE_WAIT:-40}"                     # down-path gate: time a pinned create stays unplaced

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx()  { "$OTX" "$@"; }
invm1() { run_on "$SMOKE_HANDLE_1" "$@"; }
invm2() { run_on "$SMOKE_HANDLE_2" "$@"; }

agent_yaml() { printf '/etc/otherix/agent.yaml'; }                       # Lima path
restart_node() { run_on "$(smoke_handle "$1")" sudo systemctl restart otherix-agent; }
node_ip() { run_on "$(smoke_handle "$1")" sh -c "ip -4 -o addr show | grep -oE '192\.168\.[0-9]+\.[0-9]+' | head -1"; }
node_status() { otx node get "$1" --output json 2>/dev/null | jq -r '.status'; }
overlay_ip()  { otx node get "$1" --output json 2>/dev/null | jq -r '.wireguard.overlay_ip' | cut -d/ -f1; }
node_id_of()  { otx node get "$1" --output json 2>/dev/null | jq -r '.id'; }
etcdget() { etcdctl --endpoints="$ETCD" get "$1" --print-value-only 2>/dev/null; }
etcdput() { etcdctl --endpoints="$ETCD" put "$1" "$2" >/dev/null 2>&1; }

# back up agent.yaml once per node so a re-run never stacks edits, then work from
# the clean copy.
backup_yaml() {
  local h y; h="$(smoke_handle "$1")"; y="$(agent_yaml)"
  run_on "$h" sudo sh -c "test -f '${y}.geo-bak' || cp '${y}' '${y}.geo-bak'"
  run_on "$h" sudo cp "${y}.geo-bak" "${y}"
}
restore_yaml() {
  local h y; h="$(smoke_handle "$1")"; y="$(agent_yaml)"
  run_on "$h" sudo sh -c "test -f '${y}.geo-bak' && cp '${y}.geo-bak' '${y}' || true" 2>/dev/null || true
}

# make_natd IDX - clear the node's WireGuard advertised endpoint (no direct
# tunnel; peers reach it only by roaming through the gateway).
make_natd() {
  local h y; h="$(smoke_handle "$1")"; y="$(agent_yaml)"
  backup_yaml "$1"
  run_on "$h" sudo sed -i -E 's|^([[:space:]]*advertised_endpoint:)[[:space:]]*".*:51820"|\1 ""|' "$y"
}

# make_gateway - CP gateway role + co-located gateway config block + restart. The
# co-located node keeps its single node identity's WireGuard key; the distinct
# standalone-gateway key must be absent or it would present a public key the CP
# has registered to a gone standalone gateway.
GW_IP=""
make_gateway() {
  local h y; h="$(smoke_handle 3)"; y="$(agent_yaml)"
  GW_IP="$(node_ip 3)"; [ -n "$GW_IP" ] || fail "could not resolve $GW node IP"
  otx node gateway enable "$GW" >/dev/null
  backup_yaml 3
  printf 'gateway:\n  enabled: false\n  listen: "0.0.0.0:%s"\n  advertised_endpoint: "https://%s:%s"\n' \
    "$GW_INGRESS_PORT" "$GW_IP" "$GW_INGRESS_PORT" | run_on "$h" sudo tee -a "$y" >/dev/null
  run_on "$h" sudo rm -f /var/lib/otherix/wg-gateway/private.key >/dev/null 2>&1 || true
}

# repatch_advertised_port IDX PORT - rewrite the node's CP-side AdvertisedEndpoint
# port in the embedded dev etcd (see the fixture note in the header). Returns the
# ORIGINAL endpoint on stdout so the caller can restore it.
repatch_advertised_port() {
  local name="$1" port="$2" id key row orig new
  id="$(node_id_of "$name")"; [ -n "$id" ] && [ "$id" != "null" ] || { echo ""; return 1; }
  key="/otherix/nodes/${id}"
  row="$(etcdget "$key")"; [ -n "$row" ] || { echo ""; return 1; }
  orig="$(echo "$row" | jq -r '.AdvertisedEndpoint')"
  new="$(echo "$row" | jq -c --arg e "https://127.0.0.1:${port}" '.AdvertisedEndpoint=$e')"
  etcdput "$key" "$new" || { echo ""; return 1; }
  echo "$orig"
}

NODE2_ORIG_ENDPOINT=""
GATEWAY_ENABLED=0
cleanup() {
  echo "--- cleanup ---"
  otx vm delete "${NET}-a" --wait --force >/dev/null 2>&1 || true
  otx vm delete "${NET}-b" --wait --force >/dev/null 2>&1 || true
  otx vm delete "${NET}-gate" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  # restore node-2's advertised endpoint (best-effort)
  if [ -n "$NODE2_ORIG_ENDPOINT" ]; then
    local id; id="$(node_id_of "$NODE2")"
    if [ -n "$id" ] && [ "$id" != "null" ]; then
      local row; row="$(etcdget "/otherix/nodes/${id}")"
      [ -n "$row" ] && etcdput "/otherix/nodes/${id}" \
        "$(echo "$row" | jq -c --arg e "$NODE2_ORIG_ENDPOINT" '.AdvertisedEndpoint=$e')"
    fi
  fi
  [ "$GATEWAY_ENABLED" = 1 ] && otx node gateway disable "$GW" >/dev/null 2>&1 || true
  restore_yaml 1; restore_yaml 2; restore_yaml 3
  restart_node 1 >/dev/null 2>&1 || true
  restart_node 2 >/dev/null 2>&1 || true
  restart_node 3 >/dev/null 2>&1 || true
  info "stack reconfigured back toward public; a local-dev-cleanrestart guarantees a pristine stack"
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== geo-nat smoke: preconditions ==="
[ "${SMOKE_PLATFORM}" = "lima" ] || fail "geo-nat smoke is Lima-only (NAT emulation relies on the Lima topology); netns CGNAT is a separate CI smoke"
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (dev etcd fixture)"
cp_ready || fail "CP not up on :8080 (run make local-dev-deploy)"
etcdget /otherix/nodes >/dev/null 2>&1 || etcdctl --endpoints="$ETCD" endpoint health >/dev/null 2>&1 || fail "dev etcd not reachable at $ETCD"
for n in "$NODE1" "$NODE2" "$GW"; do
  [ "$(node_status "$n")" = "ready" ] || fail "$n not ready; run make local-dev-deploy"
done
pass "CP up ($(cp_version)); $NODE1, $NODE2, $GW ready"

# --- step 1: reconfigure into the geo topology -------------------------
echo "=== step 1: reconfigure - $GW gateway, $NODE1 + $NODE2 NAT'd ==="
cleanup >/dev/null 2>&1 || true   # clear stale VMs/net from a prior run (keeps backups)
make_gateway; GATEWAY_ENABLED=1
make_natd 1
make_natd 2
NODE2_ORIG_ENDPOINT="$(repatch_advertised_port "$NODE2" "$CONTROL_PORT")" \
  || fail "could not repatch $NODE2 advertised port in etcd"
info "$NODE2 advertised endpoint: ${NODE2_ORIG_ENDPOINT} -> https://127.0.0.1:${CONTROL_PORT}"
restart_node 1; restart_node 2; restart_node 3
pass "$GW is the co-located gateway; $NODE1 + $NODE2 are NAT'd (advertised control :${CONTROL_PORT})"

# --- step 2: mesh + relay convergence, node-level relay ping -----------
echo "=== step 2: wait for the hub-and-spoke relay to wire, then ping node->node through it ==="
info "waiting <= ${CONVERGE_WAIT}s for $NODE1/$NODE2 to route each other's overlay /32 via $GW"
OVL1=""; OVL2=""
deadline=$(( SECONDS + CONVERGE_WAIT )); conv=0
while (( SECONDS < deadline )); do
  OVL1="$(overlay_ip "$NODE1")"; OVL2="$(overlay_ip "$NODE2")"
  if [[ "$OVL1" == 10.42.* && "$OVL2" == 10.42.* ]]; then
    # node-1 must carry node-2's /32 in the gateway peer's allowed-ips (relayed), and vice versa.
    if invm1 sudo wg show otwg0 allowed-ips 2>/dev/null | grep -q "${OVL2}/32" \
       && invm2 sudo wg show otwg0 allowed-ips 2>/dev/null | grep -q "${OVL1}/32"; then
      conv=1; break
    fi
  fi
  sleep 5
done
(( conv == 1 )) || fail "relay routing not wired within ${CONVERGE_WAIT}s ($NODE1=$OVL1 $NODE2=$OVL2)"
pass "relay wired: $NODE1=$OVL1 and $NODE2=$OVL2 each route the other via $GW"

# The two NAT'd nodes have no direct tunnel; an overlay ping between them can only
# traverse the gateway relay. (The Lima nodes are full Ubuntu, so ping exists.)
info "node-level overlay ping $NODE1 ($OVL1) -> $NODE2 ($OVL2) through the relay (<= ${RELAY_PING_WAIT}s)"
deadline=$(( SECONDS + RELAY_PING_WAIT )); relay=0
while (( SECONDS < deadline )); do
  if invm1 ping -c1 -W2 "$OVL2" >/dev/null 2>&1; then relay=1; break; fi
  sleep 3
done
(( relay == 1 )) || fail "node-level overlay ping through the relay failed - the WireGuard relay data plane is down"
pass "node-level overlay ping through the relay succeeded (no direct $NODE1<->$NODE2 tunnel)"

# --- step 3: control splice - a VM on each NAT'd node reaches running ---
echo "=== step 3: CP drives a VM onto each NAT'd node THROUGH the gateway splice ==="
otx create -f "${SCRIPT_DIR}/manifests/overlay.yaml" || fail "create -f overlay.yaml failed"
VNI="$(otx network get "$NET" --output json | jq -r '.vni')"
[[ "$VNI" =~ ^[0-9]+$ ]] || fail "no vni on $NET after create"
pass "overlay $NET created (vni=$VNI)"

WAIT_BOTH=$(( VM_WAIT * 4 ))   # two VMs, cold-image budget each, shared --wait
info "creating ${NET}-a on $NODE1 and ${NET}-b on $NODE2 via the splice (<= ${WAIT_BOTH}s)"
sed -e "s|@@IMAGE_URL@@|${IMAGE_URL}|g" -e "s|@@ARCH@@|${ARCH}|g" \
  "${SCRIPT_DIR}/manifests/vms.yaml.tmpl" \
  | otx create -f - --wait --wait-timeout "${WAIT_BOTH}s" \
  || fail "VM create did not reach running on both NAT'd nodes (control splice failed)"
pass "both VMs reached running on NAT'd nodes - CP->NAT'd control splice works on $NODE1 AND $NODE2"

VMID_A="$(otx vm get "${NET}-a" --output json | jq -r '.id')"
info "VM-A id=$VMID_A (probes VM-B across the relay)"

# --- step 4: guest cross-site reachability through the relay ------------
echo "=== step 4: guest VM-A ($NODE1) -> VM-B ($NODE2) over the relayed overlay ==="
SERIAL_A="${SMOKE_STATE_1}/vms/${VMID_A}/serial.log"
info "watching $SERIAL_A for OVERLAY_PING_OK (<= ${PING_WAIT}s)"
deadline=$(( SECONDS + PING_WAIT )); seen=0
while (( SECONDS < deadline )); do
  if invm1 sudo grep -q "OVERLAY_PING_OK" "$SERIAL_A" 2>/dev/null; then seen=1; break; fi
  sleep 5
done
if (( seen != 1 )); then
  echo "--- VM-A serial tail ---"; invm1 sudo tail -30 "$SERIAL_A" 2>/dev/null || true
  fail "no OVERLAY_PING_OK - guest cross-site relay reachability did not succeed"
fi
pass "OVERLAY_PING_OK: guest on $NODE1 reached guest on $NODE2 hub-and-spoke through $GW"

# --- step 4b: the SAME serial, but CP-mediated (kubectl-style) ----------
# step 4 read serial.log directly on the node; this proves the OPERATOR path:
# `otherix vm logs` -> CP -> NAT'd agent over the gateway splice returns the same
# bytes. Both nodes are NAT'd, so a successful dump can only have traversed the
# splice - a direct CP->node dial has no route. Capture-then-grep so pipefail +
# an early grep -q SIGPIPE cannot mask a real match.
echo "=== step 4b: otherix vm logs through the CP splice (NAT'd nodes, kubectl-style) ==="
info "otherix vm logs ${NET}-a: CP -> $NODE1 (NAT'd) over the splice, must return the guest serial"
la_deadline=$(( SECONDS + 45 )); la_ok=0
while (( SECONDS < la_deadline )); do
  logs_a="$(otx vm logs "${NET}-a" 2>/dev/null || true)"
  if printf '%s\n' "$logs_a" | grep -q "OVERLAY_PING_OK"; then la_ok=1; break; fi
  sleep 3
done
(( la_ok == 1 )) || fail "otherix vm logs ${NET}-a returned no OVERLAY_PING_OK - CP-mediated logs to a NAT'd node is broken"
info "otherix vm logs ${NET}-b: CP -> $NODE2 (NAT'd) over the splice"
lb_deadline=$(( SECONDS + 45 )); lb_ok=0
while (( SECONDS < lb_deadline )); do
  logs_b="$(otx vm logs "${NET}-b" 2>/dev/null || true)"
  if printf '%s\n' "$logs_b" | grep -q "SMOKE_NET"; then lb_ok=1; break; fi
  sleep 3
done
(( lb_ok == 1 )) || fail "otherix vm logs ${NET}-b returned no serial - CP-mediated logs to a NAT'd node is broken"
pass "otherix vm logs relayed BOTH NAT'd nodes' serial through the CP splice (kubectl-style, no direct node access)"

# --- step 5: down-path schedulability gate -----------------------------
echo "=== step 5: down-path gate - remove the gateway, a NAT'd node becomes unschedulable while still 'ready' ==="
otx node gateway disable "$GW" >/dev/null || fail "could not disable gateway role on $GW"
GATEWAY_ENABLED=0
info "gateway role removed; a VM pinned to $NODE1 must NOT get placed (no reachable public gateway)"
# Accept 409 (no eligible node) at submit OR a create that never leaves pending.
if otx vm create "${NET}-gate" --node "$NODE1" --network "$NET" \
     --image-url "$IMAGE_URL" --arch "$ARCH" >/dev/null 2>&1; then
  deadline=$(( SECONDS + GATE_WAIT )); placed=0
  while (( SECONDS < deadline )); do
    ph="$(otx vm get "${NET}-gate" --output json 2>/dev/null | jq -r '.status.phase')"
    [[ "$ph" == "running" ]] && { placed=1; break; }
    sleep 3
  done
  (( placed == 0 )) || fail "down-path gate leaked: ${NET}-gate reached running with no gateway"
  info "${NET}-gate stayed unplaced with the gateway down (phase=$(otx vm get "${NET}-gate" --output json 2>/dev/null | jq -r '.status.phase'))"
else
  info "vm create pinned to $NODE1 was refused up front (no eligible node) - gate held"
fi
# The gate must NOT flip the node's own status - it stays ready (orthogonal to health).
[ "$(node_status "$NODE1")" = "ready" ] || fail "down-path gate wrongly changed $NODE1 status (should stay ready)"
pass "down-path gate excluded $NODE1 while its status stayed ready; re-enabling gateway"
otx node gateway disable "$GW" >/dev/null 2>&1 || true   # idempotent no-op if already off
otx vm delete "${NET}-gate" --wait --force >/dev/null 2>&1 || true

trap - EXIT
cleanup
echo
echo "${GREEN}=== geo-nat smoke PASSED ===${NC}"
