#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Artifact-pool replication-factor update smoke: raising a pool's
# replication_factor re-replicates its existing snapshots to the new factor,
# driven through the otherix CLI as an operator would.
#
#   1. create an artifact pool with replication factor 1
#   2. vm create on node-1, snapshot into the pool -> the blob starts on node-1 only
#   3. poll the snapshot durability -> "durable" with observed_replicas == 1 (K=1)
#   4. otherix artifact-pool update <pool> --replication-factor 2
#   5. poll the snapshot durability -> "durable" with observed_replicas == 2
#      (the reconcile loop placed a second replica because the pool's K rose)
#
# The reconcile loop reads the pool's replication factor live each pass, so
# bumping K from 1 to 2 drives an immediate second placement of every snapshot
# blob tagged into the pool. This smoke proves that operator workflow end to
# end through the CLI: no node loss, just a replication_factor change.
#
# The serial device is /dev/console (the active serial console on the cloud
# image, arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM
# smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-artifact-pool-rf-update
#        (or: bash dev/smoke/artifact-pool-rf-update/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # producer node (create + snapshot pinned here)
SRC_VM="${SRC_VM:-rfupd-src}"                 # source VM on node-1; snapshotted
POOL="${POOL:-rfupd-$(date +%s)}"             # this smoke's OWN artifact pool, starts K=1
SNAP_NAME="${SNAP_NAME:-s1}"                  # snapshot name, unique within SRC_VM
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"                 # seconds for the snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
K1_WAIT="${K1_WAIT:-120}"                     # seconds for durable at observed=1 (single producing node)
K2_WAIT="${K2_WAIT:-300}"                     # seconds to re-replicate to observed=2 after the bump
                                              # (reconcile ~10s + a pull + a heartbeat)

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

# wait_durable_obs SNAP-ID WANT-OBSERVED TIMEOUT -> wait until durability=="durable"
# AND observed_replicas == WANT-OBSERVED.
wait_durable_obs() {
  local id="$1" want="$2" to="$3" deadline d o
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    d="$(snap_field "$id" '.durability')"; o="$(snap_field "$id" '.observed_replicas')"
    [[ "$d" == "durable" && "${o:-x}" == "$want" ]] && { pass "durable (observed=$o)"; return 0; }
    sleep 3
  done
  fail "durability never reached durable with observed=$want within ${to}s (last durability=$d observed=$o)"
}

# --- the cloud-config (always-on serial echo so the source disk has boot state) ---
# An always-on systemd oneshot echoes a marker to /dev/console (arch-agnostic) on
# EVERY boot. The source step waits for the marker so the disk has post-boot state
# before we snapshot it.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-rfupd-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix artifact-pool-rf-update-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_RFUPD_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-rfupd-sentinel.service ]
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
echo "=== artifact-pool-rf-update smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

# All three nodes must be ready: node-1 produces, one of node-2/node-3 receives
# the second replica once K rises to 2.
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

# --- step 1: create the artifact pool with replication factor 1 --------
echo "=== step 1: artifact-pool create $POOL --replication-factor 1 --members all ==="
otx artifact-pool create "$POOL" --replication-factor 1 --members all \
  || fail "artifact-pool create $POOL failed"
RF0="$(otx artifact-pool get "$POOL" --output json 2>/dev/null | jq -r '.replication_factor')"
[[ "$RF0" == "1" ]] || fail "artifact-pool $POOL replication_factor: want '1' got '${RF0:-none}' after create"
pass "artifact-pool $POOL created (K=1)"

# --- step 2: create the source VM on node-1 -> running, lay down disk state ----
echo "=== step 2: create $SRC_VM on $NODE1 -> running ==="
printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
info "$SRC_VM id=$SRC_ID"
# wait for the boot marker so the disk has post-boot state before we snapshot it.
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$SRC_ID" "OTHERIX_RFUPD_BOOT ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $SRC_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "${SMOKE_STATE_1}/vms/${SRC_ID}/serial.log" 2>/dev/null || true; \
       fail "$SRC_VM did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$SRC_VM created, running, and booted on $NODE1"

# --- step 3: snapshot into the artifact pool -> ready ------------------
echo "=== step 3: vm snapshot $SRC_VM --name $SNAP_NAME --artifact-pool $POOL -> ready ==="
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
info "snapshot id=$SNAP_ID tagged into artifact pool $POOL"
pass "snapshot $SNAP_NAME ready (id=$SNAP_ID) tagged into artifact pool $POOL"

# --- step 4: at K=1 the snapshot is durable on the single producing node ----
echo "=== step 4: poll snapshot durability -> durable, observed_replicas == 1 (K=1) ==="
wait_durable_obs "$SNAP_ID" 1 "$K1_WAIT"
pass "snapshot durable at observed=1 (the single producing node) while the pool is K=1"

# --- step 5: raise the pool's replication factor to 2 ------------------
echo "=== step 5: artifact-pool update $POOL --replication-factor 2 ==="
otx artifact-pool update "$POOL" --replication-factor 2 \
  || fail "artifact-pool update $POOL --replication-factor 2 failed"
RF2="$(otx artifact-pool get "$POOL" --output json 2>/dev/null | jq -r '.replication_factor')"
[[ "$RF2" == "2" ]] || fail "artifact-pool $POOL replication_factor: want '2' got '${RF2:-none}' after update"
pass "artifact-pool $POOL replication factor raised to 2"

# --- step 6: the reconcile loop re-replicates to observed=2 ------------
echo "=== step 6: poll snapshot durability -> durable, observed_replicas == 2 (re-replicated) ==="
wait_durable_obs "$SNAP_ID" 2 "$K2_WAIT"
pass "snapshot re-replicated to observed=2 after the replication-factor bump"

# --- step 7: cleanup ---------------------------------------------------
echo "=== step 7: cleanup ==="
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx snapshot delete "$SNAP_ID" --wait --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== artifact-pool-rf-update smoke PASSED ===${NC}"
echo "  durable observed=1 at K=1  ->  artifact-pool update K=2  ->  durable observed=2"
pass "replication-factor update drove re-replication to K=2"
