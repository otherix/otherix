#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Artifact-janitor smoke: the two background cleaners that keep a node's
# content-addressed blob store healthy without ever destroying referenced data,
# proven end to end against REAL agents on a three-node stack.
#
# Two independent janitors are exercised:
#
#   1. AGENT-LOCAL HYGIENE SWEEP (node-local, no cluster decisions). On agent
#      start (and hourly thereafter) the agent reconciles its blob store against
#      itself: it clears interrupted-upload temp files left under
#      blobs/.staging, and repairs a blob whose checksum sidecar is missing
#      (re-hashing the bytes; a content match rewrites the lost .sha256 sidecar
#      and KEEPS the blob). This smoke plants a stale staging temp and removes a
#      live blob's sidecar on a replica holder, then triggers the boot-time sweep
#      by restarting the agents (an operator restart triggers the store
#      reconcile). It asserts the staging temp is gone, the sidecar reappeared
#      (repaired), and the blob itself survived.
#
#   2. LEAKED-BLOB BACKSTOP (CP-side durability reconcile). The reconcile pass now
#      also walks the digests observed across nodes' blob inventories, not only
#      the placement map, so a blob a node still holds but whose placement record
#      was lost is revisited and, when it has zero references, collected through
#      the existing gated reclaim. This smoke deletes a snapshot (dropping its
#      blob's last reference) AND deletes that blob's placement keys directly in
#      the dev store - modelling a residual leak: bytes on disk, no placement
#      record, no reference. It asserts the backstop reclaims the leaked blob from
#      every holder, while a second, still-referenced blob is never touched.
#
# The blob lives in the agent's content-addressed store at
# <state_root>/artifacts/blobs/<digest> (the .staging temp dir and the
# <digest>.sha256 sidecar sit alongside). We read a snapshot's disk[0] digest
# from the CP projection and probe each node's blob store directly.
#
# The serial device is /dev/console (the active serial console on the cloud
# image, arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM
# smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled. jq +
# etcdctl on PATH (the embedded etcd dev member listens on 127.0.0.1:2379, no
# TLS).
#
# Usage: make smoke-artifact-janitor   (or: bash dev/smoke/artifact-janitor/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # producer node (create + snapshot pinned here)
SRC_VM="${SRC_VM:-jan-src}"                    # source VM on node-1; snapshotted A/B
POOL="${POOL:-jan-$(date +%s)}"               # this smoke's OWN artifact pool, K=2
SNAP_A="${SNAP_A:-a1}"                         # referenced snapshot; its blob X is leaked then collected
SNAP_B="${SNAP_B:-b1}"                         # second snapshot; its blob Y stays referenced throughout
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"                 # seconds for a snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
DURABLE_WAIT="${DURABLE_WAIT:-180}"           # seconds for a 2nd replica placement (reconcile ~10s)
READY_WAIT="${READY_WAIT:-120}"               # seconds for nodes to return to ready after the restart
# The leaked-blob reclaim must land after the references AND the placement keys
# are gone: a reconcile pass walks the observed digest, finds zero refs, enqueues
# the reclaim, the agent deletes the blob, the next heartbeat clears the holder.
# Give it several intervals plus task latency.
RECLAIM_WAIT="${RECLAIM_WAIT:-240}"           # seconds for X to vanish from BOTH holders after the leak
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"          # embedded etcd dev client endpoint (no TLS)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# etcd_keys PREFIX -> keys under PREFIX, one per line ("" when none)
etcd_keys() { ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" get "$1" --prefix --keys-only 2>/dev/null | sed '/^$/d' || true; }
# etcd_del_prefix PREFIX -> delete every key under PREFIX
etcd_del_prefix() { ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" del "$1" --prefix >/dev/null 2>&1 || true; }

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

# sidecar_present IDX DIGEST -> 0 if node IDX holds the blob's .sha256 sidecar
sidecar_present() {
  local idx="$1" digest="$2"
  run_on "$(smoke_handle "$idx")" sudo test -f "$(smoke_state "$idx")/artifacts/blobs/$digest.sha256" 2>/dev/null
}

# staging_dir IDX -> the node's interrupted-upload staging directory
staging_dir() { printf '%s' "$(smoke_state "$1")/artifacts/blobs/.staging"; }

# --- the cloud-config (always-on serial echo so the source disk has boot state) ---
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot, so the disk has post-boot state
# before we snapshot it.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-jan-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix artifact-janitor-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_JAN_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-jan-sentinel.service ]
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
  otx vm delete "$SRC_VM" --wait --force >/dev/null 2>&1 || true
  local n sid
  for n in "$SNAP_A" "$SNAP_B"; do
    sid="$(snapshot_id_of "$n" || true)"
    [ -n "$sid" ] && otx snapshot delete "$sid" --wait >/dev/null 2>&1 || true
  done
  otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== artifact-janitor smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (it pokes the embedded etcd dev member to model the lost placement record)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
ETCDCTL_API=3 etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 \
  || fail "etcd not reachable at $ETCD_EP (the dev CP embeds it on 2379; run make local-dev-start)"
CP_VERSION="$(cp_version)"
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
pass "CP up (${CP_VERSION}); node-1/node-2/node-3 ready; default disk pool ready on $NODE1; etcd reachable"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: pool + source VM + snapshot A -> durable (blob X on 2 nodes) ---
echo "=== step 1: artifact-pool $POOL (K=2), $SRC_VM on $NODE1, snapshot $SNAP_A -> durable ==="
otx artifact-pool create "$POOL" --replication-factor 2 --members all \
  || fail "artifact-pool create $POOL failed"
pass "artifact-pool $POOL created (K=2)"

printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
info "$SRC_VM id=$SRC_ID"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$SRC_ID" "OTHERIX_JAN_BOOT ${SRC_VM}" "$GUEST_WAIT" \
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

# Identify the replica holder of X (node-2 or node-3, besides the producer node-1).
REPLICA_IDX=""
deadline=$(( SECONDS + 60 ))
while (( SECONDS < deadline )) && [[ -z "$REPLICA_IDX" ]]; do
  for i in 2 3; do blob_present "$i" "$X" && { REPLICA_IDX="$i"; break; }; done
  [[ -z "$REPLICA_IDX" ]] && sleep 3
done
[[ -n "$REPLICA_IDX" ]] || fail "no replica holder found among node-2/node-3 for digest X=$X"
blob_present 1 "$X" || fail "producer $NODE1 unexpectedly does not hold blob X"
pass "snapshot $SNAP_A durable: blob X on $NODE1 (producer) and node-$REPLICA_IDX (replica)"

# --- step 2: agent hygiene sweep (clear staging + repair sidecar) ------
echo "=== step 2: plant store damage on node-$REPLICA_IDX, restart agents -> boot sweep repairs it ==="
REPLICA_H="$(smoke_handle "$REPLICA_IDX")"
STAGE_DIR="$(staging_dir "$REPLICA_IDX")"
STAGE_FILE="${STAGE_DIR}/interrupted-upload-smoke"

# Plant a stale interrupted-upload temp under the node's staging dir. The boot
# sweep clears ALL staging regardless of age, but back-date it anyway so the
# model matches a real abandoned upload.
run_on "$REPLICA_H" sudo install -d -m 0750 "$STAGE_DIR" \
  || fail "could not ensure staging dir $STAGE_DIR on node-$REPLICA_IDX"
run_on "$REPLICA_H" sudo sh -c "printf 'partial-blob-bytes' > '$STAGE_FILE'" \
  || fail "could not plant staging temp $STAGE_FILE on node-$REPLICA_IDX"
run_on "$REPLICA_H" sudo touch -d '-2 hours' "$STAGE_FILE" || true
run_on "$REPLICA_H" sudo test -f "$STAGE_FILE" \
  || fail "planted staging temp $STAGE_FILE not present after write on node-$REPLICA_IDX"
info "planted stale staging temp $STAGE_FILE on node-$REPLICA_IDX"

# Remove blob X's sidecar on the replica (the blob bytes stay): models a sidecar
# write interrupted after the atomic blob rename but before the .sha256 landed.
sidecar_present "$REPLICA_IDX" "$X" \
  || fail "blob X sidecar unexpectedly already absent on node-$REPLICA_IDX before the damage step"
run_on "$REPLICA_H" sudo rm -f "$(smoke_state "$REPLICA_IDX")/artifacts/blobs/$X.sha256" \
  || fail "could not remove blob X sidecar on node-$REPLICA_IDX"
sidecar_present "$REPLICA_IDX" "$X" \
  && fail "blob X sidecar still present on node-$REPLICA_IDX after rm (damage not applied)"
blob_present "$REPLICA_IDX" "$X" \
  || fail "blob X bytes vanished when removing only its sidecar on node-$REPLICA_IDX"
info "removed blob X sidecar on node-$REPLICA_IDX (bytes intact, sidecar missing)"

# Trigger the boot-time store reconcile: an operator restart of the agents runs
# the sweep immediately on start (the periodic interval is hourly). Bounce api +
# agents in place; state is preserved.
info "restarting api + agents (operator restart triggers the boot-time store reconcile)"
make local-dev-restart || fail "make local-dev-restart failed"

# Re-wait for all three nodes ready and the default pool ready before asserting.
for n in node-1 node-2 node-3; do
  deadline=$(( SECONDS + READY_WAIT )); rdy=0
  while (( SECONDS < deadline )); do
    [[ "$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)" == "ready" ]] && { rdy=1; break; }
    sleep 3
  done
  (( rdy == 1 )) || fail "$n did not return to ready within ${READY_WAIT}s after the restart"
done
deadline=$(( SECONDS + READY_WAIT )); ok=0
while (( SECONDS < deadline )); do pool_ready "$NODE1" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default disk pool not ready on $NODE1 within ${READY_WAIT}s after the restart"
pass "node-1/node-2/node-3 ready again after the restart"

# Give the boot sweep a moment to run after the agent comes up, then assert.
deadline=$(( SECONDS + 60 )); swept=0
while (( SECONDS < deadline )); do
  if ! run_on "$REPLICA_H" sudo test -f "$STAGE_FILE" 2>/dev/null && sidecar_present "$REPLICA_IDX" "$X"; then
    swept=1; break
  fi
  sleep 3
done
(( swept == 1 )) || {
  st=$(run_on "$REPLICA_H" sudo test -f "$STAGE_FILE" 2>/dev/null && echo present || echo gone)
  sc=$(sidecar_present "$REPLICA_IDX" "$X" && echo present || echo missing)
  fail "boot sweep did not clean node-$REPLICA_IDX (staging temp=$st sidecar=$sc) - hygiene sweep ineffective"
}
run_on "$REPLICA_H" sudo test -f "$STAGE_FILE" 2>/dev/null \
  && fail "staging temp $STAGE_FILE survived the boot sweep on node-$REPLICA_IDX"
sidecar_present "$REPLICA_IDX" "$X" \
  || fail "blob X sidecar NOT repaired on node-$REPLICA_IDX after the boot sweep"
blob_present "$REPLICA_IDX" "$X" \
  || fail "boot sweep DELETED valid blob X on node-$REPLICA_IDX (it should have repaired the sidecar, not removed the blob)"
pass "hygiene sweep: staging temp cleared, blob X sidecar repaired, blob X bytes survived on node-$REPLICA_IDX"

# --- step 3: leaked-blob backstop -> X reclaimed, referenced Y untouched ---
echo "=== step 3: snapshot $SNAP_B (referenced blob Y), then leak X and assert the backstop collects only X ==="

# Snapshot B gives us a second, still-referenced blob Y that the backstop must
# never touch while it collects the leaked X.
otx vm snapshot "$SRC_VM" --name "$SNAP_B" --artifact-pool "$POOL" \
  --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot $SNAP_B --artifact-pool $POOL did not finish within ${SNAP_WAIT}s"
B_ID="$(snapshot_id_of "$SNAP_B")"
[[ "$B_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve snapshot id for '$SNAP_B' (got '$B_ID')"
[[ "$(snap_field "$B_ID" '.status')" == "ready" ]] || fail "snapshot $SNAP_B not ready"
Y="$(snap_field "$B_ID" '.disks[0].sha256')"
[[ "$Y" =~ ^[0-9a-f]{64}$ ]] || fail "could not resolve disk[0] sha256 for snapshot $B_ID (got '$Y')"
[[ "$Y" != "$X" ]] || fail "snapshot $SNAP_B reuses blob X ($Y); the smoke needs two distinct blobs"
info "snapshot $SNAP_B id=$B_ID disk[0] digest Y=$Y"
wait_durable "$B_ID" "$DURABLE_WAIT"
# Record Y's holders so we can assert it stays put throughout.
Y_REPLICA_IDX=""
for i in 2 3; do blob_present "$i" "$Y" && { Y_REPLICA_IDX="$i"; break; }; done
[[ -n "$Y_REPLICA_IDX" ]] || fail "no replica holder found for referenced blob Y=$Y"
blob_present 1 "$Y" || fail "producer $NODE1 unexpectedly does not hold referenced blob Y"
pass "snapshot $SNAP_B durable: referenced blob Y on $NODE1 and node-$Y_REPLICA_IDX"

# Drop X's only reference: delete snapshot A. X now has zero references, but the
# orphaned-GC path collects it through the placement map. To model a LOST
# placement record (the residual leak the observed-digest backstop closes) we
# also delete X's placement keys directly in the dev store - leaving X's BYTES on
# both nodes with no placement entry and no reference.
otx snapshot delete "$A_ID" --wait --wait-timeout "${OP_WAIT}s" \
  || fail "snapshot delete $SNAP_A did not finish within ${OP_WAIT}s"
otx snapshot get "$A_ID" --output json >/dev/null 2>&1 && fail "$SNAP_A still visible after delete"
info "snapshot $SNAP_A deleted (blob X now unreferenced)"

# Wait for X's placement keys to settle (the orphan-GC pass may already have begun
# trimming them); then delete the whole prefix to model the lost record. The blob
# bytes are independent of these keys, so this is purely the placement-record loss.
etcd_del_prefix "/otherix/placement/$X/"
remaining="$(etcd_keys "/otherix/placement/$X/")"
[[ -z "$remaining" ]] || fail "X placement keys still present after del --prefix: $remaining"
pass "blob X placement keys deleted in the dev store (bytes on disk, no placement record, no reference)"

# The backstop reconcile walks observed node-blob digests, sees X with zero refs,
# and collects it from every holder via the gated reclaim. Poll until X is gone
# from BOTH holders. Throughout, the referenced Y must stay present everywhere.
info "polling for the backstop to reclaim leaked blob X from BOTH holders (<= ${RECLAIM_WAIT}s)"
deadline=$(( SECONDS + RECLAIM_WAIT )); reclaimed=0
while (( SECONDS < deadline )); do
  blob_present 1 "$Y"              || fail "BACKSTOP VIOLATION: referenced blob Y vanished from $NODE1"
  blob_present "$Y_REPLICA_IDX" "$Y" || fail "BACKSTOP VIOLATION: referenced blob Y vanished from node-$Y_REPLICA_IDX"
  if ! blob_present 1 "$X" && ! blob_present "$REPLICA_IDX" "$X"; then
    reclaimed=1; break
  fi
  sleep 5
done
(( reclaimed == 1 )) || {
  p1=$(blob_present 1 "$X" && echo present || echo gone)
  pr=$(blob_present "$REPLICA_IDX" "$X" && echo present || echo gone)
  fail "leaked blob X NOT reclaimed within ${RECLAIM_WAIT}s ($NODE1=$p1 node-$REPLICA_IDX=$pr); the backstop left an orphaned blob behind"
}
# Final confirmation: X gone from every node, Y still on its holders.
blob_present 1 "$X" && fail "leaked blob X still on $NODE1 after the reclaim window"
blob_present "$REPLICA_IDX" "$X" && fail "leaked blob X still on node-$REPLICA_IDX after the reclaim window"
blob_present 1 "$Y" || fail "referenced blob Y missing from $NODE1 at the end (backstop over-collected)"
blob_present "$Y_REPLICA_IDX" "$Y" || fail "referenced blob Y missing from node-$Y_REPLICA_IDX at the end (backstop over-collected)"
pass "backstop: leaked blob X reclaimed from BOTH holders; referenced blob Y untouched throughout"

# --- step 4: cleanup ---------------------------------------------------
echo "=== step 4: cleanup ==="
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx snapshot delete "$B_ID" --wait --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== artifact-janitor smoke PASSED ===${NC}"
echo "  hygiene sweep: planted staging temp + removed blob X sidecar on node-$REPLICA_IDX;"
echo "    an operator restart triggered the boot sweep -> staging cleared, sidecar repaired, blob bytes survived"
echo "  backstop: blob X (digest=$X) leaked (last ref deleted + placement keys removed in the store);"
echo "    the observed-digest reconcile reclaimed X from both holders while referenced blob Y (digest=$Y) stayed put"
echo "PASS"
