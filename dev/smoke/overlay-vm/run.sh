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
# Unlike seed-mvp (which provisions a pool + template ONLY on node-1), this smoke
# provisions a second pool on node-2 and materialises the shared template into it
# so a VM can actually be placed on node-2. Both pool + the node-2 materialisation
# are torn down at the end.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make local-dev-start
# Both nodes ready and otwg0 up. The N3c agent reconciler MUST be in the running
# agents or the FDB is never programmed.
#
# Usage: make smoke-overlay-vm   (or: bash dev/smoke/overlay-vm/run.sh)

set -euo pipefail

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
VM1="${VM1:-otherix-dev-1}"           # Lima instance backing node-1
VM2="${VM2:-otherix-dev-2}"           # Lima instance backing node-2
NODE1="${NODE1:-node-1}"
NODE2="${NODE2:-node-2}"
TEMPLATE="${TEMPLATE:-ubuntu-noble-arm64-mvp}"   # seed-mvp arm64 template
POOL2="${POOL2:-pool-mvp-2}"          # smoke-provisioned pool on node-2
POOL2_PATH="${POOL2_PATH:-/opt/otherix/pools/default}"
SUBNET="${SUBNET:-10.71.0.0/24}"
IP1="${IP1:-10.71.0.10}"
IP2="${IP2:-10.71.0.11}"
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

# gen_userdata SELF-IP PEER-IP DO-PROBE(yes/no) -> #cloud-config on stdout.
# Statically configures the single overlay NIC. The guest's only NIC enumerates
# as enp0s1 (deterministic from the agent's QEMU machine type + single virtio-net
# device on PCI slot 1). We target it by a glob match (en*) and mark it optional
# so systemd-networkd-wait-online does not block boot for ~3min waiting on a DHCP
# lease that never comes (there is no DHCP server on the overlay). A runcmd
# `ip addr replace` is a belt-and-suspenders fallback in case the netplan rewrite
# is masked by the template's baked 50-cloud-init.yaml.
#
# The reachability probe is a TCP connect to the peer's sshd (:22) via bash
# /dev/tcp - NOT ping. The Ubuntu *minimal* cloudimg ships no iputils-ping (and
# no nc/arping); curl/wget/bash-builtins are all that's present. A successful
# /dev/tcp open proves the full L3 path (ARP over the overlay flood entry + the
# unicast frame across the encrypted VXLAN) end to end. sshd is up by default in
# the image, so the passive peer needs no listener setup. The success sentinel is
# echoed to the guest serial console, which the agent persists to
# /opt/otherix/vms/<id>/serial.log (no SSH into the guest required).
gen_userdata() {
  local self="$1" peer="$2" probe="$3"
  cat <<EOF
#cloud-config
users:
  - name: otherix
    plain_text_passwd: otherix
    lock_passwd: false
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [sudo]
    shell: /bin/bash
chpasswd:
  expire: false
ssh_pwauth: true
write_files:
  - path: /etc/netplan/50-cloud-init.yaml
    permissions: '0600'
    content: |
      network:
        version: 2
        ethernets:
          overlay-nic:
            match:
              name: "en*"
            dhcp4: false
            dhcp6: false
            optional: true
            addresses: [${self}/24]
runcmd:
  - netplan apply || true
  - sleep 2
  - |
    # Serial console device differs by arch: ttyAMA0 (PL011) on arm64, ttyS0 on
    # amd64. The agent captures whichever the QEMU -serial chardev backs into
    # /opt/otherix/vms/<id>/serial.log, so pick the present one.
    SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0
    IF=\$(ls /sys/class/net | grep -E '^en' | head -1)
    ip addr replace ${self}/24 dev "\$IF" 2>/dev/null || true
    ip link set "\$IF" up 2>/dev/null || true
    sleep 2
    { echo "SMOKE_NET if=\$IF serial=\$SC"; ip -br addr show; } > "\$SC" 2>&1 || true
EOF
  if [[ "$probe" == "yes" ]]; then
    cat <<EOF
  - |
    SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0
    for i in \$(seq 1 150); do
      if timeout 3 bash -c "echo > /dev/tcp/${peer}/22" 2>/dev/null; then
        echo "OVERLAY_PING_OK ${self}->${peer}" > "\$SC"
        break
      fi
      echo "SMOKE_PROBE ${self}->${peer}:22 attempt \$i failed" > "\$SC"
      sleep 2
    done
EOF
  fi
}

