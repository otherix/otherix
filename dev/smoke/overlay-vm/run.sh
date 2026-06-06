#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Networking N3c overlay VM-to-VM smoke - proves two real VMs on different
# nodes, attached to the same type=overlay network, reach each other over the
# encrypted VXLAN overlay using ONLY the CP-distributed FDB (no manual
# neigh/fdb, unlike the N3b smoke-overlay which programmed the FDB by hand).
# Guest IPs are cloud-init-configured (N3c has no CP-side IP assignment).
#
# What it proves, end to end:
#   - operator CLI (primary): `otherix network create --type overlay` allocates
#     a VNI + otb<vni> bridge; `otherix vm create --network <overlay>` attaches a
#     NIC to that overlay on each node;
#   - the agent programs the otvx<vni> FDB from the CP's declared_fdb
#     (head-end replication): the peer VM's MAC -> peer node's otwg0 VTEP IP plus
#     the all-zeros BUM/flood entry. NO manual `bridge fdb`/`ip neigh` is run;
#   - cross-node datapath: VM-A (node-1) reaches VM-B (node-2) over the VXLAN that
#     rides the encrypted WireGuard underlay, with the unicast FDB entry the only
#     thing making the frame leave the node (VXLAN learning is off). The guest
#     probe is a TCP connect to the peer's sshd (the minimal cloudimg has no ping).
#
# No pool staging: the CP auto-provisions the cluster default pool ("default") on
# every node as it reaches ready (PR #15), so a VM can be placed on either node
# without a manual pool. Both VMs are created WITHOUT --pool (resolving to that
# default) from an image URL - the agent materializes the image into the pool's
# basename-keyed cache on first use (IfNotPresent).
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make local-dev-start
# Both nodes ready and otwg0 up. The N3c agent reconciler MUST be in the running
# agents or the FDB is never programmed.
#
# Usage: make smoke-overlay-vm   (or: bash dev/smoke/overlay-vm/run.sh)

set -euo pipefail

# --- configuration -----------------------------------------------------
# Resource names, node pinning, subnet, and the two guest IPs are fixed in the
# committed manifests (manifests/overlay.yaml + manifests/vms.yaml.tmpl); the
# constants below mirror them for the assertions/log lines and MUST stay in
# sync with those files. Only IMAGE_URL / ARCH are rendered into the template.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OTX="${OTX:-./bin/otherix}"
VM1="${VM1:-otherix-dev-1}"           # Lima instance backing node-1
VM2="${VM2:-otherix-dev-2}"           # Lima instance backing node-2
NODE1="node-1"
NODE2="node-2"
NET="ovvm-smoke"                      # fixed; the smoke delete-firsts any stale leftover
IMAGE_URL="${IMAGE_URL:-https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-arm64.img}"  # Resolute (26.04) minimal arm64 cloudimg
ARCH="${ARCH:-arm64}"
IP1="10.71.0.10"
IP2="10.71.0.11"
PING_WAIT="${PING_WAIT:-720}"         # seconds to wait for the cross-node ping sentinel.
                                      # On dev Macs (M1/M2 Lima, no nested KVM) the guest
                                      # boots under TCG software emulation and can take ~7min
                                      # to reach cloud-init's runcmd stage, so this is generous.
VM_WAIT="${VM_WAIT:-420}"             # seconds to wait per vm create task
FDB_WAIT="${FDB_WAIT:-180}"           # seconds to wait for CP-distributed FDB on both nodes

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx()   { "$OTX" "$@"; }
invm1() { limactl shell "$VM1" -- "$@"; }
invm2() { limactl shell "$VM2" -- "$@"; }

# overlay_ip NODE -> prints the node's otwg0 overlay host IP (no /prefix)
overlay_ip() { otx node get "$1" --output json 2>/dev/null | jq -r '.wireguard.overlay_ip' | cut -d/ -f1; }

# net_ready_both NET -> 0 when the network reconciled to "ready" on both nodes.
# The scheduler's network-aware filter (ADR 0034 NL18) excludes any node where a
# requested network has not reconciled to ready, so a VM-create raced ahead of
# overlay materialisation fails with no_eligible_nodes. We gate on this first.
net_ready_both() {
  local net="$1" n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx network get "$net" --output json 2>/dev/null \
        | jq -r --arg n "$n" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
        == "ready" ]] || return 1
  done
  return 0
}

