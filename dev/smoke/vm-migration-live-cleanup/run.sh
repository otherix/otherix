#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Repeated live VM migration smoke - the real-agent gate for the migration
# cleanup-leak fix. Live migration used to leak per-migration QMP state, so a VM
# could be live-migrated only ONCE:
#   - the source/target leaked the `migtls` QMP TLS-creds object, so the SECOND
#     live migration of the same VM failed at qemu with
#       add client tls creds: object-add tls-creds-x509:
#       attempt to add duplicate property 'migtls'
#   - the target also leaked its incoming qemu process + the picked migration
#     port pair on failure/cancel, surfacing later as "address already in use" or
#     "vm already present".
# The fix deletes the `migtls` object on BOTH sides at the end of every migration
# and adds a target teardown owner that releases the incoming qemu + port pair.
#
# What this smoke proves: ONE VM can be LIVE-migrated REPEATEDLY (1->2->1->2),
# every migration reaching the 'completed' phase (live=true), with NO migration
# failing with `duplicate property 'migtls'` and NO port exhaustion. The SECOND
# migration (node-2 -> node-1) is the one that regressed before the fix - if it
# (or any of the three) fails with a migtls/port error_message, the leak is back.
#
# It is intentionally SIMPLER than vm-migration-live-overlay: there is no
# stationary peer and no cross-node connectivity teeth. The single migrant VM is
# created WITHOUT --disk-gib (image-sized boot disk, so the default-disk path is
# exercised too) and attached to a type=overlay network only because the
# network-aware scheduler refuses to place a VM on a node where a requested
# network has not reconciled to ready - the overlay just gives the VM a NIC that
# materialises on both nodes. The core assertion is purely the three migration
# records reaching 'completed'.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-deploy   (gets the fix onto the running agents)
# Both node-1 and node-2 ready, the overlay agent reconciler running, and the
# cluster default pool reconciled on both nodes.
#
# Usage: make smoke-vm-migration-live-cleanup
#        (or: bash dev/smoke/vm-migration-live-cleanup/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # the VM starts here; migrations bounce 1->2->1->2
NODE2="node-2"
NET="${NET_NAME:-migclean-smoke}"             # overlay network; delete-firsted (distinct from sibling smokes)
VM="${VM_NAME:-migclean}"                     # the single migrant VM (created WITHOUT --disk-gib)
SUBNET="10.76.0.0/24"                         # plain overlay subnet; distinct from sibling smokes (10.72/10.74)
IPV="10.76.0.10"                              # the VM's fixed overlay IP (no CP IPAM on a plain overlay)
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"           # seconds per live migrate cutover (TCG is slow; 3 sequential migrations need generous headroom)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds for the CP-projected status.phase / node
MIG_PHASE_WAIT="${MIG_PHASE_WAIT:-120}"       # seconds for the migration record -> completed
NET_READY_WAIT="${NET_READY_WAIT:-180}"       # seconds for the overlay to reconcile ready on both nodes

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME -> prints the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# vm_node NAME -> prints the VM's current node name ("" if unscheduled/gone). The
# CP keeps .node = current_node_id and only updates it after a migration cutover,
# so this is the node-change signal.
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // ""' 2>/dev/null || true; }

# assert_phase NAME WANT [TIMEOUT] -> poll status.phase until it equals WANT
assert_phase() {
  local name="$1" want="$2" to="${3:-$PHASE_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(vm_phase "$name")"
    [[ "$got" == "$want" ]] && { pass "$name phase=$want"; return 0; }
    sleep 2
  done
  fail "$name phase: want '$want' got '${got:-none}' after ${to}s"
}

# assert_node NAME WANT [TIMEOUT] -> poll the VM's current node until it equals WANT
assert_node() {
  local name="$1" want="$2" to="${3:-$PHASE_WAIT}" deadline got
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    got="$(vm_node "$name")"
    [[ "$got" == "$want" ]] && { pass "$name node=$want"; return 0; }
    sleep 2
  done
  fail "$name node: want '$want' got '${got:-none}' after ${to}s"
}

