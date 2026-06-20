#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Blob-scrub smoke: the agent's periodic blob scrubber detects silent on-disk
# corruption (bit-rot) of a durability-tracked content-addressed blob, deletes
# the confirmed-corrupt copy, and the CP durability reconcile re-replicates a
# healthy copy back to the artifact pool's replication factor - driven through
# the otherix CLI as an operator would.
#
#   1. create an artifact pool with replication factor 2 (--members all)
#   2. vm create on node-1, lay down a write-once guest marker, snapshot into the
#      pool; poll the snapshot durability -> "durable" (blob D on 2 nodes)
#   3. find the 2nd holder node H (the non-node-1 node holding the blob) and
#      capture the digest D
#   4. TAMPER: append bytes to H's on-disk copy so it no longer hashes to D, while
#      LEAVING the <D>.sha256 sidecar in place (modelling bit-rot the Put-time
#      verify cannot catch)
#   5. wait for a scrub pass to re-hash, fail verification, and DELETE the corrupt
#      copy: the blob vanishes from H's blob store (the durable assertion) and the
#      agent journal logs `blob scrub: artifact store` with corrupt_deleted > 0
#      (corroboration)
#   6. assert durability transiently leaves "durable" then re-heals back to
#      "durable" with 2 holders (the reconcile re-replicated a healthy copy)
#   7. assert the OTHER, untouched holder still physically holds D the whole time
#      (the scrubber deleted ONLY the corrupt copy, never the healthy one)
#
# The blob lives in the agent's content-addressed store at
# <state_root>/artifacts/blobs/<digest> with a <digest>.sha256 sidecar. We read
# the snapshot's disk[0] digest from the CP projection and probe each candidate
# node's blob store directly (the on-disk store is exactly what the next
# heartbeat reports as node_blobs; its disappearance there is the durable signal).
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree, with
# the dev `artifacts.scrub` block (short interval/reverify) in place:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-blob-scrub   (or: bash dev/smoke/blob-scrub/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # producer node (create + snapshot pinned here)
SRC_VM="${SRC_VM:-scrub-src}"                 # source VM on node-1; snapshotted
POOL="${POOL:-scrub-$(date +%s)}"             # this smoke's OWN artifact pool, K=2
SNAP_NAME="${SNAP_NAME:-s1}"                  # snapshot name, unique within SRC_VM
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"                 # seconds for the snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
FIRST_DURABLE_WAIT="${FIRST_DURABLE_WAIT:-180}"  # seconds for the first replica placement (reconcile ~10s)
# The dev scrub fires every ~10s and re-verifies a blob once every ~3s, so a
# tampered copy is re-hashed and deleted within a couple of intervals; budget a
# bounded wait that covers a slow pass plus the next heartbeat clearing it.
SCRUB_WAIT="${SCRUB_WAIT:-90}"                # seconds for the scrub to delete the corrupt copy
LEFT_WAIT="${LEFT_WAIT:-90}"                  # seconds for the corrupt-copy loss to drop durability
# Re-heal: the reconcile re-replicates from the healthy holder back to K=2 (pull +
# heartbeat). Give it several intervals plus task + transfer latency.
REHEAL_WAIT="${REHEAL_WAIT:-300}"            # seconds to re-replicate back to 2 holders

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

# wait_left_durable SNAP-ID TIMEOUT -> wait until durability!=durable OR observed<2
# (the corrupt copy's deletion drops the snapshot below its replication factor).
wait_left_durable() {
  local id="$1" to="$2" deadline d o
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    d="$(snap_field "$id" '.durability')"; o="$(snap_field "$id" '.observed_replicas')"
    [[ "$d" != "durable" || "${o:-0}" -lt 2 ]] && { pass "corrupt-copy loss observed (durability=$d observed=$o)"; return 0; }
    sleep 2
  done
  fail "durability stayed durable after the corrupt copy was deleted within ${to}s"
}

