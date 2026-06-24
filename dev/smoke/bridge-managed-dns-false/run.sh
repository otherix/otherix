#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Managed-bridge dns=false smoke - proves that a managed bridge created with
# DHCP on but DNS off (dhcp: true, dns: false) addresses its VMs WITHOUT
# advertising the anycast resolver, that the setting round-trips through the
# declarative manifest path, and that the real agent honours it.
#
# A managed bridge runs the per-node DHCP responder and (when dns is on) the
# anycast DNS forwarder at 169.254.1.1. With dns: false the responder still hands
# out an address and - because egress is nat - a default route, but does NOT
# advertise 169.254.1.1 as the resolver (no DHCP option 6). Addressing is
# independent of egress, and the resolver is independent of addressing.
#
# What it proves, end to end, as a real operator (CLI + manifest only):
#   - a Network manifest with `dhcp: true, dns: false` is accepted and the
#     created network reports dns=false (the field reaches the server);
#   - `network get -o yaml` round-trips `dns: false` and `-o json` reports
#     `.dns == false` (the view deserialises it and the projection emits it);
#   - a VM on the bridge DHCPs an IPv4 lease in the subnet (DHCP still works);
#   - the VM does NOT receive 169.254.1.1 as a resolver (option 6 withheld);
#   - the VM still has a default route via 169.254.1.1 (egress=nat is
#     independent of dns), i.e. dns: false turns off ONLY the resolver.
#
# The VM DHCPs its NIC; a boot sentinel echoes the leased address, the route
# table, and a computed verdict on whether 169.254.1.1 is a configured DNS
# server, to the guest serial console (captured by the agent to serial.log).
# Reading the resolver as a guest-computed verdict (not "does a lookup fail")
# avoids attributing a stray fallback resolver to the bridge.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
#
# Usage: make smoke-bridge-managed-dns-false
#   (or: bash dev/smoke/bridge-managed-dns-false/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"

NET="bridge-dns-false"           # managed bridge, dhcp on + dns OFF + nat egress
BRIDGE_NAME="smkbr1"             # Linux ifname, <= 15 chars (otb*/otvx*/otwg* are reserved)
SUBNET="10.89.0.0/24"            # unlikely to clash with the dev stack
VM="bridge-dns-false-vm"

RESOLVER="169.254.1.1"           # the resolver that must NOT be advertised here

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"
GUEST_WAIT="${GUEST_WAIT:-720}"
NET_WAIT="${NET_WAIT:-180}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

net_ready_both() {
  local net="$1" n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx network get "$net" --output json 2>/dev/null \
        | jq -r --arg n "$n" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
        == "ready" ]] || return 1
  done
  return 0
}

wait_net_ready_both() {
  local net="$1" deadline=$(( SECONDS + NET_WAIT ))
  info "waiting for $net to reconcile ready on both nodes (<= ${NET_WAIT}s)"
  while (( SECONDS < deadline )); do
    net_ready_both "$net" && { pass "$net reconciled ready on both nodes"; return 0; }
    sleep 3
  done
  otx network get "$net" --output json 2>/dev/null | jq -c '.status.nodes' >&2 || true
  fail "$net did not reconcile ready on both nodes within ${NET_WAIT}s"
}

serial_for() { printf '%s' "$(smoke_state 1)/vms/$1/serial.log"; }

