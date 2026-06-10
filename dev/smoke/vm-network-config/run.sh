#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM network-config smoke - proves a real VM gets its STATIC guest address from
# an operator-supplied cloud-init network-config delivered via
# `otherix vm create --network-config <file>`, with NO write_files-netplan +
# `netplan apply` hack in user-data. This is the end-to-end test of the
# --network-config passthrough as a real operator CLI flow.
#
# What it proves, end to end, on ONE VM pinned to node-1:
#   - operator CLI (primary): `otherix network create --type overlay --egress nat`
#     creates an egress overlay (link-local anycast gateway 169.254.1.1, per-node
#     SNAT); `otherix vm create --network <overlay> --user-data <ud>
#     --network-config <nc>` attaches a NIC and ships BOTH cloud-init channels;
#   - the guest comes up with the static address 10.62.0.5/24 from the
#     network-config alone - the user-data does NOT touch networking (no
#     write_files netplan, no `netplan apply`), so a static IP on the wire proves
#     --network-config (NoCloud /network-config) was honoured, not DHCP;
#   - the user-data is ONLY a verification sentinel: a systemd oneshot echoes
#     OTHERIX_NETCFG_UP plus `ip -4 addr show` to the serial console on boot, and
#     (bonus) probes egress to 169.254.1.1 / 8.8.8.8 and echoes OTHERIX_EGRESS_OK
#     on success. The agent persists the console to
#     <state_root>/vms/<id>/serial.log (no SSH into the guest required).
#
# PRIMARY assertion: the serial log shows 10.62.0.5 (static IP applied from
# --network-config). egress (OTHERIX_EGRESS_OK) is a best-effort bonus signal.
#
# No pool staging: the CP auto-provisions the cluster default pool ("default") on
# every node as it reaches ready, so the VM is created WITHOUT --pool (resolving
# to that default) from an image URL - the agent materializes the image into the
# pool's basename-keyed cache on first use (IfNotPresent).
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready with the cluster default pool reconciled.
#
# Usage: make smoke-vm-network-config   (or: bash dev/smoke/vm-network-config/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
NET="${NET_NAME:-vmnc-smoke}"             # egress overlay; delete-firsted
VM="${VM_NAME:-vmnc-smoke}"               # the single VM; delete-firsted
SUBNET="10.62.0.0/24"
STATIC_IP="10.62.0.5"                      # the address the network-config sets
GATEWAY="169.254.1.1"                      # link-local anycast egress gateway
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"         # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"           # seconds to wait for the boot sentinel (TCG is slow)
NET_WAIT="${NET_WAIT:-180}"               # seconds to wait for the overlay to reconcile ready

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# net_ready NET -> 0 when the overlay reconciled to "ready" on node-1. The
# network-aware scheduler refuses to place a VM on a node where a requested
# network has not reconciled ready (no_eligible_nodes), so gate on this first.
net_ready() {
  [[ "$(otx network get "$1" --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" == "ready" ]]
}

