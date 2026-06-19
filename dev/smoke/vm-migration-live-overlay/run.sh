#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Live VM migration + overlay data-plane smoke - proves that a LIVE
# migration of an OVERLAY-attached VM keeps the encrypted VXLAN data plane
# following the guest: connectivity to/from the migrated VM continues across the
# cutover with no prolonged blackhole. This is the real-agent gate for the epoch
# fence + fast-push FDB nudge + GARP; mock-green is NOT
# sufficient. It combines the two sibling smokes:
#   - vm-migration-live  -> the LIVE cutover (otherix vm migrate, no --offline)
#                           and the runcmd-survives-migration serial heartbeat.
#   - overlay-vm         -> two real VMs on different nodes on one type=overlay
#                           network, cross-node reachability over the CP-FDB.
#
# Topology and what it proves, end to end:
#   VM-B (node-2, stationary peer) and VM-A (node-1, the migrant) attach one NIC
#   each to the same type=overlay network with fixed cloud-init overlay IPs (no
#   CP IPAM on a plain overlay - same as overlay-vm). BOTH guests run a long-lived
#   runcmd loop that, once a second, TCP-probes the PEER's sshd (:22) over the
#   overlay and, on success, writes "OVERLAY_PING_OK <n>" to the serial console
#   with a monotonically incrementing <n> (the minimal cloudimg has no ping, so
#   /dev/tcp, exactly like overlay-vm). The agent persists each guest's console to
#   <state_root>/vms/<id>/serial.log (append-only; the VM id is STABLE across the
#   migration, so VM-A's post-cutover log lives on the TARGET node under the same
#   id).
#
#   create A+B          -> both running, both OVERLAY_PING_OK counters climbing
#                          (cross-node overlay reachability established both ways)
#   migrate A (live)    -> node-1 -> node-2; the migration record reaches
#                          'completed' (live=true) and VM-A's current node becomes
#                          node-2.
#   THE TEETH           -> within a few seconds of cutover:
#                          (forward) VM-A's OVERLAY_PING_OK <n> CONTINUES advancing
#                            on the TARGET serial.log -> A still reaches B over the
#                            overlay after the move (the data plane followed it);
#                          (reverse) VM-B's OVERLAY_PING_OK <n> also keeps
#                            advancing -> B still reaches A at its new location
#                            (GARP/FDB repoint worked).
#                          A stall beyond STALL_THRESHOLD seconds with no counter
#                          progress is the blackhole this smoke guards against -> fail.
#
# Why a counter, not a one-shot sentinel: a one-shot "did it ever connect" proves
# nothing about the cutover - it could have connected once before the migrate and
# never again. The advancing counter, sampled BEFORE and AFTER the move, is what
# proves the overlay kept carrying traffic THROUGH the cutover.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# Both node-1 and node-2 ready, otwg0 up, the overlay agent reconciler running,
# and the cluster default pool reconciled on both nodes.
#
# Usage: make smoke-vm-migration-live-overlay
#        (or: bash dev/smoke/vm-migration-live-overlay/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # source node (VM-A is pinned here)
NODE2="node-2"                                # target node (VM-B is pinned here; A migrates here)
NET="${NET_NAME:-migovl-smoke}"               # overlay network; delete-firsted
VMA="${VMA_NAME:-migovl-a}"                   # the migrant
VMB="${VMB_NAME:-migovl-b}"                   # the stationary peer
SUBNET="10.72.0.0/24"                         # plain overlay subnet (no CP IPAM); IPs set in cloud-init
IPA="10.72.0.10"                              # VM-A fixed overlay IP
IPB="10.72.0.11"                              # VM-B fixed overlay IP
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds per vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"           # seconds for the live migrate cutover (disk copy on TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds for the CP-projected status.phase / node
MIG_PHASE_WAIT="${MIG_PHASE_WAIT:-120}"       # seconds for the migration record -> completed
PING_WAIT="${PING_WAIT:-720}"                 # seconds for the first OVERLAY_PING_OK to appear (TCG boot is slow)
NET_READY_WAIT="${NET_READY_WAIT:-180}"       # seconds for the overlay to reconcile ready on both nodes
STALL_THRESHOLD="${STALL_THRESHOLD:-15}"      # seconds of no counter progress after cutover = blackhole = fail
ADVANCE_WAIT="${ADVANCE_WAIT:-60}"            # seconds to wait for the post-cutover counter to start advancing

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
# nodes. The VMs below are created WITHOUT --pool, resolving to that default.
pool_ready_both() {
  local n
  for n in "$NODE1" "$NODE2"; do
    [[ "$(otx pool get default --output json 2>/dev/null \
        | jq -r --arg n "$n" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]] || return 1
  done
  return 0
}

# ping_counter HANDLE STATE VMID -> the maximum OVERLAY_PING_OK <n> seen in the
# VM's serial.log on the node identified by HANDLE/STATE, or empty when no line is
# present yet. Parses "OVERLAY_PING_OK <n>" and takes the largest <n>. The log is
# append-only -> this is monotonic on a single node.
ping_counter() {
  local handle="$1" state="$2" vmid="$3" out
  out="$(run_on "$handle" sudo grep -oE 'OVERLAY_PING_OK [0-9]+' \
      "${state}/vms/${vmid}/serial.log" 2>/dev/null)" || true
  [[ -n "$out" ]] || { printf ''; return 0; }
  printf '%s\n' "$out" | awk '{print $2}' | sort -n | tail -1
}

# wait_ping HANDLE STATE VMID -> block until at least one OVERLAY_PING_OK line
# appears in the VM's serial.log on that node, then print the counter seen. This
# is the cross-node-reachability sentinel: it only appears after the guest TCP-
# connects to its peer over the overlay.
wait_ping() {
  local handle="$1" state="$2" vmid="$3" deadline c; deadline=$(( SECONDS + PING_WAIT ))
  info "waiting for OVERLAY_PING_OK on $handle (vmid=$vmid, <= ${PING_WAIT}s)"
  while (( SECONDS < deadline )); do
    c="$(ping_counter "$handle" "$state" "$vmid")"
    [[ -n "$c" ]] && { printf '%s' "$c"; return 0; }
    sleep 5
  done
  echo "--- serial tail ($handle vmid=$vmid) ---" >&2
  run_on "$handle" sudo tail -40 "${state}/vms/${vmid}/serial.log" 2>/dev/null >&2 || true
  fail "no OVERLAY_PING_OK appeared on $handle (vmid=$vmid) within ${PING_WAIT}s"
}

# assert_advancing LABEL HANDLE STATE VMID BASELINE -> assert the VM's
# OVERLAY_PING_OK counter advances PAST baseline within ADVANCE_WAIT, never
# stalling longer than STALL_THRESHOLD between observed increments. This is the
# anti-blackhole teeth: after the live cutover the overlay must keep carrying
# traffic, so the counter must keep climbing. A stall > STALL_THRESHOLD means a
# prolonged blackhole - the failure this smoke guards against.
assert_advancing() {
  local label="$1" handle="$2" state="$3" vmid="$4" baseline="$5"
  local deadline last last_progress now cur
  deadline=$(( SECONDS + ADVANCE_WAIT ))
  last="$baseline"
  last_progress=$SECONDS
  info "$label: watching counter advance past $baseline (<= ${ADVANCE_WAIT}s, stall budget ${STALL_THRESHOLD}s)"
  while (( SECONDS < deadline )); do
    cur="$(ping_counter "$handle" "$state" "$vmid")"
    if [[ "$cur" =~ ^[0-9]+$ ]] && (( cur > last )); then
      last="$cur"
      last_progress=$SECONDS
      if (( last > baseline )); then
        pass "$label: counter advanced $baseline -> $last after cutover (overlay data plane followed the guest)"
        return 0
      fi
    fi
    now=$SECONDS
    if (( now - last_progress > STALL_THRESHOLD )); then
      echo "--- serial tail ($handle vmid=$vmid) ---" >&2
      run_on "$handle" sudo tail -60 "${state}/vms/${vmid}/serial.log" 2>/dev/null >&2 || true
      fail "$label: counter stalled at $last for > ${STALL_THRESHOLD}s (baseline $baseline) - overlay BLACKHOLE across the cutover"
    fi
    sleep 2
  done
  fail "$label: counter did not advance past baseline $baseline within ${ADVANCE_WAIT}s (last=$last)"
}

cleanup() {
  echo "--- cleanup ---"
  if [ -n "${SMOKE_KEEP:-}" ]; then
    info "SMOKE_KEEP set; leaving VMs ${VMA}/${VMB} and network ${NET} in place for inspection"
    return
  fi
  otx vm delete "$VMA" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VMB" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- the per-VM cloud-config (static overlay IP + every-second peer probe) ---
# gen_userdata SELF_IP PEER_IP -> a #cloud-config that:
#   - statically configures the single overlay NIC (en* glob) to SELF_IP (no DHCP
#     on a plain overlay) via BOTH a netplan file AND a belt-and-suspenders
#     `ip addr replace` in runcmd that does NOT depend on netplan/networkd
#     succeeding (the proven overlay-vm pattern: netplan alone has been observed
#     to leave the NIC with only a link-local IPv6 and no overlay IPv4, which
#     blackholes the smoke at baseline). A SMOKE_NET diagnostic line (interface +
#     `ip -br addr show`) is emitted to the serial console so a future failure
#     shows the real interface state;
#   - writes a long-lived probe script that TCP-probes PEER_IP:22 over the overlay
#     once a second and, on a successful connect, writes "OVERLAY_PING_OK <n>" to
#     the arch-correct serial console (ttyS0 on amd64, ttyAMA0 on arm64) with a
#     monotonically incrementing <n>; on a failed connect it writes a distinct
#     "OVERLAY_PROBE_FAIL <self>-><peer>:22" line so failures are visible. The
#     agent captures that console into serial.log. The probe is ONE long-lived
#     process launched DETACHED (setsid + nohup) so it survives cloud-init exiting
#     AND a LIVE migration: the counter CONTINUES on the target after the cutover
#     (the continuity this smoke keys on). The minimal cloudimg ships no ping/nc,
#     hence bash/sh /dev/tcp.
# Quoted heredoc -> the @@SELF@@/@@PEER@@ placeholders are substituted by sed; the
# guest's own shell vars ($SC, $IF, $n) stay literal.
gen_userdata() {
  local self="$1" peer="$2"
  sed -e "s|@@SELF@@|${self}|g" -e "s|@@PEER@@|${peer}|g" <<'EOF'
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
  - path: /usr/local/bin/otherix-overlay-probe.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      SC=/dev/ttyS0
      [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0
      n=0
      while :; do
        if timeout 3 bash -c "echo > /dev/tcp/@@PEER@@/22" 2>/dev/null; then
          n=$((n+1))
          echo "OVERLAY_PING_OK $n @@SELF@@->@@PEER@@" > "$SC"
        else
          echo "OVERLAY_PROBE_FAIL @@SELF@@->@@PEER@@:22" > "$SC"
        fi
        sleep 1
      done
runcmd:
  - netplan apply || true
  - sleep 2
  - |
    SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0
    IF=$(ls /sys/class/net | grep -E '^en' | head -1)
    ip addr replace @@SELF@@/24 dev "$IF" 2>/dev/null || true
    ip link set "$IF" up 2>/dev/null || true
    sleep 2
    { echo "SMOKE_NET if=$IF serial=$SC"; ip -br addr show; } > "$SC" 2>&1 || true
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-overlay-probe.sh >/dev/null 2>&1 < /dev/null &" ]
EOF
}

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-live-overlay smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"

for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

# default pool reconciled on BOTH nodes (CP auto-provision); the VMs resolve to it.
deadline=$(( SECONDS + 60 )); pool_ok=0
while (( SECONDS < deadline )); do pool_ready_both && { pool_ok=1; break; }; sleep 3; done
(( pool_ok == 1 )) || fail "default pool not ready on both nodes within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 + $NODE2 ready; default pool ready on both"

# Per-node exec handles + state roots for the serial probe. The dev stack maps
# node-1 -> handle/state index 1 and node-2 -> index 2.
H1="$SMOKE_HANDLE_1"; S1="$SMOKE_STATE_1"
H2="$SMOKE_HANDLE_2"; S2="$SMOKE_STATE_2"

# --- step 1: overlay network ------------------------------------------
echo "=== step 1: create overlay network $NET ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers
otx network create "$NET" --type overlay --subnet "$SUBNET" \
  || fail "network create (overlay) failed"
VNI="$(otx network get "$NET" --output json | jq -r '.vni')"
[[ "$VNI" =~ ^[0-9]+$ ]] || fail "no vni on $NET after create"
pass "overlay $NET created (vni=$VNI subnet=$SUBNET)"

# Gate on overlay materialisation before placing VMs: the network-aware scheduler
# refuses to place a VM on a node where the overlay has not reconciled to ready.
info "waiting for $NET to reconcile ready on both nodes (<= ${NET_READY_WAIT}s)"
deadline=$(( SECONDS + NET_READY_WAIT )); net_ok=0
while (( SECONDS < deadline )); do net_ready_both "$NET" && { net_ok=1; break; }; sleep 3; done
(( net_ok == 1 )) || { otx network get "$NET" --output json | jq -c '.status.nodes'; fail "$NET did not reconcile ready on both nodes within ${NET_READY_WAIT}s"; }
pass "$NET reconciled ready on both nodes"

# --- step 2: VM-B on node-2 (the stationary peer) ----------------------
echo "=== step 2: create $VMB on $NODE2 ($IPB) -> running ==="
# VM-B probes VM-A (@IPA) once it exists; create B first so A's probe finds a peer.
gen_userdata "$IPB" "$IPA" | otx vm create "$VMB" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE2" --network "$NET" \
  --vcpus 2 --memory-mb 2048 --disk-gib 10 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VMB did not reach running within ${CREATE_WAIT}s"
assert_phase "$VMB" running
VMID_B="$(otx vm get "$VMB" --output json | jq -r '.id')"
[[ "$VMID_B" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VMB id (got '$VMID_B')"
[[ "$(vm_node "$VMB")" == "$NODE2" ]] || fail "$VMB did not land on $NODE2"
info "$VMB id=$VMID_B on $NODE2"
pass "$VMB created and running on $NODE2"

# --- step 3: VM-A on node-1 (the migrant) ------------------------------
echo "=== step 3: create $VMA on $NODE1 ($IPA) -> running ==="
gen_userdata "$IPA" "$IPB" | otx vm create "$VMA" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --vcpus 2 --memory-mb 2048 --disk-gib 10 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VMA did not reach running within ${CREATE_WAIT}s"
assert_phase "$VMA" running
VMID_A="$(otx vm get "$VMA" --output json | jq -r '.id')"
[[ "$VMID_A" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VMA id (got '$VMID_A')"
[[ "$(vm_node "$VMA")" == "$NODE1" ]] || fail "$VMA did not land on $NODE1 (the migration source)"
info "$VMA id=$VMID_A on $NODE1"
pass "$VMA created and running on $NODE1"

# --- step 4: both directions reachable before the move -----------------
echo "=== step 4: overlay reachable both ways before the migrate ==="
# Each VM's OVERLAY_PING_OK appears only after a successful TCP connect to its peer
# over the overlay. Wait for both, then record VM-A's pre-cutover counter (N_a) -
# the value the live cutover must CONTINUE advancing past on the target.
wait_ping "$H1" "$S1" "$VMID_A" >/dev/null     # A -> B established
wait_ping "$H2" "$S2" "$VMID_B" >/dev/null     # B -> A established
N_A="$(ping_counter "$H1" "$S1" "$VMID_A")"
N_B="$(ping_counter "$H2" "$S2" "$VMID_B")"
[[ "$N_A" =~ ^[0-9]+$ && "$N_B" =~ ^[0-9]+$ ]] || fail "could not read pre-cutover counters (A='$N_A' B='$N_B')"
info "pre-cutover counters: A->B N_a=$N_A  B->A N_b=$N_B"
pass "overlay reachable both ways before the migrate (A->B and B->A)"

# --- step 5: live migrate VM-A to node-2 -------------------------------
echo "=== step 5: migrate $VMA (live) $NODE1 -> $NODE2 ==="
# No --offline: live is the default. A live cutover migrates the RUNNING qemu
# process to the target without a guest reboot; the overlay data plane must follow.
otx vm migrate "$VMA" --node "$NODE2" \
  --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "vm migrate (live) did not complete within ${MIGRATE_WAIT}s"
assert_node "$VMA" "$NODE2"
assert_phase "$VMA" running
pass "$VMA migrated: current node changed to $NODE2, phase running"

# --- step 6: migration record reached completed (live=true) ------------
echo "=== step 6: migration record -> completed (live) ==="
MIGID="$(otx migration list --vm "$VMID_A" --output json 2>/dev/null | jq -r '.data[0].id // ""')"
[[ "$MIGID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve migration id for $VMA (got '${MIGID:-none}')"
info "migration id=$MIGID"
deadline=$(( SECONDS + MIG_PHASE_WAIT )); mphase=""
while (( SECONDS < deadline )); do
  mphase="$(otx migration get "$MIGID" --output json 2>/dev/null | jq -r '.phase // ""' || true)"
  [[ "$mphase" == "completed" ]] && break
  case "$mphase" in failed|cancelled) fail "migration $MIGID reached terminal '$mphase', expected completed" ;; esac
  sleep 2
done
[[ "$mphase" == "completed" ]] || fail "migration $MIGID phase: want 'completed' got '${mphase:-none}' after ${MIG_PHASE_WAIT}s"
mlive="$(otx migration get "$MIGID" --output json 2>/dev/null | jq -r '.live // empty' || true)"
[[ "$mlive" == "true" ]] || fail "migration $MIGID live: want 'true' got '${mlive:-none}' (this is the LIVE smoke)"
pass "migration $MIGID phase=completed live=true"

# --- step 7: THE TEETH - overlay data plane followed the guest ---------
echo "=== step 7: overlay connectivity continues across the cutover (no blackhole) ==="
# FORWARD (A -> B): VM-A's id is stable across the move, so its post-cutover
# serial.log now lives on the TARGET node (node-2). Re-baseline to whatever the
# target log currently shows (the append-only target stream may start fresh at the
# cutover) and assert the counter keeps climbing - A still reaches B after moving.
A_BASE="$(ping_counter "$H2" "$S2" "$VMID_A")"
A_BASE="${A_BASE:-0}"
info "forward A->B: target ($NODE2) baseline counter=$A_BASE"
assert_advancing "forward A->B" "$H2" "$S2" "$VMID_A" "$A_BASE"

# REVERSE (B -> A): VM-B never moved; its counter must keep climbing past its
# pre-cutover value, proving B still reaches A at its NEW location (GARP/FDB
# repoint). We baseline at N_B (B's pre-cutover counter on node-2).
info "reverse B->A: node-2 baseline counter=$N_B"
assert_advancing "reverse B->A" "$H2" "$S2" "$VMID_B" "$N_B"
pass "overlay data plane followed the guest both ways (no prolonged blackhole at cutover)"

# --- step 8: teardown --------------------------------------------------
echo "=== step 8: teardown VMs + overlay ==="
otx vm delete "$VMA" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VMB" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
pass "VMs + overlay deleted"

trap - EXIT
echo
echo "${GREEN}=== vm-migration-live-overlay smoke PASSED ===${NC}"
