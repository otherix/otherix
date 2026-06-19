#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Artifact garbage-collection smoke: the reference-counted blob collector
# reclaims unneeded copies of a content-addressed blob and NEVER reclaims a copy
# a snapshot still references - driven through the otherix CLI as an operator
# would. This is the safety gate for the collector: a referenced blob must
# survive forever; only when its last reference is gone may its copies be
# reclaimed.
#
# The proof tracks ONE blob through its whole reference lifetime:
#   1. create an artifact pool with replication factor 2 (--members all)
#   2. vm create on node-1, lay down a write-once guest marker, snapshot A into
#      the pool; poll A -> "durable" (blob X on 2 nodes). Capture X = disk[0]
#      digest.
#   3. REFERENCED-BLOB-NEVER-RECLAIMED (the gate): while snapshot A still
#      references X, hold across several reconcile intervals and assert X stays on
#      BOTH holder nodes and A stays "durable". The collector runs every interval;
#      a referenced blob must survive every pass. Then recreate a VM from A and
#      boot it to prove X is intact and usable.
#   4. ORPHANED GC: delete snapshot A. X now has zero references. Poll until X is
#      gone from BOTH holder nodes' blob stores AND no longer appears in any
#      snapshot's holder data. The before/after on the SAME blob - survives while
#      referenced, reclaimed once its last reference is gone - is reference
#      counting in action.
#   5. OVER-REPLICATION GC: snapshot C from the VM, let it reach durable on 2
#      nodes, then lower the pool replication factor to 1. Poll until exactly 1
#      live holder of C's blob remains (the surplus copy is reclaimed, never the
#      last one).
#   6. recreate-from-snapshot of the still-referenced C boots (no false reclaim of
#      a referenced blob).
#
# The blob lives in the agent's content-addressed store at
# <state_root>/artifacts/blobs/<digest>. We read the snapshot's disk[0] digest
# from the CP projection and probe each node's blob store directly, plus the CP's
# snapshot holder_nodes projection, to confirm what is present and what is gone.
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-artifact-gc   (or: bash dev/smoke/artifact-gc/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # producer node (create + snapshot pinned here)
SRC_VM="${SRC_VM:-gc-src}"                    # source VM on node-1; snapshotted A/C
DST_VM="${DST_VM:-gc-dst}"                    # VM recreated from a referenced snapshot
POOL="${POOL:-gc-$(date +%s)}"               # this smoke's OWN artifact pool, K=2
SNAP_A="${SNAP_A:-a1}"                         # referenced snapshot (held, then deleted for the orphan step)
SNAP_C="${SNAP_C:-c1}"                         # snapshot for the over-replication step
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
RECREATE_WAIT="${RECREATE_WAIT:-600}"         # seconds for vm create --from-snapshot -> running
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"                 # seconds for a snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
DURABLE_WAIT="${DURABLE_WAIT:-180}"           # seconds for a 2nd replica placement (reconcile ~10s)
# The referenced-blob hold window must span SEVERAL reconcile intervals (dev tick
# ~10s) so a buggy collector would have fired by the time we declare the blob safe.
HOLD_WINDOW="${HOLD_WINDOW:-60}"              # seconds to hold-and-verify X survives while referenced
# The orphaned reclaim must land after the references drop: a reconcile pass
# enqueues the reclaim task, the agent deletes the blob, and the next heartbeat
# clears the holder. Give it several intervals plus task latency.
RECLAIM_WAIT="${RECLAIM_WAIT:-240}"           # seconds for X to vanish from BOTH nodes + projection
# Over-replication trim: lowering K enqueues a reclaim of the surplus holder; the
# blob must converge to exactly 1 live holder.
TRIM_WAIT="${TRIM_WAIT:-240}"                 # seconds for the surplus copy to be reclaimed -> 1 holder

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME -> prints the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

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

# serial_count HANDLE STATE VMID PATTERN -> count of lines matching PATTERN in the
# VM's serial.log on the node identified by HANDLE/STATE (0 when the file is absent).
serial_count() {
  local handle="$1" state="$2" id="$3" pat="$4" n
  n="$(run_on "$handle" sudo grep -c "$pat" "${state}/vms/${id}/serial.log" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_serial HANDLE STATE VMID PATTERN TIMEOUT -> block until PATTERN appears in
# the VM's serial.log on HANDLE/STATE; returns non-zero on timeout.
wait_serial() {
  local handle="$1" state="$2" id="$3" pat="$4" to="$5" deadline; deadline=$(( SECONDS + to ))
  info "watching ${state}/vms/${id}/serial.log on $handle for '$pat' (<= ${to}s)"
  while (( SECONDS < deadline )); do
    (( $(serial_count "$handle" "$state" "$id" "$pat") > 0 )) && return 0
    sleep 5
  done
  return 1
}

# snap_field SNAP-ID JQ -> prints a field of the snapshot projection ("" on any error)
snap_field() { otx snapshot get "$1" --output json 2>/dev/null | jq -r "$2" 2>/dev/null || true; }

# wait_durable SNAP-ID TIMEOUT -> wait until durability==durable && observed_replicas>=2
wait_durable() {
  local id="$1" to="$2" deadline d o
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    d="$(snap_field "$id" '.durability')"; o="$(snap_field "$id" '.observed_replicas')"
    [[ "$d" == "durable" && "${o:-0}" -ge 2 ]] && { pass "durable (observed=$o)"; return 0; }
    sleep 3
  done
  fail "durability never reached durable within ${to}s (last durability=$d observed=$o)"
}

# blob_present IDX DIGEST -> 0 if node IDX physically holds the blob, 1 otherwise
blob_present() {
  local idx="$1" digest="$2"
  run_on "$(smoke_handle "$idx")" sudo test -f "$(smoke_state "$idx")/artifacts/blobs/$digest" 2>/dev/null
}

# replica_holders DIGEST -> number of node-2/node-3 nodes physically holding the
# blob (node-1 is the producer; this counts the replica copies placed by the
# reconcile loop).
replica_holders() {
  local digest="$1" n=0 i
  for i in 2 3; do blob_present "$i" "$digest" && n=$(( n + 1 )); done
  printf '%s' "$n"
}

# total_holders DIGEST -> number of nodes (across all three) physically holding
# the blob. The collector keeps the rendezvous-preferred holder, which is not
# necessarily the producer, so the over-replication assertion counts the total
# rather than expecting any specific node to survive.
total_holders() {
  local digest="$1" n=0 i
  for i in 1 2 3; do blob_present "$i" "$digest" && n=$(( n + 1 )); done
  printf '%s' "$n"
}

# any_snapshot_lists_holder DIGEST -> 0 if ANY snapshot of SRC_ID still lists a
# node holding the digest in its CP projection holder_nodes; 1 otherwise. Confirms
# the orphaned blob left the CP's observed holder data too, not just disk.
any_snapshot_lists_holder() {
  local digest="$1" sid d
  for sid in $(otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null | jq -r '.data[]?.id' 2>/dev/null); do
    d="$(snap_field "$sid" '.disks[0].sha256')"
    [[ "$d" == "$digest" ]] || continue
    local cnt; cnt="$(snap_field "$sid" '(.holder_nodes // []) | length')"
    [[ "${cnt:-0}" -gt 0 ]] && return 0
  done
  return 1
}

# --- the cloud-config (always-on serial echo so the source disk has boot state) ---
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot. The source step waits for the marker
# so the disk has post-boot state before we snapshot it; the recreate step asserts
# the restored guest reaches the marker, i.e. it actually boots off the blob.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-gc-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix artifact-gc-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_GC_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-gc-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- bookkeeping for cleanup ------------------------------------------
SRC_ID=""

# snapshot_id_of NAME -> prints the snapshot uuid for NAME on $SRC_ID ("" if absent)
snapshot_id_of() {
  [ -n "${SRC_ID:-}" ] || return 0
  otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null \
    | jq -r --arg n "$1" 'first(.data[]? | select(.name==$n)) | .id // empty' 2>/dev/null || true
}

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$DST_VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$SRC_VM" --wait --force >/dev/null 2>&1 || true
  local n sid
  for n in "$SNAP_A" "$SNAP_C"; do
    sid="$(snapshot_id_of "$n" || true)"
    [ -n "$sid" ] && otx snapshot delete "$sid" --wait >/dev/null 2>&1 || true
  done
  otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== artifact-gc smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"

# All three nodes ready: node-1 produces, node-2/node-3 host the replica copies.
for n in node-1 node-2 node-3; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start for a three-node stack"
done

# default DISK pool reconciled on node-1 (the source VM resolves to it).
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready "$NODE1" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default disk pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); node-1/node-2/node-3 ready; default disk pool ready on $NODE1"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: create the artifact pool with replication factor 2 --------
echo "=== step 1: artifact-pool create $POOL --replication-factor 2 --members all ==="
otx artifact-pool create "$POOL" --replication-factor 2 --members all \
  || fail "artifact-pool create $POOL failed"
otx artifact-pool get "$POOL" --output json >/dev/null \
  || fail "artifact-pool get $POOL failed after create"
pass "artifact-pool $POOL created (K=2)"

# --- step 2: source VM on node-1 -> running, snapshot A -> durable -----
echo "=== step 2: create $SRC_VM on $NODE1 -> running, snapshot $SNAP_A -> durable ==="
printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
info "$SRC_VM id=$SRC_ID"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$SRC_ID" "OTHERIX_GC_BOOT ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $SRC_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "${SMOKE_STATE_1}/vms/${SRC_ID}/serial.log" 2>/dev/null || true; \
       fail "$SRC_VM did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$SRC_VM created, running, and booted on $NODE1"

otx vm snapshot "$SRC_VM" --name "$SNAP_A" --artifact-pool "$POOL" \
  --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot $SNAP_A --artifact-pool $POOL did not finish within ${SNAP_WAIT}s"
A_ID="$(snapshot_id_of "$SNAP_A")"
[[ "$A_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve snapshot id for '$SNAP_A' (got '$A_ID')"
[[ "$(snap_field "$A_ID" '.status')" == "ready" ]] || fail "snapshot $SNAP_A not ready"
X="$(snap_field "$A_ID" '.disks[0].sha256')"
[[ "$X" =~ ^[0-9a-f]{64}$ ]] || fail "could not resolve disk[0] sha256 for snapshot $A_ID (got '$X')"
info "snapshot $SNAP_A id=$A_ID disk[0] digest X=$X"
wait_durable "$A_ID" "$DURABLE_WAIT"
pass "snapshot $SNAP_A ready and durable (blob X on 2 nodes)"

# Identify the two live holders of X (producer node-1 + one replica node).
REPLICA_IDX=""
deadline=$(( SECONDS + 60 ))
while (( SECONDS < deadline )) && [[ -z "$REPLICA_IDX" ]]; do
  for i in 2 3; do blob_present "$i" "$X" && { REPLICA_IDX="$i"; break; }; done
  [[ -z "$REPLICA_IDX" ]] && sleep 3
done
[[ -n "$REPLICA_IDX" ]] || fail "no replica holder found among node-2/node-3 for digest X=$X"
blob_present 1 "$X" || fail "producer $NODE1 unexpectedly does not hold blob X"
pass "blob X confirmed physically present on $NODE1 (producer) and node-$REPLICA_IDX (replica)"

# --- step 3: referenced-blob-never-reclaimed (THE GATE) ----------------
echo "=== step 3: hold while $SNAP_A references X; the collector must never reclaim it ==="
info "holding for ${HOLD_WINDOW}s across several reconcile intervals while X has a reference"
deadline=$(( SECONDS + HOLD_WINDOW ))
while (( SECONDS < deadline )); do
  blob_present 1 "$X"              || fail "GATE VIOLATED: blob X disappeared from $NODE1 while $SNAP_A references it"
  blob_present "$REPLICA_IDX" "$X" || fail "GATE VIOLATED: blob X disappeared from node-$REPLICA_IDX while $SNAP_A references it"
  d="$(snap_field "$A_ID" '.durability')"
  [[ "$d" == "durable" ]]          || fail "GATE VIOLATED: $SNAP_A durability dropped to '$d' (a referenced blob was touched)"
  sleep 5
done
pass "blob X held on BOTH holders across ${HOLD_WINDOW}s and $SNAP_A stayed durable - a referenced blob is never reclaimed"

# recreate from the still-referenced A and boot it -> proves X is intact + usable.
echo "=== step 3b: recreate $DST_VM --from-snapshot $SNAP_A -> running (X still usable) ==="
otx vm create "$DST_VM" --from-snapshot "$A_ID" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 \
  --wait --wait-timeout "${RECREATE_WAIT}s" \
  || fail "vm create --from-snapshot $SNAP_A did not reach running within ${RECREATE_WAIT}s"
assert_phase "$DST_VM" running
DST_ID="$(otx vm get "$DST_VM" --output json | jq -r '.id')"
[[ "$DST_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $DST_VM id (got '$DST_ID')"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$DST_ID" "OTHERIX_GC_BOOT ${DST_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "${SMOKE_STATE_1}/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "$DST_VM did not boot from the still-referenced blob X within ${GUEST_WAIT}s"; }
pass "$DST_VM booted from the still-referenced blob X - referenced data intact"
# tear the recreate down so it does not linger into the orphan step.
otx vm delete "$DST_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true

# --- step 4: orphaned GC -> X reclaimed from every holder --------------
echo "=== step 4: delete $SNAP_A; X now has zero references -> reclaimed from BOTH holders ==="
otx snapshot delete "$A_ID" --wait --wait-timeout "${OP_WAIT}s" \
  || fail "snapshot delete $SNAP_A did not finish within ${OP_WAIT}s"
otx snapshot get "$A_ID" --output json >/dev/null 2>&1 && fail "$SNAP_A still visible after delete"
info "$SNAP_A deleted; polling for X to be reclaimed from BOTH holders and from the CP holder projection (<= ${RECLAIM_WAIT}s)"
deadline=$(( SECONDS + RECLAIM_WAIT )); reclaimed=0
while (( SECONDS < deadline )); do
  if ! blob_present 1 "$X" && ! blob_present "$REPLICA_IDX" "$X" && ! any_snapshot_lists_holder "$X"; then
    reclaimed=1; break
  fi
  sleep 5
done
(( reclaimed == 1 )) || {
  p1=$(blob_present 1 "$X" && echo present || echo gone)
  pr=$(blob_present "$REPLICA_IDX" "$X" && echo present || echo gone)
  fail "orphaned blob X NOT reclaimed within ${RECLAIM_WAIT}s ($NODE1=$p1 node-$REPLICA_IDX=$pr); the collector left an unreferenced blob behind"
}
pass "orphaned blob X reclaimed from BOTH holders and cleared from the CP holder projection (referenced -> survived, unreferenced -> collected)"

# --- step 5: over-replication GC -> surplus copy trimmed to 1 holder ---
echo "=== step 5: snapshot $SNAP_C -> durable on 2; lower pool RF to 1 -> trim to 1 holder ==="
otx vm snapshot "$SRC_VM" --name "$SNAP_C" --artifact-pool "$POOL" \
  --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot $SNAP_C --artifact-pool $POOL did not finish within ${SNAP_WAIT}s"
C_ID="$(snapshot_id_of "$SNAP_C")"
[[ "$C_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve snapshot id for '$SNAP_C' (got '$C_ID')"
[[ "$(snap_field "$C_ID" '.status')" == "ready" ]] || fail "snapshot $SNAP_C not ready"
Y="$(snap_field "$C_ID" '.disks[0].sha256')"
[[ "$Y" =~ ^[0-9a-f]{64}$ ]] || fail "could not resolve disk[0] sha256 for snapshot $C_ID (got '$Y')"
info "snapshot $SNAP_C id=$C_ID disk[0] digest Y=$Y"
wait_durable "$C_ID" "$DURABLE_WAIT"
[[ "$(replica_holders "$Y")" -ge 1 ]] || fail "blob Y has no replica holder before the RF lowering"
pass "snapshot $SNAP_C durable: blob Y on the producer + a replica (2 holders)"

info "lowering pool $POOL replication factor to 1 (the surplus copy becomes reclaimable)"
otx artifact-pool update "$POOL" --replication-factor 1 \
  || fail "artifact-pool update $POOL --replication-factor 1 failed"
[[ "$(otx artifact-pool get "$POOL" --output json | jq -r '.replication_factor')" == "1" ]] \
  || fail "pool $POOL replication_factor did not read back as 1 after update"

info "polling for blob Y to converge to exactly 1 live holder (<= ${TRIM_WAIT}s)"
deadline=$(( SECONDS + TRIM_WAIT )); trimmed=0
while (( SECONDS < deadline )); do
  th="$(total_holders "$Y")"
  (( th >= 1 )) || fail "OVER-REPL VIOLATION: blob Y reclaimed below 1 holder (count=$th); the collector dropped the last copy of a referenced blob"
  if (( th == 1 )); then trimmed=1; break; fi
  sleep 5
done
(( trimmed == 1 )) || fail "surplus copy of blob Y not reclaimed within ${TRIM_WAIT}s (holders still $(total_holders "$Y"); over-replication never converged to 1)"
(( $(total_holders "$Y") >= 1 )) || fail "blob Y lost its last holder after the trim - over-replication GC dropped below 1"
pass "over-replication trimmed: blob Y reclaimed down to exactly 1 live holder ($SNAP_C still referenced and held)"

# --- step 6: a still-referenced snapshot recreates and boots (no false reclaim) ---
echo "=== step 6: recreate $DST_VM --from-snapshot $SNAP_C -> running (referenced blob usable) ==="
otx vm create "$DST_VM" --from-snapshot "$C_ID" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 \
  --wait --wait-timeout "${RECREATE_WAIT}s" \
  || fail "vm create --from-snapshot $SNAP_C did not reach running within ${RECREATE_WAIT}s"
assert_phase "$DST_VM" running
DST_ID="$(otx vm get "$DST_VM" --output json | jq -r '.id')"
[[ "$DST_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $DST_VM id (got '$DST_ID')"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$DST_ID" "OTHERIX_GC_BOOT ${DST_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "${SMOKE_STATE_1}/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "$DST_VM did not boot from the still-referenced blob Y within ${GUEST_WAIT}s"; }
pass "$DST_VM booted from still-referenced $SNAP_C - the collector never reclaimed a referenced blob"

# --- step 7: cleanup ---------------------------------------------------
echo "=== step 7: cleanup ==="
otx vm delete "$DST_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx snapshot delete "$C_ID" --wait --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== artifact-gc smoke PASSED ===${NC}"
echo "  blob X (digest=$X) referenced by $SNAP_A"
echo "  held across reconcile intervals -> X stayed on both holders + $SNAP_A durable (referenced blob never reclaimed)"
echo "  $SNAP_A deleted -> X reclaimed from both holders + cleared from holder projection (orphan collected)"
echo "  pool RF 2 -> 1 -> blob Y trimmed to exactly 1 holder (over-replication collected, never below 1)"
echo "PASS"