serial_count() {
  local serial="$1" pat="$2" n
  n="$(run_on "$SMOKE_HANDLE_1" sudo grep -c "$pat" "$serial" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

wait_serial() {
  local serial="$1" pat="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  info "watching $serial on $NODE1 for '$pat' (<= ${to}s)"
  while (( SECONDS < deadline )); do
    (( $(serial_count "$serial" "$pat") > 0 )) && return 0
    sleep 5
  done
  return 1
}

dump_serial() { run_on "$SMOKE_HANDLE_1" sudo tail -80 "$1" 2>/dev/null || true; }

TMPDIR_BR=""
delete_stale_resources() {
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
}
cleanup() {
  echo "--- cleanup ---"
  delete_stale_resources
  [ -n "$TMPDIR_BR" ] && rm -rf "$TMPDIR_BR" 2>/dev/null || true
}
trap cleanup EXIT

# --- cloud-init payloads (network-config + user-data) ------------------
TMPDIR_BR="$(mktemp -d)"
NET_FILE="${TMPDIR_BR}/net.yaml"
RT_FILE="${TMPDIR_BR}/net-roundtrip.yaml"
NC_FILE="${TMPDIR_BR}/network-config.yaml"
UD_FILE="${TMPDIR_BR}/user-data.yaml"

# Network manifest: the new dns: false field on a managed bridge with dhcp + nat.
cat >"$NET_FILE" <<EOF
apiVersion: otherix/v1
kind: Network
metadata:
  name: ${NET}
spec:
  type: bridge
  managed: true
  bridgeName: ${BRIDGE_NAME}
  egress: nat
  subnet: ${SUBNET}
  dhcp: true
  dns: false
EOF

# network-config (netplan v2): DHCP the single bridge NIC.
cat >"$NC_FILE" <<'EOF'
network:
  version: 2
  ethernets:
    overlay-nic:
      match:
        name: "en*"
      dhcp4: true
      dhcp6: false
      optional: true
EOF

# user-data: a verification sentinel only. A systemd oneshot ordered
# After=network-online.target echoes, to the arch-correct serial device:
#   - OTHERIX_DHCP_ADDR + `ip -4 addr show`   (the leased IPv4 - DHCP still works)
#   - OTHERIX_ROUTE + `ip route`               (default route - egress=nat, dns-independent)
#   - OTHERIX_RESOLVERS + the per-link DNS config (diagnostic)
#   - a computed verdict: OTHERIX_RESOLVER_ABSENT when 169.254.1.1 is NOT a
#     configured DNS server (the dns: false expectation), else
#     OTHERIX_RESOLVER_PRESENT. The check reads the systemd-resolved per-link
#     servers and the networkd-written resolv.conf, both populated from DHCP
#     option 6; the option-121 default route gateway is NOT a DNS entry, so a
#     169.254.1.1 here is attributable to option 6 alone.
cat >"$UD_FILE" <<'EOF'
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-dns-false.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix managed-bridge dns=false smoke sentinel
      After=network-online.target
      Wants=network-online.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; { echo OTHERIX_DHCP_ADDR; ip -4 addr show; echo OTHERIX_ROUTE; ip route; echo OTHERIX_RESOLVERS; resolvectl dns 2>/dev/null; cat /run/systemd/resolve/resolv.conf 2>/dev/null; } > "$SC" 2>&1; if { resolvectl dns 2>/dev/null; cat /run/systemd/resolve/resolv.conf 2>/dev/null; } | grep -q 169.254.1.1; then echo OTHERIX_RESOLVER_PRESENT > "$SC"; else echo OTHERIX_RESOLVER_ABSENT > "$SC"; fi'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-dns-false.service ]
EOF

# --- preconditions -----------------------------------------------------
echo "=== bridge-managed-dns-false smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
pool_ready_node1() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready_node1 && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); both nodes ready; default pool ready on $NODE1"

delete_stale_resources

# --- step 1: create the bridge from a manifest, assert dns=false round-trips --
echo "=== step 1: managed bridge $NET from a manifest (dhcp: true, dns: false, egress: nat) ==="
otx create -f "$NET_FILE" || fail "create -f (Network/$NET) failed"