# net_ready_both NET -> 0 when the overlay reconciled to "ready" on both nodes.
# The scheduler's network-aware filter excludes any node where a requested network
# has not reconciled to ready, so a VM-create raced ahead of overlay
# materialisation fails with no_eligible_nodes. We gate on this first.
net_ready_both() {
  local net="$1" n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx network get "$net" --output json 2>/dev/null \
        | jq -r --arg n "$n" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
        == "ready" ]] || return 1
  done
  return 0
}

# pool_ready_both -> 0 when the cluster default pool reconciled to "ready" on both
# nodes. The VM below is created WITHOUT --pool, resolving to that default.
pool_ready_both() {
  local n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx pool get default --output json 2>/dev/null \
        | jq -r --arg n "$n" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]] || return 1
  done
  return 0
}

# migrate_and_assert FROM TO -> live-migrate the single VM FROM -> TO and assert
# the migration reaches phase=completed live=true. On any failure it prints the
# migration's error_message and fails with a clear message; an error_message that
# mentions `duplicate property 'migtls'`, "address already in use", or
# "vm already present" here means the cleanup-leak fix regressed.
migrate_and_assert() {
  local from="$1" to="$2"
  echo "=== migrate $VM (live) $from -> $to ==="
  [[ "$(vm_node "$VM")" == "$from" ]] || fail "$VM not on expected source $from before migrate (got '$(vm_node "$VM")')"

  otx vm migrate "$VM" --node "$to" \
    --wait --wait-timeout "${MIGRATE_WAIT}s" \
    || fail "vm migrate (live) $from -> $to did not complete within ${MIGRATE_WAIT}s"
  assert_node "$VM" "$to"
  assert_phase "$VM" running

  # Newest migration record for this VM is migration list's first row (migrations
  # are returned newest-first), exactly as the sibling live smokes derive MIGID.
  local migid
  migid="$(otx migration list --vm "$VMID" --output json 2>/dev/null | jq -r '.data[0].id // ""')"
  [[ "$migid" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve migration id for $VM ($from -> $to) (got '${migid:-none}')"
  info "migration id=$migid ($from -> $to)"

  local deadline mphase=""
  deadline=$(( SECONDS + MIG_PHASE_WAIT ))
  while (( SECONDS < deadline )); do
    mphase="$(otx migration get "$migid" --output json 2>/dev/null | jq -r '.phase // ""' || true)"
    [[ "$mphase" == "completed" ]] && break
    case "$mphase" in
      failed|cancelled)
        local emsg
        emsg="$(otx migration get "$migid" --output json 2>/dev/null | jq -r '.error_message // ""' || true)"
        echo "${RED}migration $migid ($from -> $to) error_message:${NC} ${emsg:-<none>}" >&2
        fail "migration $migid ($from -> $to) reached terminal '$mphase', expected completed"
        ;;
    esac
    sleep 2
  done
  if [[ "$mphase" != "completed" ]]; then
    local emsg
    emsg="$(otx migration get "$migid" --output json 2>/dev/null | jq -r '.error_message // ""' || true)"
    echo "${RED}migration $migid ($from -> $to) error_message:${NC} ${emsg:-<none>}" >&2
    fail "migration $migid ($from -> $to) phase: want 'completed' got '${mphase:-none}' after ${MIG_PHASE_WAIT}s"
  fi

  local mlive
  mlive="$(otx migration get "$migid" --output json 2>/dev/null | jq -r '.live // empty' || true)"
  [[ "$mlive" == "true" ]] || fail "migration $migid ($from -> $to) live: want 'true' got '${mlive:-none}' (this is the LIVE smoke)"
  pass "migration $migid ($from -> $to) phase=completed live=true"
}

cleanup() {
  echo "--- cleanup ---"
  if [ -n "${SMOKE_KEEP:-}" ]; then
    info "SMOKE_KEEP set; leaving VM ${VM} and network ${NET} in place for inspection"
    return
  fi
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- the migrant cloud-config (static overlay IP only) -----------------
# gen_userdata SELF_IP -> a #cloud-config that statically configures the single
# overlay NIC (en* glob) to SELF_IP (no DHCP on a plain overlay) via BOTH a
# netplan file AND a belt-and-suspenders `ip addr replace` in runcmd that does
# NOT depend on netplan/networkd succeeding (the proven overlay-vm pattern). This
# smoke has no connectivity teeth, so there is no peer probe - the NIC is here
# only so the VM is a normal overlay-attached guest under live migration.
# Quoted heredoc -> @@SELF@@ is substituted by sed; the guest's own $IF stays
# literal.
gen_userdata() {
  local self="$1"
  sed -e "s|@@SELF@@|${self}|g" <<'EOF'
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
            addresses: [@@SELF@@/24]
runcmd:
  - netplan apply || true
  - sleep 2
  - |
    IF=$(ls /sys/class/net | grep -E '^en' | head -1)
    ip addr replace @@SELF@@/24 dev "$IF" 2>/dev/null || true
    ip link set "$IF" up 2>/dev/null || true
EOF
}

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-live-cleanup smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

# default pool reconciled on BOTH nodes (CP auto-provision); the VM resolves to it.
deadline=$(( SECONDS + 60 )); pool_ok=0
while (( SECONDS < deadline )); do pool_ready_both && { pool_ok=1; break; }; sleep 3; done
(( pool_ok == 1 )) || fail "default pool not ready on both nodes within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 + $NODE2 ready; default pool ready on both"

# --- step 1: overlay network ------------------------------------------
echo "=== step 1: create overlay network $NET ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers
otx network create "$NET" --type overlay --subnet "$SUBNET" \
  || fail "network create (overlay) failed"
VNI="$(otx network get "$NET" --output json | jq -r '.vni')"
[[ "$VNI" =~ ^[0-9]+$ ]] || fail "no vni on $NET after create"
pass "overlay $NET created (vni=$VNI subnet=$SUBNET)"

# Gate on overlay materialisation before placing the VM: the network-aware
# scheduler refuses to place a VM on a node where the overlay has not reconciled
# to ready, and the migrations need it ready on BOTH nodes.
info "waiting for $NET to reconcile ready on both nodes (<= ${NET_READY_WAIT}s)"
deadline=$(( SECONDS + NET_READY_WAIT )); net_ok=0
while (( SECONDS < deadline )); do net_ready_both "$NET" && { net_ok=1; break; }; sleep 3; done
(( net_ok == 1 )) || { otx network get "$NET" --output json | jq -c '.status.nodes'; fail "$NET did not reconcile ready on both nodes within ${NET_READY_WAIT}s"; }
pass "$NET reconciled ready on both nodes"

# --- step 2: the single migrant VM on node-1 (NO --disk-gib) -----------
echo "=== step 2: create $VM on $NODE1 ($IPV, image-sized boot disk, NO --disk-gib) -> running ==="
gen_userdata "$IPV" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --vcpus 2 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM id (got '$VMID')"
[[ "$(vm_node "$VM")" == "$NODE1" ]] || fail "$VM did not land on $NODE1 (the first migration source)"
info "$VM id=$VMID on $NODE1 (image-sized boot disk, no --disk-gib)"
pass "$VM created and running on $NODE1"

# --- step 3: three sequential live migrations (1->2->1->2) -------------
# The whole point: the SAME VM survives repeated live migrations. Before the fix
# the SECOND (node-2 -> node-1) failed with `duplicate property 'migtls'` because
# the migtls QMP object leaked. Each call asserts the migration reached completed
# live=true; a migtls/port error_message inside any of them = regression.
echo "=== step 3: repeated live migration ($NODE1 -> $NODE2 -> $NODE1 -> $NODE2) ==="
migrate_and_assert "$NODE1" "$NODE2"   # 1 -> 2
migrate_and_assert "$NODE2" "$NODE1"   # 2 -> 1  (this is the one that regressed before the fix)
migrate_and_assert "$NODE1" "$NODE2"   # 1 -> 2
pass "all THREE live migrations reached completed (no migtls leak, no port exhaustion)"

# --- step 4: the VM ends up on node-2, still running -------------------
echo "=== step 4: final state - $VM on $NODE2, still running ==="
assert_node "$VM" "$NODE2"
assert_phase "$VM" running
pass "$VM ends on $NODE2 and is still running after three live migrations"

# --- step 5: teardown --------------------------------------------------
echo "=== step 5: teardown VM + overlay ==="
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "VM + overlay deleted"

trap - EXIT
echo
echo "=== vm-migration-live-cleanup smoke PASSED ==="
