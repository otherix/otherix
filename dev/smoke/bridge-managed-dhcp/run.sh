#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Managed-bridge DHCP/DNS smoke - proves that a managed bridge network with
# DHCP, DNS and NAT egress hands a VM a leased address, an in-network resolver,
# and a default route, all through the operator-facing CLI.
#
# A managed bridge is an isolated per-host L2 segment that reuses the same
# anycast 169.254.1.1 datapath as overlays: the per-node DHCP responder hands
# out an address in the bridge subnet, advertises 169.254.1.1 as the resolver
# (DHCP option 6), and - because egress is nat - advertises a default route via
# 169.254.1.1 (DHCP option 3) so the VM reaches the internet through per-node
# SNAT.
#
# What it proves, end to end, as a real operator (CLI only):
#   - `otherix network create <net> --type bridge --managed --bridge-name ...
#     --egress nat --subnet ... --dhcp` reconciles ready, and a VM on it
#     DHCPs an IPv4 lease in the bridge subnet from the per-node responder;
#   - the VM resolves a name through the in-network resolver 169.254.1.1
#     (DHCP option 6 handed out);
#   - the VM has a default route via 169.254.1.1 (DHCP option 3 - egress=nat
#     advertised it), i.e. NAT egress is wired.
#
# The VM DHCPs its NIC (netplan dhcp4: true), then a boot sentinel echoes the
# leased address, the resolver-lookup result, and the route table to the guest
# serial console. The agent persists that console to <state_root>/vms/<id>/
# serial.log, so every in-guest assertion reads from serial.log (no SSH into the
# guest). getent hosts resolves via the guest's /etc/resolv.conf, which the DHCP
# client populates from option 6 - so a successful lookup is attributable to the
# resolver advertisement alone.
#
# Note on reading the leased IP: the CP does not surface a VM's DHCP lease on
# `vm get` (DHCP is node-local observed state), so the leased address is read
# from the serial-logged `ip -4 addr show`, the same channel the sibling
# overlay-isolated-dns and vm-network-config smokes use for their address checks.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# Both nodes ready with the cluster default pool reconciled.
#
# Usage: make smoke-bridge-managed-dhcp
#   (or: bash dev/smoke/bridge-managed-dhcp/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"

NET="bridge-dhcp"                  # managed bridge, dhcp + dns + nat egress
BRIDGE_NAME="otbsmoke"            # Linux ifname, <= 15 chars
SUBNET="10.88.0.0/24"            # unlikely to clash with the dev stack
VM="bridge-dhcp-vm"

RESOLVER="169.254.1.1"            # in-network resolver advertised via DHCP option 6
RESOLVE_NAME="archive.ubuntu.com" # name the in-network forwarder answers from the node upstream

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"  # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"    # seconds to wait for the boot sentinel (TCG is slow)
NET_WAIT="${NET_WAIT:-180}"        # seconds to wait for the bridge to reconcile ready on both nodes

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# net_ready_both NET -> 0 when the network reconciled to "ready" on both nodes.
# The network-aware scheduler refuses to place a VM on a node where a requested
# network has not reconciled ready (no_eligible_nodes), so gate on this first.
net_ready_both() {
  local net="$1" n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx network get "$net" --output json 2>/dev/null \
        | jq -r --arg n "$n" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
        == "ready" ]] || return 1
  done
  return 0
}

# wait_net_ready_both NET -> poll net_ready_both until the deadline; fail on timeout.
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

# serial_for VMID -> the on-node serial.log path for a VM bound to node-1.
# The VM is pinned to node-1, so the path always resolves under SMOKE_STATE_1.
serial_for() { printf '%s' "$(smoke_state 1)/vms/$1/serial.log"; }

