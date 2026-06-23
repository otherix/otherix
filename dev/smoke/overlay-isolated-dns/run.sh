#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Isolated-overlay DNS smoke - proves that an overlay's DHCP addressing and its
# in-overlay resolver (169.254.1.1, advertised via DHCP option 6) are decoupled
# from egress NAT, and that the resolver advertisement is opt-out via --dns.
#
# What it proves, end to end, as a real operator (CLI only):
#   - ISOLATED overlay (--type overlay --subnet --dhcp, NO --egress): a VM on it
#     gets an IPv4 lease in-subnet from the per-node DHCP responder AND can
#     resolve a name through the overlay resolver 169.254.1.1 (option 6 handed
#     out), while having NO default route (no internet egress). DHCP addressing
#     and in-overlay DNS work without NAT.
#   - DNS-SUPPRESSED overlay (--type overlay --subnet --dhcp --dns=false): a VM on
#     it still gets an IPv4 lease, but name resolution via 169.254.1.1 FAILS - no
#     resolver is advertised (no DHCP option 6), so the guest has no nameserver.
#
# Both VMs DHCP their NIC (netplan dhcp4: true), then a boot sentinel echoes the
# leased address, the `getent hosts` result, and the route table to the guest
# serial console. The agent persists that console to <state_root>/vms/<id>/
# serial.log, so every in-guest assertion reads from serial.log (no SSH into the
# guest). getent hosts resolves via the guest's /etc/resolv.conf, which the DHCP
# client populates from option 6 - so a successful lookup on the isolated overlay
# and a failed lookup on the dns=false overlay are attributable to option 6 alone.
#
# Note on reading the leased IP: the CP does not surface a VM's DHCP lease on
# `vm get` (DHCP is node-local observed state), so the leased address is read
# from the serial-logged `ip -4 addr show`, the same channel the sibling
# vm-network-config smoke uses for its address assertion.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# Both nodes ready with the cluster default pool reconciled.
#
# Usage: make smoke-overlay-isolated-dns
#   (or: bash dev/smoke/overlay-isolated-dns/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NODE2="node-2"

NET_DNS="iso-dns"                  # isolated overlay, dns on (default)
SUBNET_DNS="10.83.0.0/24"
VM_DNS="iso-dns-vm"

NET_NODNS="iso-nodns"             # isolated overlay, dns suppressed
SUBNET_NODNS="10.84.0.0/24"
VM_NODNS="iso-nodns-vm"

RESOLVER="169.254.1.1"            # in-overlay resolver advertised via DHCP option 6
RESOLVE_NAME="archive.ubuntu.com" # name the in-overlay forwarder answers from the node upstream

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"  # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"    # seconds to wait for the boot sentinel (TCG is slow)
NET_WAIT="${NET_WAIT:-180}"        # seconds to wait for an overlay to reconcile ready on both nodes

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# net_ready_both NET -> 0 when the overlay reconciled to "ready" on both nodes.
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
# Both VMs are pinned to node-1, so the path always resolves under SMOKE_STATE_1.
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

TMPDIR_ISO=""
delete_stale_resources() {
  otx vm delete "$VM_DNS" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM_NODNS" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET_DNS" --force >/dev/null 2>&1 || true
  otx network delete "$NET_NODNS" --force >/dev/null 2>&1 || true
}
cleanup() {
  echo "--- cleanup ---"
  delete_stale_resources
  [ -n "$TMPDIR_ISO" ] && rm -rf "$TMPDIR_ISO" 2>/dev/null || true
}
trap cleanup EXIT

# --- cloud-init payloads (network-config + user-data) ------------------
# Written to temp files; passed to the CLI via --network-config / --user-data.
TMPDIR_ISO="$(mktemp -d)"
NC_FILE="${TMPDIR_ISO}/network-config.yaml"
UD_FILE="${TMPDIR_ISO}/user-data.yaml"