# The guest cloud-config now lives in manifests/vms.yaml.tmpl (one document per
# VM, inline `cloudInit:`), not a heredoc here. Notes preserved from the old
# gen_userdata for context:
#   - The single overlay NIC enumerates as en* (deterministic QEMU virtio-net on
#     PCI slot 1); netplan targets it by glob and marks it optional so
#     systemd-networkd-wait-online does not block boot waiting on a DHCP lease
#     that never comes (no DHCP on the overlay). A runcmd `ip addr replace` is a
#     belt-and-suspenders fallback.
#   - VM-A's reachability probe is a bash /dev/tcp connect to the peer's sshd
#     (:22), NOT ping (the Ubuntu *minimal* cloudimg ships no ping/nc). A
#     successful open proves the full L3 path (ARP over the overlay flood entry +
#     the unicast frame across the encrypted VXLAN). The OVERLAY_PING_OK sentinel
#     is echoed to the guest serial console, which the agent persists to
#     /opt/otherix/vms/<id>/serial.log (no SSH into the guest required).
cleanup() {
  echo "--- cleanup ---"
  otx vm delete "${NET}-a" --wait --force >/dev/null 2>&1 || true
  otx vm delete "${NET}-b" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== overlay-vm smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v limactl >/dev/null || fail "limactl is required"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
OVL1="$(overlay_ip "$NODE1")"; OVL2="$(overlay_ip "$NODE2")"
[[ "$OVL1" == 10.42.* && "$OVL2" == 10.42.* ]] || fail "otwg0 overlay IPs not in 10.42/16 ($OVL1, $OVL2)"
pass "CP up (${CP_VERSION}); $NODE1=$OVL1, $NODE2=$OVL2 ready"

# --- step 0: cluster default pool ready on both nodes ------------------
# No pool is created here. The CP auto-provisions the cluster default pool
# ("default") on every node as it reaches ready (PR #15), and the VMs below are
# created WITHOUT --pool, resolving to that default. Assert it reconciled on
# both nodes so a VM create fails fast with a clear cause if auto-provision is
# broken. `pool get <name>` returns the aggregated view; per-instance state lives
# under .instances[].
echo "=== step 0: cluster default pool ready on $NODE1 and $NODE2 ==="
# The pool auto-provisions a reconcile tick after a node reaches ready, so poll
# rather than assert once (mirrors the old `pool create --wait`).
pool_ready_both() {
  local n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx pool get default --output json 2>/dev/null \
        | jq -r --arg n "$n" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]] || return 1
  done
  return 0
}
deadline=$(( SECONDS + 60 )); pool_ok=0
while (( SECONDS < deadline )); do
  if pool_ready_both; then pool_ok=1; break; fi
  sleep 3
done
(( pool_ok == 1 )) || fail "default pool not ready on both nodes within 60s (CP auto-provision)"
pass "default pool ready on $NODE1 and $NODE2"

# --- step 1: overlay + two VMs (one per node) --------------------------
echo "=== step 1: create overlay + a VM on each node ==="
# best-effort delete-first so a stale leftover from a prior run does not 409
cleanup >/dev/null 2>&1 || true
otx create -f "${SCRIPT_DIR}/manifests/overlay.yaml" || fail "create -f overlay.yaml failed"
VNI="$(otx network get "$NET" --output json | jq -r '.vni')"
[[ "$VNI" =~ ^[0-9]+$ ]] || fail "no vni on $NET after create"
VTEP="otvx$VNI"
pass "overlay $NET created: vni=$VNI vtep=$VTEP"

# Gate on overlay materialisation: the network-aware scheduler refuses to place a
# VM on a node where the overlay has not reconciled to ready (no_eligible_nodes).
info "waiting for $NET to reconcile ready on both nodes (<= ${FDB_WAIT}s)"
deadline=$(( SECONDS + FDB_WAIT )); net_ok=0
while (( SECONDS < deadline )); do
  if net_ready_both "$NET"; then net_ok=1; break; fi
  sleep 3
done
(( net_ok == 1 )) || { otx network get "$NET" --output json | jq -c '.status.nodes'; fail "$NET did not reconcile ready on both nodes within ${FDB_WAIT}s"; }
pass "$NET reconciled ready on both nodes"

# Both VMs resolve to the cluster default pool (no spec.pool) and are pinned per
# node in the manifest. On a cold pool the agent materializes the image into the
# pool's basename-keyed cache on first use (IfNotPresent), so budget for the
# download. create -f applies both VM documents and --wait polls them; the wait
# budget is shared across the two, so double the per-VM cold budget.
COLD_VM_WAIT=$(( VM_WAIT * 2 ))
WAIT_BOTH=$(( COLD_VM_WAIT * 2 ))
info "creating ${NET}-a on $NODE1 ($IP1) and ${NET}-b on $NODE2 ($IP2) (default pool, <= ${WAIT_BOTH}s for both incl. cold image fetch)"
sed -e "s|@@IMAGE_URL@@|${IMAGE_URL}|g" -e "s|@@ARCH@@|${ARCH}|g" \
  "${SCRIPT_DIR}/manifests/vms.yaml.tmpl" \
  | otx create -f - --wait --wait-timeout "${WAIT_BOTH}s" \
  || fail "create -f VMs did not reach success"
