#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Artifact-pool concept smoke - proves the artifact-pool concept layer
# end to end through the `otherix` CLI against a REAL agent (qemu + QMP + the
# content-addressed blob store):
#
#   1. artifact-pool create gold              -> a cluster-level artifact pool
#   2. vm create <name> --pool gold           -> 409 pool_role_invalid (a VM may
#                                                NOT be created into an artifact
#                                                pool; disk pools only)
#   3. vm create <src> (real DISK pool)       -> running (qemu boots)
#   4. vm snapshot <src> --name s1 --artifact-pool gold -> ready
#   5. snapshot get s1                         -> artifact_pool_name == "gold"
#                                                (the snapshot carries the tag)
#   6. vm create <dst> --from-snapshot s1      -> running (recreate boots, no
#                                                image download)
#   7. artifact-pool delete gold --force       -> 409 blocking_resources
#                                                {snapshots:N} (fail-closed: the
#                                                snapshot still references gold)
#
# The seeded dev stack already has a DIFFERENT cluster-default artifact pool
# named `artifacts` (created by seed-dev); this smoke uses its OWN pool `gold`
# so the two never collide and the role/tag/fail-closed assertions are isolated.
#
# The source VM is created in the real DISK pool (the cluster default `default`,
# CP-auto-provisioned on every ready node) - the SAME pool the vm-snapshots smoke
# uses. The snapshot tag (--artifact-pool gold) is the override; without it the
# snapshot would resolve to the cluster default `artifacts`.
#
# The serial device is /dev/console (the active serial console on the cloud
# image, arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring vm-snapshots.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready with the cluster default pool reconciled, and seed-dev's default
# artifact pool `artifacts` in place.
#
# Usage: make smoke-artifact-pool   (or: bash dev/smoke/artifact-pool/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
POOL="${POOL:-gold}"                      # this smoke's OWN artifact pool (NOT the seeded default `artifacts`)
SRC_VM="${SRC_VM:-ap-src}"                # source VM in the real disk pool; snapshotted
DST_VM="${DST_VM:-ap-restored}"           # VM recreated from the snapshot
BAD_VM="${BAD_VM:-ap-badpool}"            # VM whose `--pool gold` must be rejected
SNAP_NAME="${SNAP_NAME:-s1}"              # snapshot name, unique within SRC_VM
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"         # seconds for vm create -> running (incl. cold image fetch)
RECREATE_WAIT="${RECREATE_WAIT:-600}"     # seconds for vm create --from-snapshot -> running
GUEST_WAIT="${GUEST_WAIT:-720}"           # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"             # seconds for the snapshot task -> ready
PHASE_WAIT="${PHASE_WAIT:-90}"            # seconds to wait for the CP-projected status.phase

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

# serial_count VMID PATTERN -> count of lines matching PATTERN in the VM's
# serial.log (0 when the file is absent).
serial_count() {
  local id="$1" pat="$2" n
  n="$(run_on "$SMOKE_HANDLE_1" sudo grep -c "$pat" "$(smoke_state 1)/vms/${id}/serial.log" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_serial VMID PATTERN TIMEOUT -> block until PATTERN appears in serial.log.
wait_serial() {
  local id="$1" pat="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  info "watching $(smoke_state 1)/vms/${id}/serial.log on $NODE1 for '$pat' (<= ${to}s)"
  while (( SECONDS < deadline )); do
    (( $(serial_count "$id" "$pat") > 0 )) && return 0
    sleep 5
  done
  return 1
}

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$DST_VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$SRC_VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$BAD_VM" --wait --force >/dev/null 2>&1 || true
  # delete the snapshot (now unreferenced) so the artifact pool can be reclaimed
  local sid; sid="$(snapshot_id || true)"
  [ -n "$sid" ] && otx snapshot delete "$sid" --wait >/dev/null 2>&1 || true
  otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# snapshot_id -> prints the snapshot uuid for $SNAP_NAME on $SRC_ID ("" if absent)
snapshot_id() {
  [ -n "${SRC_ID:-}" ] || return 0
  otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null \
    | jq -r --arg n "$SNAP_NAME" 'first(.data[]? | select(.name==$n)) | .id // empty' 2>/dev/null || true
}

# --- the cloud-config (always-on serial echo so we can see the recreate boot) ---
# Mirrors vm-snapshots: an always-on systemd oneshot that on EVERY boot echoes a
# marker plus the live hostname to /dev/console (arch-agnostic). The recreate
# step asserts the restored guest reaches this marker - i.e. it actually boots.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-ap-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix artifact-pool-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_AP_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-ap-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- preconditions -----------------------------------------------------
echo "=== artifact-pool smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# default DISK pool reconciled on node-1 (the source VM resolves to it).
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default disk pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 ready; default disk pool ready"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: create the artifact pool ----------------------------------
echo "=== step 1: artifact-pool create $POOL ==="
otx artifact-pool create "$POOL" --replication-factor 1 --members all \
  || fail "artifact-pool create $POOL failed"
otx artifact-pool get "$POOL" --output json >/dev/null \
  || fail "artifact-pool get $POOL failed after create"
AP_ROLE="$(otx artifact-pool get "$POOL" --output json | jq -r '.role // .pool_role // empty')"
info "artifact-pool $POOL role=${AP_ROLE:-<unset>}"
pass "artifact-pool $POOL created and visible"

# --- step 2: role separation - VM create into the artifact pool is rejected ----
echo "=== step 2: vm create $BAD_VM --pool $POOL MUST fail pool_role_invalid ==="
err="$(otx vm create "$BAD_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --pool "$POOL" \
  --vcpus 2 --memory-mib 2048 --no-cloud-init 2>&1 1>/dev/null || true)"