# network-config (netplan v2): DHCP the single overlay NIC. The lease (address,
# and - only when the overlay advertises it - the option-6 nameserver) is the
# ONLY source of the guest's addressing and resolver, so a name that resolves
# is attributable to option 6, and a leased address proves the DHCP responder.
# dhcp4-overrides.use-dns lets netplan honour the option-6 nameserver. Marked
# optional so systemd-networkd-wait-online does not block boot if a renewal lags.
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
# write_files netplan, no `netplan apply`), so addressing/resolution come solely
# from DHCP. A systemd oneshot ordered After=network-online.target echoes, to the
# arch-correct serial device:
#   - OTHERIX_DHCP_ADDR + `ip -4 addr show`   (the leased IPv4)
#   - OTHERIX_ROUTE + `ip route`               (presence/absence of a default route)
#   - the resolver probe: getent hosts <name> resolves via /etc/resolv.conf, which
#     the DHCP client populates from option 6. On success it echoes
#     OTHERIX_DNS_OK; on failure OTHERIX_DNS_FAIL. The isolated overlay advertises
#     169.254.1.1 (option 6) so the lookup succeeds; the --dns=false overlay hands
#     out no nameserver so it fails.
# The agent captures the serial console into serial.log. Quoted heredoc -> the $SC
# guest shell var stays literal, not expanded here.
cat >"$UD_FILE" <<'EOF'
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-iso-dns.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix isolated-overlay DNS smoke sentinel
      After=network-online.target
      Wants=network-online.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; { echo OTHERIX_DHCP_ADDR; ip -4 addr show; echo OTHERIX_ROUTE; ip route; } > "$SC" 2>&1; if getent hosts archive.ubuntu.com >/dev/null 2>&1; then echo OTHERIX_DNS_OK > "$SC"; else echo OTHERIX_DNS_FAIL > "$SC"; fi'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-iso-dns.service ]
EOF

# --- preconditions -----------------------------------------------------
echo "=== overlay-isolated-dns smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

# default pool reconciled on node-1 (both VMs are pinned to node-1 and created
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

# --- step 1: isolated overlay with DHCP + DNS (no egress) --------------
echo "=== step 1: isolated overlay $NET_DNS (--dhcp, dns default-on, NO egress) ==="
otx network create "$NET_DNS" --type overlay --subnet "$SUBNET_DNS" --dhcp \
  || fail "network create $NET_DNS failed"
# Assert the overlay is genuinely isolated (no egress) - DHCP/DNS must work anyway.
EGRESS_DNS="$(otx network get "$NET_DNS" --output json | jq -r '.egress')"
[[ "$EGRESS_DNS" == "none" ]] || fail "$NET_DNS should have egress=none (isolated), got '$EGRESS_DNS'"
pass "isolated overlay $NET_DNS created (subnet $SUBNET_DNS, dhcp on, egress none)"
wait_net_ready_both "$NET_DNS"

# --- step 2: VM on the isolated overlay; DHCP lease + DNS + no default ---
echo "=== step 2: VM $VM_DNS on $NET_DNS (DHCP addressing) ==="
otx vm create "$VM_DNS" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET_DNS" \
  --vcpus 2 --memory-mb 2048 \
  --user-data "$UD_FILE" --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_DNS did not reach running within ${CREATE_WAIT}s"