# serial_count SERIAL PATTERN -> count of lines matching PATTERN in SERIAL (0 when
# the file is absent). The agent appends the guest console there; callers wait for
# a count rather than a single grep so a stale match across reboots does not race.
serial_count() {
  local serial="$1" pat="$2" n
  n="$(run_on "$SMOKE_HANDLE_1" sudo grep -c "$pat" "$serial" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_serial SERIAL PATTERN TIMEOUT -> block until PATTERN appears in SERIAL.
wait_serial() {
  local serial="$1" pat="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  info "watching $serial on $NODE1 for '$pat' (<= ${to}s)"
  while (( SECONDS < deadline )); do
    (( $(serial_count "$serial" "$pat") > 0 )) && return 0
    sleep 5
  done
  return 1
}

# dump_serial SERIAL -> best-effort tail of the VM's serial.log, for failure diag.
dump_serial() { run_on "$SMOKE_HANDLE_1" sudo tail -60 "$1" 2>/dev/null || true; }

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
# Written to temp files; passed to the CLI via --network-config / --user-data.
TMPDIR_BR="$(mktemp -d)"
NC_FILE="${TMPDIR_BR}/network-config.yaml"
UD_FILE="${TMPDIR_BR}/user-data.yaml"

# network-config (netplan v2): DHCP the single bridge NIC. The lease (address,
# the option-6 nameserver, and the option-3 default route) is the ONLY source of
# the guest's addressing, resolver and route, so a name that resolves and a
# default route present are attributable to the bridge's DHCP advertisements, and
# a leased address proves the per-node DHCP responder. systemd-networkd honours
# the DHCP options by default. optional:true keeps systemd-networkd-wait-online
# from blocking boot if a renewal lags.
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

# user-data: a verification sentinel only. It must NOT configure networking (no
# write_files netplan, no `netplan apply`), so addressing/resolution/routing come
# solely from DHCP. A systemd oneshot ordered After=network-online.target echoes,
# to the arch-correct serial device:
#   - OTHERIX_DHCP_ADDR + `ip -4 addr show`   (the leased IPv4)
#   - OTHERIX_ROUTE + `ip route`               (presence of a default route)
#   - the resolver probe: getent hosts <name> resolves via /etc/resolv.conf, which
#     the DHCP client populates from option 6. On success it echoes
#     OTHERIX_DNS_OK; on failure OTHERIX_DNS_FAIL. The managed bridge advertises
#     169.254.1.1 (option 6) so the lookup succeeds.
# The agent captures the serial console into serial.log. Quoted heredoc -> the $SC
# guest shell var stays literal, not expanded here.
cat >"$UD_FILE" <<'EOF'
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-bridge-dhcp.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix managed-bridge DHCP/DNS smoke sentinel
      After=network-online.target
      Wants=network-online.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; { echo OTHERIX_DHCP_ADDR; ip -4 addr show; echo OTHERIX_ROUTE; ip route; } > "$SC" 2>&1; if getent hosts archive.ubuntu.com >/dev/null 2>&1; then echo OTHERIX_DNS_OK > "$SC"; else echo OTHERIX_DNS_FAIL > "$SC"; fi'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-bridge-dhcp.service ]
EOF

# --- preconditions -----------------------------------------------------
echo "=== bridge-managed-dhcp smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

# default pool reconciled on node-1 (the VM is pinned to node-1 and created
# without --pool, resolving to the cluster default auto-provisioned per node).
pool_ready_node1() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready_node1 && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); both nodes ready; default pool ready on $NODE1"

# best-effort delete-first so a stale leftover from a prior run does not 409
delete_stale_resources

# --- step 1: managed bridge with DHCP + DNS + NAT egress ---------------
echo "=== step 1: managed bridge $NET (--managed --bridge-name $BRIDGE_NAME --egress nat --subnet $SUBNET --dhcp) ==="
otx network create "$NET" --type bridge --managed --bridge-name "$BRIDGE_NAME" \
  --egress nat --subnet "$SUBNET" --dhcp \
  || fail "network create $NET failed"
# Assert the bridge advertises NAT egress (it is what provisions the default route).
EGRESS="$(otx network get "$NET" --output json | jq -r '.egress')"
[[ "$EGRESS" == "nat" ]] || fail "$NET should have egress=nat, got '$EGRESS'"
pass "managed bridge $NET created (bridge $BRIDGE_NAME, subnet $SUBNET, dhcp on, egress nat)"
wait_net_ready_both "$NET"

# --- step 2: VM on the bridge; DHCP lease + DNS + default route ---------
echo "=== step 2: VM $VM on $NET (DHCP addressing) ==="
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

# Wait for the boot sentinel, then assert: a DHCP lease in 10.88.0.0/24, a
# successful resolver lookup (option 6 handed out 169.254.1.1), and a default
# route via 169.254.1.1 (option 3 - egress=nat). The leased address is read from
# the serial-logged `ip -4 addr show` (the CP does not surface DHCP leases).
if ! wait_serial "$SERIAL" "OTHERIX_DHCP_ADDR" "$GUEST_WAIT"; then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "no OTHERIX_DHCP_ADDR sentinel within ${GUEST_WAIT}s - $VM did not boot"
fi
# DHCP addressing: an address in the bridge subnet (10.88.0.x) on the wire.
if (( $(serial_count "$SERIAL" "inet 10\.88\.0\.") == 0 )); then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM got no DHCP lease in $SUBNET - per-node DHCP responder did not hand out an address"
fi
pass "$VM got a DHCP lease in $SUBNET (per-node DHCP responder on the managed bridge)"

# DNS: the resolver lookup must succeed (option 6 advertised 169.254.1.1).
if ! wait_serial "$SERIAL" "OTHERIX_DNS_OK" 120; then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM could not resolve $RESOLVE_NAME via $RESOLVER - DHCP option 6 not honoured on the managed bridge"
fi
pass "$VM resolved $RESOLVE_NAME via $RESOLVER (DHCP option 6 advertised the in-network resolver)"

# NAT egress: a default route via 169.254.1.1 (option 3). The route table was
# logged under OTHERIX_ROUTE; assert a `default` line via the anycast gateway.
if (( $(serial_count "$SERIAL" "^default.*${RESOLVER}") == 0 )); then
  echo "--- $VM serial tail ---"; dump_serial "$SERIAL"
  fail "$VM has no default route via $RESOLVER - egress=nat must advertise the anycast default route (DHCP option 3)"
fi
pass "$VM has a default route via $RESOLVER (egress=nat advertised the anycast gateway)"

# --- step 3: teardown --------------------------------------------------
echo "=== step 3: teardown VM + bridge ==="
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "VM + bridge deleted"

trap - EXIT
rm -rf "$TMPDIR_BR" 2>/dev/null || true
echo
echo "${GREEN}=== bridge-managed-dhcp smoke PASSED ===${NC}"