pass "overlay $NET + both VMs created (one per node)"

# resolve the two VM ids (for the on-node serial.log path)
VMID_A="$(otx vm get "${NET}-a" --output json | jq -r '.id')"
VMID_B="$(otx vm get "${NET}-b" --output json | jq -r '.id')"
info "VM-A id=$VMID_A  VM-B id=$VMID_B"

# --- step 2: CP-programmed FDB on both nodes (no manual fdb) ------------
echo "=== step 2: CP-distributed FDB present on both nodes (no manual fdb) ==="
# Once each node hosts a local overlay VM and a peer VM exists on the other node,
# the CP's declared_fdb (head-end replication) gives each node, for the peer's
# VTEP: an all-zeros BUM/flood entry AND the peer VM's unicast MAC entry. The
# agent installs both; nothing in this smoke runs `bridge fdb add`. We assert
# both the flood entry and at least one non-zero unicast entry to the peer VTEP.
fdb_ok() {  # $1=lima-vm  $2=peer-overlay-ip
  local vm="$1" peer_ovl="$2" dump
  dump="$(limactl shell "$vm" -- bridge fdb show dev "$VTEP" 2>/dev/null || true)"
  echo "$dump" | grep -qi "00:00:00:00:00:00 .*dst ${peer_ovl}" || return 1   # BUM/flood
  echo "$dump" | grep -Eiv "00:00:00:00:00:00" | grep -qi "dst ${peer_ovl}" || return 1  # peer-VM unicast
  return 0
}
deadline=$(( SECONDS + FDB_WAIT )); ok=0
while (( SECONDS < deadline )); do
  if fdb_ok "$VM1" "$OVL2" && fdb_ok "$VM2" "$OVL1"; then ok=1; break; fi
  sleep 5
done
if (( ok != 1 )); then
  echo "--- $NODE1 ($VTEP) fdb ---"; invm1 bridge fdb show dev "$VTEP" || true
  echo "--- $NODE2 ($VTEP) fdb ---"; invm2 bridge fdb show dev "$VTEP" || true
  fail "CP-distributed FDB not present on both nodes within ${FDB_WAIT}s"
fi
pass "both nodes: CP-distributed FDB on $VTEP (flood + peer-VM unicast, no manual fdb)"

# --- step 3: cross-node VM-to-VM reachability (the money shot) ----------
echo "=== step 3: cross-node VM-to-VM reachability over the overlay ==="
# VM-A's cloud-init TCP-probes VM-B's sshd (:22) over the overlay and writes
# OVERLAY_PING_OK to its serial console on success; the agent persists that
# console to /opt/otherix/vms/<id>/serial.log. A successful connect proves ARP
# resolved across the overlay (via the CP flood entry) and the unicast frame
# traversed the encrypted VXLAN to the peer guest - all on the CP-distributed
# FDB, no manual neigh/fdb. (The minimal cloudimg has no ping, hence /dev/tcp.)
SERIAL_A="/opt/otherix/vms/${VMID_A}/serial.log"
info "watching $SERIAL_A on $NODE1 for OVERLAY_PING_OK (<= ${PING_WAIT}s)"
deadline=$(( SECONDS + PING_WAIT )); seen=0
while (( SECONDS < deadline )); do
  if invm1 sudo grep -q "OVERLAY_PING_OK" "$SERIAL_A" 2>/dev/null; then seen=1; break; fi
  sleep 5
done
if (( seen != 1 )); then
  echo "--- $NODE1 $VTEP fdb ---"; invm1 bridge fdb show dev "$VTEP" || true
  echo "--- VM-A serial tail ---"; invm1 sudo tail -40 "$SERIAL_A" 2>/dev/null || true
  fail "no OVERLAY_PING_OK sentinel - cross-node overlay reachability did not succeed"
fi
pass "cross-node VM-to-VM reachability over the overlay succeeded (CP-distributed FDB, no manual neigh)"

# --- step 4: teardown --------------------------------------------------
echo "=== step 4: teardown VMs + overlay ==="
# Imperative + --wait so the VMs are gone before the overlay delete (avoids a
# NIC-release race on the slow cross-node path); best-effort so a teardown hiccup
# never masks the reachability result proven above. The dedicated manifests smoke
# covers the `delete -f` reverse-order path.
otx vm delete "${NET}-a" --wait --force >/dev/null 2>&1 || true
otx vm delete "${NET}-b" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "VMs + overlay deleted"

trap - EXIT
echo
echo "${GREEN}=== overlay-vm smoke PASSED ===${NC}"