# blob_present IDX DIGEST -> 0 if node IDX physically holds the blob, 1 otherwise
blob_present() {
  local idx="$1" digest="$2"
  run_on "$(smoke_handle "$idx")" sudo test -f "$(smoke_state "$idx")/artifacts/blobs/$digest" 2>/dev/null
}

# sidecar_present IDX DIGEST -> 0 if node IDX still holds the <digest>.sha256
# sidecar, 1 otherwise.
sidecar_present() {
  local idx="$1" digest="$2"
  run_on "$(smoke_handle "$idx")" sudo test -f "$(smoke_state "$idx")/artifacts/blobs/${digest}.sha256" 2>/dev/null
}

# wait_blob_gone IDX DIGEST TIMEOUT -> block until node IDX no longer physically
# holds the blob (the scrubber deleted it); non-zero on timeout.
wait_blob_gone() {
  local idx="$1" digest="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    blob_present "$idx" "$digest" || return 0
    sleep 3
  done
  return 1
}

# scrub_delete_count IDX SINCE -> count of `blob scrub: artifact store` agent
# journal lines reporting at least one corrupt_deleted, since SINCE, on node IDX.
# Corroborates the on-disk disappearance with the scrubber's own log evidence.
scrub_delete_count() {
  local idx="$1" since="$2" n
  n="$(run_on "$(smoke_handle "$idx")" sudo journalctl -u otherix-agent --since "$since" --no-pager 2>/dev/null \
        | grep -E 'blob scrub: artifact store' \
        | grep -E '"corrupt_deleted":[1-9]|corrupt_deleted=[1-9]' \
        | grep -cE '.' 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# --- the cloud-config (always-on serial echo so the source disk has boot state) ---
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot. The source step waits for the marker
# so the disk has post-boot state before we snapshot it.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-scrub-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix blob-scrub-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_SCRUB_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-scrub-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- bookkeeping for cleanup ------------------------------------------
SRC_ID=""; SNAP_ID=""

# snapshot_id -> prints the snapshot uuid for $SNAP_NAME on $SRC_ID ("" if absent)
snapshot_id() {
  [ -n "${SRC_ID:-}" ] || return 0
  otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null \
    | jq -r --arg n "$SNAP_NAME" 'first(.data[]? | select(.name==$n)) | .id // empty' 2>/dev/null || true
}

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$SRC_VM" --wait --force >/dev/null 2>&1 || true
  local sid; sid="$(snapshot_id || true)"
  [ -n "$sid" ] && otx snapshot delete "$sid" --wait >/dev/null 2>&1 || true
  otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== blob-scrub smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"

# All three nodes must be ready: node-1 produces, one of node-2/node-3 becomes the
# 2nd holder (the tamper target), the remaining node is a re-replication candidate.
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
echo "STEP 1: artifact-pool create $POOL --replication-factor 2 --members all"
otx artifact-pool create "$POOL" --replication-factor 2 --members all \
  || fail "artifact-pool create $POOL failed"
otx artifact-pool get "$POOL" --output json >/dev/null \
  || fail "artifact-pool get $POOL failed after create"
pass "artifact-pool $POOL created (K=2)"

# --- step 2: create the source VM on node-1 -> running, lay down disk state ----
echo "STEP 2: create $SRC_VM on $NODE1 -> running"
printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
info "$SRC_VM id=$SRC_ID"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$SRC_ID" "OTHERIX_SCRUB_BOOT ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $SRC_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "${SMOKE_STATE_1}/vms/${SRC_ID}/serial.log" 2>/dev/null || true; \
       fail "$SRC_VM did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$SRC_VM created, running, and booted on $NODE1"

# --- step 3: snapshot into the artifact pool -> ready, durable on 2 holders ----
echo "STEP 3: vm snapshot $SRC_VM --name $SNAP_NAME --artifact-pool $POOL -> ready -> durable"
otx vm snapshot "$SRC_VM" --name "$SNAP_NAME" --artifact-pool "$POOL" \
  --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot --artifact-pool $POOL did not finish within ${SNAP_WAIT}s"
SNAP_STATUS=""
deadline=$(( SECONDS + PHASE_WAIT ))
while (( SECONDS < deadline )); do
  read -r SNAP_ID SNAP_STATUS < <(otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null \
    | jq -r --arg n "$SNAP_NAME" 'first(.data[]? | select(.name==$n)) | "\(.id) \(.status)"' 2>/dev/null) || true
  [[ "$SNAP_STATUS" == "ready" ]] && break
  sleep 2
done
[[ "$SNAP_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve snapshot id for '$SNAP_NAME' (got '$SNAP_ID')"
[[ "$SNAP_STATUS" == "ready" ]] || fail "snapshot '$SNAP_NAME' status: want 'ready' got '${SNAP_STATUS:-none}'"
DIGEST="$(snap_field "$SNAP_ID" '.disks[0].sha256')"
[[ "$DIGEST" =~ ^[0-9a-f]{64}$ ]] || fail "could not resolve disk[0] sha256 for snapshot $SNAP_ID (got '$DIGEST')"
info "snapshot id=$SNAP_ID disk[0] digest D=$DIGEST"
wait_durable "$SNAP_ID" "$FIRST_DURABLE_WAIT"
pass "snapshot $SNAP_NAME ready and durable (blob D on 2 nodes)"

# --- step 4: identify the 2nd holder node (the tamper target) ----------
echo "STEP 4: find the 2nd holder node (non-node-1 node holding blob D)"
# The producer is node-1 (index 1). The 2nd replica landed on node-2 or node-3.
# Probe each candidate's content-addressed blob store for the digest.
HOLDER_IDX=""
deadline=$(( SECONDS + 60 ))
while (( SECONDS < deadline )) && [[ -z "$HOLDER_IDX" ]]; do
  for i in 2 3; do blob_present "$i" "$DIGEST" && { HOLDER_IDX="$i"; break; }; done
  [[ -z "$HOLDER_IDX" ]] && sleep 3
done
[[ -n "$HOLDER_IDX" ]] || fail "no 2nd holder found among node-2/node-3 for digest $DIGEST"
HOLDER_NODE="node-$HOLDER_IDX"
# The producer node-1 is the healthy holder we expect the scrubber to LEAVE alone.
HEALTHY_IDX=1; HEALTHY_NODE="$NODE1"
blob_present "$HEALTHY_IDX" "$DIGEST" || fail "producer $HEALTHY_NODE unexpectedly does not hold blob D"
info "2nd holder (tamper target) is $HOLDER_NODE; healthy holder (must survive) is $HEALTHY_NODE"
pass "blob D present on $HEALTHY_NODE (producer) and $HOLDER_NODE (replica)"

# --- step 5: tamper with the holder's on-disk copy (bit-rot) -----------
echo "STEP 5: corrupt the on-disk copy of D on $HOLDER_NODE (append bytes, keep the sidecar)"
# Record the journal window just before the tamper so the corroborating scrub-log
# read only counts a delete that this corruption caused.
SCRUB_SINCE="$(date '+%Y-%m-%d %H:%M:%S')"
BLOB_PATH="$(smoke_state "$HOLDER_IDX")/artifacts/blobs/$DIGEST"
# Append bytes so the content no longer hashes to D, while LEAVING the sidecar in
# place - exactly bit-rot the Put-time verify cannot catch, only a re-hash can.
run_on "$(smoke_handle "$HOLDER_IDX")" sudo bash -c "printf CORRUPT >> '$BLOB_PATH'" \
  || fail "could not append corruption to $BLOB_PATH on $HOLDER_NODE"
sidecar_present "$HOLDER_IDX" "$DIGEST" \
  || fail "sidecar ${DIGEST}.sha256 unexpectedly gone on $HOLDER_NODE after tamper (it must remain)"
blob_present "$HOLDER_IDX" "$DIGEST" \
  || fail "blob file disappeared on $HOLDER_NODE right after tamper (expected it to still exist, just corrupt)"
pass "blob D corrupted on $HOLDER_NODE (sidecar left intact); waiting for a scrub pass"

# --- step 6: the scrubber detects + deletes the corrupt copy -----------
echo "STEP 6: scrub pass deletes the corrupt copy of D on $HOLDER_NODE"
wait_blob_gone "$HOLDER_IDX" "$DIGEST" "$SCRUB_WAIT" \
  || { echo "--- $HOLDER_NODE agent journal tail ---"; run_on "$(smoke_handle "$HOLDER_IDX")" sudo journalctl -u otherix-agent --since "$SCRUB_SINCE" --no-pager 2>/dev/null | grep -E 'blob scrub' | tail -10 || true; \
       fail "corrupt blob D not deleted from $HOLDER_NODE within ${SCRUB_WAIT}s (scrubber did not collect it)"; }
# The sidecar must be gone too (the scrubber deletes blob + sidecar together).
sidecar_present "$HOLDER_IDX" "$DIGEST" \
  && fail "sidecar ${DIGEST}.sha256 still present on $HOLDER_NODE after the corrupt blob was deleted"
pass "corrupt copy of D deleted from $HOLDER_NODE's blob store (blob + sidecar gone)"

# Corroboration: the scrubber's own log line reports a corrupt_deleted delete.
if (( $(scrub_delete_count "$HOLDER_IDX" "$SCRUB_SINCE") > 0 )); then
  pass "agent journal on $HOLDER_NODE logged 'blob scrub: artifact store' with corrupt_deleted > 0"
else
  info "scrub-delete log line not observed on $HOLDER_NODE (the on-disk disappearance above is the durable signal)"
fi

# --- step 7: durability drops then re-heals back to 2 holders ----------
echo "STEP 7: durability transiently leaves durable, then re-replicates back to 2 holders"
wait_left_durable "$SNAP_ID" "$LEFT_WAIT"
pass "the deleted copy was observed: durability left durable"

wait_durable "$SNAP_ID" "$REHEAL_WAIT"
# Confirm a 2nd live holder physically holds the blob again (re-replicated onto
# node-2/node-3 - back to H once it re-pulls a clean copy, or onto the third node).
deadline=$(( SECONDS + 60 )); healed=0
while (( SECONDS < deadline )); do
  for i in 2 3; do blob_present "$i" "$DIGEST" && { healed=1; break; }; done
  (( healed == 1 )) && break
  sleep 3
done
(( healed == 1 )) || fail "no replica holder physically holds blob D after the re-heal window"
pass "re-replicated: a 2nd holder physically holds D again and durability is durable (2 holders)"

# --- step 8: the healthy holder was never touched ----------------------
echo "STEP 8: the untouched healthy holder $HEALTHY_NODE still holds D (only the corrupt copy was deleted)"
blob_present "$HEALTHY_IDX" "$DIGEST" \
  || fail "the healthy holder $HEALTHY_NODE LOST blob D - the scrubber deleted a healthy copy (it must delete only the corrupt one)"
pass "$HEALTHY_NODE still physically holds D - the scrubber deleted ONLY the corrupt copy"

# --- step 9: cleanup ---------------------------------------------------
echo "STEP 9: cleanup"
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx snapshot delete "$SNAP_ID" --wait --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== blob-scrub smoke PASSED ===${NC}"
echo "  blob D (digest=$DIGEST) durable on $HEALTHY_NODE + $HOLDER_NODE"
echo "  corrupted on $HOLDER_NODE -> scrubber re-hashed, failed verify, deleted the corrupt copy"
echo "  durability left durable -> re-replicated back to 2 holders; $HEALTHY_NODE's healthy copy untouched"
echo "PASS"