VMID_DNS="$(otx vm get "$VM_DNS" --output json | jq -r '.id')"
[[ "$VMID_DNS" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM_DNS id (got '$VMID_DNS')"
SERIAL_DNS="$(serial_for "$VMID_DNS")"
info "$VM_DNS id=$VMID_DNS  serial=$SERIAL_DNS"
pass "$VM_DNS created and running"

# Wait for the boot sentinel, then assert: a DHCP lease in 10.83.0.0/24, a
# successful resolver lookup (option 6 handed out 169.254.1.1), and NO default
# route (isolated - no internet). The leased address is read from the
# serial-logged `ip -4 addr show` (the CP does not surface DHCP leases).
if ! wait_serial "$SERIAL_DNS" "OTHERIX_DHCP_ADDR" "$GUEST_WAIT"; then
  echo "--- $VM_DNS serial tail ---"; dump_serial "$SERIAL_DNS"
  fail "no OTHERIX_DHCP_ADDR sentinel within ${GUEST_WAIT}s - $VM_DNS did not boot"
fi
# DHCP addressing: an address in the overlay subnet (10.83.0.x) on the wire.
if (( $(serial_count "$SERIAL_DNS" "inet 10\.83\.0\.") == 0 )); then
  echo "--- $VM_DNS serial tail ---"; dump_serial "$SERIAL_DNS"
  fail "$VM_DNS got no DHCP lease in $SUBNET_DNS - per-node DHCP responder did not hand out an address"
fi
pass "$VM_DNS got a DHCP lease in $SUBNET_DNS (DHCP addressing works with no egress)"

# DNS: the resolver lookup must succeed (option 6 advertised 169.254.1.1).
if ! wait_serial "$SERIAL_DNS" "OTHERIX_DNS_OK" 120; then
  echo "--- $VM_DNS serial tail ---"; dump_serial "$SERIAL_DNS"
  fail "$VM_DNS could not resolve $RESOLVE_NAME via $RESOLVER - DHCP option 6 not honoured on isolated overlay"
fi
pass "$VM_DNS resolved $RESOLVE_NAME via $RESOLVER (DHCP option 6 advertised the in-overlay resolver, no egress)"

# Isolated: NO default route (no internet egress). The route table was logged
# under OTHERIX_ROUTE; assert no `default` line appears in serial.log.
if (( $(serial_count "$SERIAL_DNS" "^default") > 0 )); then
  echo "--- $VM_DNS serial tail ---"; dump_serial "$SERIAL_DNS"
  fail "$VM_DNS has a default route - the isolated overlay must not give the VM internet egress"
fi
pass "$VM_DNS has NO default route (isolated overlay: addressing + DNS, but no internet)"

# --- step 3: dns-suppressed overlay; DHCP lease but DNS fails ----------
echo "=== step 3: isolated overlay $NET_NODNS (--dhcp --dns=false) ==="
otx network create "$NET_NODNS" --type overlay --subnet "$SUBNET_NODNS" --dhcp --dns=false \
  || fail "network create $NET_NODNS failed"
pass "isolated overlay $NET_NODNS created (subnet $SUBNET_NODNS, dhcp on, dns off)"
wait_net_ready_both "$NET_NODNS"

echo "=== step 4: VM $VM_NODNS on $NET_NODNS (DHCP lease, but no resolver) ==="
otx vm create "$VM_NODNS" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET_NODNS" \
  --vcpus 2 --memory-mb 2048 \
  --user-data "$UD_FILE" --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_NODNS did not reach running within ${CREATE_WAIT}s"
VMID_NODNS="$(otx vm get "$VM_NODNS" --output json | jq -r '.id')"
[[ "$VMID_NODNS" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM_NODNS id (got '$VMID_NODNS')"
SERIAL_NODNS="$(serial_for "$VMID_NODNS")"
info "$VM_NODNS id=$VMID_NODNS  serial=$SERIAL_NODNS"
pass "$VM_NODNS created and running"

# Still gets a DHCP lease (addressing is independent of the resolver advert).
if ! wait_serial "$SERIAL_NODNS" "OTHERIX_DHCP_ADDR" "$GUEST_WAIT"; then
  echo "--- $VM_NODNS serial tail ---"; dump_serial "$SERIAL_NODNS"
  fail "no OTHERIX_DHCP_ADDR sentinel within ${GUEST_WAIT}s - $VM_NODNS did not boot"
fi
if (( $(serial_count "$SERIAL_NODNS" "inet 10\.84\.0\.") == 0 )); then
  echo "--- $VM_NODNS serial tail ---"; dump_serial "$SERIAL_NODNS"
  fail "$VM_NODNS got no DHCP lease in $SUBNET_NODNS - addressing must work even with --dns=false"
fi
pass "$VM_NODNS got a DHCP lease in $SUBNET_NODNS (addressing works with --dns=false)"

# DNS MUST fail: --dns=false suppresses option 6, so the guest has no nameserver.
if ! wait_serial "$SERIAL_NODNS" "OTHERIX_DNS_FAIL" 120; then
  echo "--- $VM_NODNS serial tail ---"; dump_serial "$SERIAL_NODNS"
  fail "$VM_NODNS resolved $RESOLVE_NAME despite --dns=false - DHCP option 6 was advertised when it should not be"
fi
# Defensive: the success sentinel must NOT be present for this VM.
if (( $(serial_count "$SERIAL_NODNS" "OTHERIX_DNS_OK") > 0 )); then
  echo "--- $VM_NODNS serial tail ---"; dump_serial "$SERIAL_NODNS"
  fail "$VM_NODNS emitted OTHERIX_DNS_OK - resolution succeeded despite --dns=false"
fi
pass "$VM_NODNS could NOT resolve via $RESOLVER (--dns=false suppresses DHCP option 6)"

# --- step 5: teardown --------------------------------------------------
echo "=== step 5: teardown VMs + overlays ==="
otx vm delete "$VM_DNS" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_NODNS" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET_DNS" --force >/dev/null 2>&1 || true
otx network delete "$NET_NODNS" --force >/dev/null 2>&1 || true
pass "VMs + overlays deleted"

trap - EXIT
rm -rf "$TMPDIR_ISO" 2>/dev/null || true
echo
echo "${GREEN}=== overlay-isolated-dns smoke PASSED ===${NC}"