# The server must report dns=false (the manifest field reached it).
DNS_JSON="$(otx network get "$NET" --output json | jq -r '.dns')"
[[ "$DNS_JSON" == "false" ]] || fail "$NET should report dns=false, got '$DNS_JSON'"
# dhcp must still be on, egress nat (addressing + egress unaffected by dns: false).
DHCP_JSON="$(otx network get "$NET" --output json | jq -r '.dhcp')"
EGRESS_JSON="$(otx network get "$NET" --output json | jq -r '.egress')"
[[ "$DHCP_JSON" == "true" ]] || fail "$NET should report dhcp=true, got '$DHCP_JSON'"
[[ "$EGRESS_JSON" == "nat" ]] || fail "$NET should report egress=nat, got '$EGRESS_JSON'"
pass "$NET created from manifest; server reports dhcp=true, dns=false, egress=nat"

# Round-trip: get -o yaml must project dns: false (view deserialised it, the
# projection emits it because it diverges from the bridge default dns==dhcp).
otx network get "$NET" --output yaml > "$RT_FILE"
grep -Eq '^[[:space:]]*dns:[[:space:]]*false[[:space:]]*$' "$RT_FILE" \
  || { echo "--- projected manifest ---"; cat "$RT_FILE"; fail "get -o yaml did not round-trip 'dns: false'"; }
# The projected document must still parse + plan as apply-ready (dry-run, no server hit).
otx create -f "$RT_FILE" --dry-run >/dev/null 2>&1 \
  || fail "projected manifest is not apply-ready (create -f --dry-run failed)"
pass "get -o yaml round-trips 'dns: false' and re-projects to an apply-ready manifest"

wait_net_ready_both "$NET"

# --- step 2: VM on the bridge; lease + route present, resolver absent ----
echo "=== step 2: VM $VM on $NET (DHCP addressing, no resolver) ==="
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --vcpus 2 --memory-mb 2048 \
  --user-data "$UD_FILE" --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM id (got '$VMID')"
SERIAL="$(serial_for "$VMID")"
info "$VM id=$VMID  serial=$SERIAL"
pass "$VM created and running"

if ! wait_serial "$SERIAL" "OTHERIX_DHCP_ADDR" "$GUEST_WAIT"; then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "no OTHERIX_DHCP_ADDR sentinel within ${GUEST_WAIT}s - $VM did not boot"
fi

# DHCP still works: an address in the bridge subnet on the wire.
if (( $(serial_count "$SERIAL" "inet 10\.89\.0\.") == 0 )); then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM got no DHCP lease in $SUBNET - per-node DHCP responder did not hand out an address"
fi
pass "$VM got a DHCP lease in $SUBNET (DHCP works with dns: false)"

# The resolver verdict: 169.254.1.1 must NOT be a configured DNS server.
if ! wait_serial "$SERIAL" "OTHERIX_RESOLVER_ABSENT\|OTHERIX_RESOLVER_PRESENT" 120; then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM did not report a resolver verdict"
fi
if (( $(serial_count "$SERIAL" "OTHERIX_RESOLVER_PRESENT") > 0 )); then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM was advertised $RESOLVER as a resolver - dns: false must withhold DHCP option 6"
fi
if (( $(serial_count "$SERIAL" "OTHERIX_RESOLVER_ABSENT") == 0 )); then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM did not confirm the resolver is absent"
fi
pass "$VM was NOT advertised $RESOLVER as a resolver (dns: false withheld DHCP option 6)"

# Egress is independent of dns: a default route via 169.254.1.1 is still present.
if (( $(serial_count "$SERIAL" "^default.*${RESOLVER}") == 0 )); then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM has no default route via $RESOLVER - egress=nat must still advertise the default route"
fi
pass "$VM still has a default route via $RESOLVER (egress=nat is independent of dns)"

# --- step 3: teardown --------------------------------------------------
echo "=== step 3: teardown VM + bridge ==="
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "VM + bridge deleted"

trap - EXIT
rm -rf "$TMPDIR_BR" 2>/dev/null || true
echo
echo "${GREEN}=== bridge-managed-dns-false smoke PASSED ===${NC}"