echo "$err" | grep -q "pool_role_invalid" \
  || fail "vm create --pool $POOL did NOT return pool_role_invalid; got: ${err:-<no error>}"
# defense-in-depth: the rejected VM must not exist
otx vm get "$BAD_VM" --output json >/dev/null 2>&1 \
  && fail "rejected VM $BAD_VM unexpectedly exists" || true
pass "vm create into an artifact pool rejected with pool_role_invalid"

# --- step 3: create the source VM in the real DISK pool -> running -----
echo "=== step 3: create $SRC_VM on $NODE1 (default disk pool) -> running ==="
printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
info "$SRC_VM id=$SRC_ID"
# wait for the boot marker so the disk has post-boot state before we snapshot it.
wait_serial "$SRC_ID" "OTHERIX_AP_BOOT ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $SRC_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "$(smoke_state 1)/vms/${SRC_ID}/serial.log" 2>/dev/null || true; \
       fail "$SRC_VM did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$SRC_VM created, running, and booted"

# --- step 4: snapshot into the artifact pool -> ready ------------------
echo "=== step 4: vm snapshot $SRC_VM --name $SNAP_NAME --artifact-pool $POOL -> ready ==="
otx vm snapshot "$SRC_VM" --name "$SNAP_NAME" --artifact-pool "$POOL" \
  --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot --artifact-pool $POOL did not finish within ${SNAP_WAIT}s"
SNAP_ID=""; SNAP_STATUS=""
deadline=$(( SECONDS + PHASE_WAIT ))
while (( SECONDS < deadline )); do
  read -r SNAP_ID SNAP_STATUS < <(otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null \
    | jq -r --arg n "$SNAP_NAME" 'first(.data[]? | select(.name==$n)) | "\(.id) \(.status)"' 2>/dev/null) || true
  [[ "$SNAP_STATUS" == "ready" ]] && break
  sleep 2
done
[[ "$SNAP_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve snapshot id for '$SNAP_NAME' (got '$SNAP_ID')"
[[ "$SNAP_STATUS" == "ready" ]] || fail "snapshot '$SNAP_NAME' status: want 'ready' got '${SNAP_STATUS:-none}'"
pass "snapshot $SNAP_NAME ready (id=$SNAP_ID)"

# --- step 5: assert the snapshot carries the artifact-pool tag ---------
echo "=== step 5: snapshot get $SNAP_ID -> artifact_pool_name == $POOL ==="
GOT_POOL="$(otx snapshot get "$SNAP_ID" --output json | jq -r '.artifact_pool_name // empty')"
[[ "$GOT_POOL" == "$POOL" ]] \
  || fail "snapshot artifact_pool_name: want '$POOL' got '${GOT_POOL:-<null>}'"
pass "snapshot $SNAP_NAME tagged artifact_pool_name=$POOL"

# --- step 6: recreate FROM the snapshot -> running ---------------------
echo "=== step 6: vm create $DST_VM --from-snapshot $SNAP_ID -> running ==="
otx vm create "$DST_VM" --from-snapshot "$SNAP_ID" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 \
  --wait --wait-timeout "${RECREATE_WAIT}s" \
  || fail "vm create --from-snapshot did not reach running within ${RECREATE_WAIT}s"
assert_phase "$DST_VM" running
DST_ID="$(otx vm get "$DST_VM" --output json | jq -r '.id')"
[[ "$DST_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $DST_VM id (got '$DST_ID')"
wait_serial "$DST_ID" "OTHERIX_AP_BOOT ${DST_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "$(smoke_state 1)/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "restored VM $DST_VM did not boot from the snapshot within ${GUEST_WAIT}s"; }
pass "$DST_VM recreated from snapshot and booted"

# --- step 7: fail-closed delete - the pool is referenced by the snapshot ---
echo "=== step 7: artifact-pool delete $POOL --force MUST fail (blocking snapshots) ==="
# DST_VM still holds the recreated disk but does NOT reference the pool; only the
# snapshot does. Leave the snapshot in place so the delete is rejected. Use JSON
# output so the structured blocking_resources map (with `snapshots`) lands on
# stdout regardless of where the human-readable text/exit error is rendered.
out="$(otx artifact-pool delete "$POOL" --force --output json 2>/dev/null || true)"
[ -n "$out" ] || fail "artifact-pool delete $POOL produced no JSON (did it unexpectedly succeed?)"
# note: jq's `//` treats the boolean false as falsy, so extract `.deleted`
# directly (it is always present in the blocked-delete payload).
deleted="$(echo "$out" | jq -r '.deleted')"
[[ "$deleted" == "false" ]] || fail "artifact-pool delete $POOL was not rejected (deleted=$deleted); JSON: $out"
snap_block="$(echo "$out" | jq -r '.blocking_resources.snapshots // empty')"
[[ "$snap_block" =~ ^[0-9]+$ && "$snap_block" -ge 1 ]] \
  || fail "artifact-pool delete $POOL did NOT report blocking snapshots; JSON: $out"
# the pool must still exist after the rejected delete
otx artifact-pool get "$POOL" --output json >/dev/null \
  || fail "artifact-pool $POOL vanished despite the fail-closed delete"
pass "artifact-pool delete fail-closed: blocked by $snap_block referencing snapshot(s)"

echo
echo "${GREEN}=== artifact-pool smoke PASSED ===${NC}"
echo "PASS"