# serial_count PATTERN -> count of lines matching PATTERN in the VM's serial.log
# (0 when the file is absent). The agent appends the guest console there; callers
# wait for a count rather than a single grep so a stale match across reboots does
# not race them.
serial_count() {
  local n
  n="$(run_on "$SMOKE_HANDLE_1" sudo grep -c "$1" "$SERIAL" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_serial PATTERN TIMEOUT -> block until PATTERN appears in serial.log.
wait_serial() {
  local pat="$1" to="$2" deadline; deadline=$(( SECONDS + to ))
  info "watching $SERIAL on $NODE1 for '$pat' (<= ${to}s)"
  while (( SECONDS < deadline )); do
    (( $(serial_count "$pat") > 0 )) && return 0
    sleep 5
  done
  return 1
}

TMPDIR_NC=""
# delete_stale_resources removes the CP-side VM + overlay (best-effort). Used both
# for the pre-run delete-first and by cleanup. It deliberately does NOT touch the
# local temp dir, so the pre-run delete-first cannot wipe the cloud-init payloads
# (which are written before step 1).
delete_stale_resources() {
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
}
cleanup() {
  echo "--- cleanup ---"
  delete_stale_resources
  [ -n "$TMPDIR_NC" ] && rm -rf "$TMPDIR_NC" 2>/dev/null || true
}
trap cleanup EXIT

# --- cloud-init payloads (network-config + user-data) ------------------
# Written to temp files; passed to the CLI via --network-config / --user-data.
TMPDIR_NC="$(mktemp -d)"
NC_FILE="${TMPDIR_NC}/network-config.yaml"
UD_FILE="${TMPDIR_NC}/user-data.yaml"

# network-config (netplan v2): the ONLY source of networking for this VM. Static
# 10.62.0.5/24, DNS + default route via the link-local anycast gateway
# 169.254.1.1 (off-subnet, so the default route is declared on-link, and a
# link-scope route to the gateway/32 makes it reachable). Shape copied from the
# overlay-egress example in docs/linux-development.md.
cat >"$NC_FILE" <<EOF
network:
  version: 2
  ethernets:
    eth0:
      match:
        name: "en*"
      addresses: [${STATIC_IP}/24]
      nameservers:
        addresses: [${GATEWAY}]
      routes:
        - to: ${GATEWAY}/32
          scope: link
        - to: default
          via: ${GATEWAY}
          on-link: true
EOF

# user-data: ONLY a verification sentinel. It must NOT configure networking - no
# write_files netplan, no `netplan apply` - so that a static address on the wire
# is attributable solely to the network-config above. A systemd oneshot ordered
# After=network-online.target echoes OTHERIX_NETCFG_UP + `ip -4 addr show` to the
# arch-correct serial device on boot, then probes egress (gateway, then 8.8.8.8)
# and echoes OTHERIX_EGRESS_OK on the first success. The agent captures the
# serial console into serial.log. Quoted heredoc -> the $SC guest shell var stays
# literal, not expanded here.
cat >"$UD_FILE" <<'EOF'
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-netcfg-up.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix network-config smoke sentinel
      After=network-online.target
      Wants=network-online.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; { echo OTHERIX_NETCFG_UP; ip -4 addr show; } > "$SC" 2>&1; for t in 169.254.1.1 8.8.8.8; do if ping -c2 -W2 "$t" >/dev/null 2>&1; then echo "OTHERIX_EGRESS_OK via $t" > "$SC"; break; fi; done'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-netcfg-up.service ]
EOF

# --- preconditions -----------------------------------------------------
echo "=== vm-network-config smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# default pool reconciled on node-1 (the VM is created without --pool and resolves
# to the cluster default, auto-provisioned per node on ready).
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 ready; default pool ready"

# --- step 1: egress overlay network ------------------------------------
echo "=== step 1: create egress overlay $NET ==="
delete_stale_resources   # best-effort delete-first of a stale leftover (keeps temp payloads)
otx network create "$NET" --type overlay --subnet "$SUBNET" --egress nat \
  || fail "network create $NET failed"
pass "egress overlay $NET created (subnet $SUBNET, egress nat)"

# Gate on overlay materialisation: the network-aware scheduler refuses to place a
# VM on a node where the overlay has not reconciled ready (no_eligible_nodes).
deadline=$(( SECONDS + NET_WAIT )); net_ok=0
info "waiting for $NET to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do
  if net_ready "$NET"; then net_ok=1; break; fi
  sleep 3
done
(( net_ok == 1 )) || { otx network get "$NET" --output json | jq -c '.status.nodes'; fail "$NET did not reconcile ready on $NODE1 within ${NET_WAIT}s"; }
pass "$NET reconciled ready on $NODE1"

# --- step 2: create the VM with BOTH cloud-init channels ----------------
echo "=== step 2: create $VM on $NET with --user-data + --network-config ==="
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --vcpus 2 --memory-mb 2048 \
  --user-data "$UD_FILE" --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
SERIAL="$(smoke_state 1)/vms/${VMID}/serial.log"
info "VM id=$VMID  serial=$SERIAL"
pass "created and running"

# --- step 3: static IP from --network-config (the money shot) -----------
echo "=== step 3: static guest IP $STATIC_IP from --network-config (no netplan hack) ==="
# Wait for the boot sentinel, then assert the static address is present in the
# serial-logged `ip -4 addr show`. The user-data configured NO networking, so a
# 10.62.0.5 on the wire proves --network-config (NoCloud /network-config) drove
# addressing, not DHCP.
if ! wait_serial "OTHERIX_NETCFG_UP" "$GUEST_WAIT"; then
  echo "--- $VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "$SERIAL" 2>/dev/null || true
  fail "no OTHERIX_NETCFG_UP sentinel within ${GUEST_WAIT}s - guest did not boot"
fi
if (( $(serial_count "$STATIC_IP") == 0 )); then
  echo "--- $VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "$SERIAL" 2>/dev/null || true
  fail "static IP $STATIC_IP not seen in serial.log - --network-config was not applied (DHCP/no-addr?)"
fi
pass "static IP $STATIC_IP present in guest - --network-config honoured (no write_files/netplan hack)"

# --- step 4: egress (bonus) --------------------------------------------
echo "=== step 4: egress via $GATEWAY / 8.8.8.8 (bonus) ==="
# The static IP assertion above is the heart of this smoke; egress is a bonus
# signal and never fails the run on its own (the gateway/SNAT path is covered by
# the dedicated overlay-egress smoke). Give it a short extra window after the
# network is up.
if wait_serial "OTHERIX_EGRESS_OK" 120; then
  pass "egress OK (OTHERIX_EGRESS_OK)"
else
  info "egress sentinel not seen (bonus only; static-IP assertion already passed)"
fi

# --- step 5: teardown --------------------------------------------------
echo "=== step 5: teardown VM + overlay ==="
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "VM + overlay deleted"

trap - EXIT
rm -rf "$TMPDIR_NC" 2>/dev/null || true
echo
echo "${GREEN}=== vm-network-config smoke PASSED ===${NC}"