NET=""
UD1=""; UD2=""
cleanup() {
  echo "--- cleanup ---"
  otx vm delete "${NET}-a" --wait --force >/dev/null 2>&1 || true
  otx vm delete "${NET}-b" --wait --force >/dev/null 2>&1 || true
  [[ -n "$NET" ]] && otx network delete "$NET" --force >/dev/null 2>&1 || true
  [[ -n "$UD1" ]] && rm -f "$UD1"
  [[ -n "$UD2" ]] && rm -f "$UD2"
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

# --- step 0: stage a pool + template on node-2 -------------------------
# seed-mvp only provisions a pool + materialises the template on node-1; a VM
# pinned to node-2 needs both there too. Idempotent: pool create upserts, and
# template create re-materialises into a new pool when the template row exists.
echo "=== step 0: stage pool + template on $NODE2 (seed-mvp leaves it bare) ==="
# pool create is not idempotent (errors on a duplicate name); tolerate a pool
# left behind by a prior run so the smoke is re-runnable.
if otx pool get "$POOL2" --output json >/dev/null 2>&1; then
  info "pool $POOL2 already present on $NODE2 (reusing)"
else
  otx pool create "$POOL2" --node "$NODE2" --path "$POOL2_PATH" --wait \
    || fail "pool create $POOL2 on $NODE2 failed"
fi
# `pool get <name>` returns the aggregated concept view; per-instance state lives
# under .instances[]. Assert the node-2 instance reconciled ready.
[[ "$(otx pool get "$POOL2" --output json \
    | jq -r --arg n "$NODE2" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]] \
  || fail "pool $POOL2 not ready on $NODE2"
pass "pool $POOL2 ready on $NODE2"
arch="$(otx node get "$NODE2" --output json | jq -r '.architecture')"
img_url="$(otx template get "$TEMPLATE" --output json | jq -r '.image_url')"
[[ -n "$img_url" && "$img_url" != "null" ]] || fail "could not read image_url for template $TEMPLATE"
info "materialising $TEMPLATE into $POOL2 on $NODE2 (<= ${VM_WAIT}s)"
otx template create "$TEMPLATE" \
  --arch "$arch" --os-family linux --os-variant ubuntu-24.04 \
  --image-url "$img_url" --pool "$POOL2" \
  --wait --wait-timeout "${VM_WAIT}s" \
  || fail "template $TEMPLATE materialise into $POOL2 failed"
pass "$TEMPLATE materialised into $POOL2 on $NODE2"

# --- step 1: overlay + two VMs (one per node) --------------------------
echo "=== step 1: create overlay + a VM on each node ==="
NET="ovvm-$$"
created="$(otx network create "$NET" --type overlay --subnet "$SUBNET" --output json)" \
  || fail "overlay network create failed"
VNI="$(echo "$created" | jq -r '.vni')"
[[ "$VNI" =~ ^[0-9]+$ ]] || fail "no vni in create response: $created"
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

UD1="$(mktemp)"; UD2="$(mktemp)"
gen_userdata "$IP1" "$IP2" yes > "$UD1"   # VM-A pings VM-B and emits the sentinel
gen_userdata "$IP2" "$IP1" no  > "$UD2"   # VM-B is passive

info "creating ${NET}-a on $NODE1 ($IP1) and ${NET}-b on $NODE2 ($IP2) (<= ${VM_WAIT}s each)"
otx vm create --name "${NET}-a" --node "$NODE1" --network "$NET" --template "$TEMPLATE" \
  --pool "pool-mvp" --vcpus 2 --memory-mb 2048 --cloud-init "$UD1" \
  --wait --wait-timeout "${VM_WAIT}s" \
  || fail "vm create A on $NODE1 did not reach success"
otx vm create --name "${NET}-b" --node "$NODE2" --network "$NET" --template "$TEMPLATE" \
  --pool "$POOL2" --vcpus 2 --memory-mb 2048 --cloud-init "$UD2" \
  --wait --wait-timeout "${VM_WAIT}s" \
  || fail "vm create B on $NODE2 did not reach success"
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
echo "=== step 4: teardown VMs + overlay + node-2 staging ==="
otx vm delete "${NET}-a" --wait --force >/dev/null 2>&1 || true
otx vm delete "${NET}-b" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
NET=""   # disarm the trap's network delete (already done)
otx pool delete "$POOL2" --force >/dev/null 2>&1 || true
pass "VMs + overlay deleted; node-2 staging cleaned up"

trap - EXIT
rm -f "$UD1" "$UD2"
echo
echo "${GREEN}=== overlay-vm smoke PASSED ===${NC}"
