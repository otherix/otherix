#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Cross-node artifact-pull smoke (slice C1) - proves a content-addressed snapshot
# blob is made available on a NON-PRODUCING node ON DEMAND, which is C1's whole
# point. Driven end to end through the `otherix` CLI against TWO real agents
# (qemu + QMP + the content-addressed blob store + the CP-brokered peer pull):
#
#   1. discover the producer (node-1) + a second ready consumer node (node-2)
#   2. vm create pull-src --node node-1 -> running; lay down a write-once guest
#      disk sentinel (the ORIGINAL VM name) so we can later prove the recreated
#      disk came off the ORIGINAL blob, not re-derived at recreate time
#   3. vm snapshot pull-src --name s1 -> ready; the content-addressed blob now
#      lives in node-1's artifact store (K=1: ONLY node-1 holds it)
#   4. vm create pull-dst --node node-2 --from-snapshot s1 -> running. node-2 does
#      NOT hold the blob, so the CP brokers a peer pull (node-2 <- node-1) BEFORE
#      it materializes the disk. THE C1 SEAM.
#   5. assert pull-dst's current node == node-2 (the steered placement held)
#   6. assert the restored guest reports the ORIGINAL sentinel "pull-src" (the
#      disk came off the blob pulled from node-1, not re-derived)
#   7. cleanup
#
# THE PROPAGATION / DETERMINISM APPROACH (why this is deterministic, not racy):
#   The producing node's OBSERVED blob inventory (node_blobs) lags the snapshot by
#   ONE heartbeat (dev heartbeat_interval=5s, ADR 0027) and is NOT surfaced via the
#   CLI - so we do NOT poll it. Instead we lean on the RETRYABLE classification of
#   the C1 seam: when the broker finds no live holder yet, vm.create fails
#   `blob_unavailable`, which the worker classifies RETRYABLE; the dispatcher
#   (poll 500ms, vm.create retry budget ~25) requeues the job, and the VM stays in
#   a non-terminal phase (pending/creating). `vm create --from-snapshot --wait`
#   polls the PUBLIC VM projection until status.phase == running (see
#   waitForVMPhase), so it naturally blocks THROUGH the retries until node-1's
#   inventory propagates into observed state, the pull succeeds, and the guest
#   boots. We only need a GENEROUS --wait-timeout (RECREATE_WAIT, default 600s) to
#   cover one or two 5s heartbeats + the blob transfer + a cold-ish boot on TCG.
#
# Why the sentinel proves the disk came off the pulled blob (not a self-fulfilling
# prophecy): mirrors the vm-snapshots smoke. The recreate gets a FRESH cidata seed
# with a NEW instance-id, so cloud-init re-runs the per-instance write_files +
# runcmd. The runcmd is WRITE-ONCE (`[ -f sentinel ] || hostname > sentinel`): on
# pull-src's first boot the file is absent so it records "pull-src"; on pull-dst
# the file ALREADY EXISTS on the disk that came off the pulled snapshot blob, so
# the guard skips the runcmd and the original "pull-src" value survives. An
# always-on oneshot echoes the file's contents to /dev/console every boot, captured
# by the agent into <state_root>/vms/<id>/serial.log on the TARGET (node-2). If the
# pull had served the WRONG blob (or the disk were re-derived), the value would be
# absent or "pull-dst" - both caught here.
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring vm-snapshots.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# BOTH node-1 and node-2 ready with the cluster default pool reconciled.
#
# Usage: make smoke-artifact-pull   (or: bash dev/smoke/artifact-pull/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # producer node (create + snapshot pinned here)
SRC_VM="${SRC_VM:-pull-src}"                  # source VM on node-1; snapshotted then deleted
DST_VM="${DST_VM:-pull-dst}"                  # VM recreated from the snapshot, STEERED to node-2
SNAP_NAME="${SNAP_NAME:-s1}"                  # snapshot name, unique within SRC_VM
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
# RECREATE_WAIT must cover one or two heartbeats (dev=5s) for the holder inventory
# to propagate + the blob transfer + a TCG boot, since vm create --from-snapshot
# --wait blocks THROUGH the blob_unavailable retries until status.phase==running.
RECREATE_WAIT="${RECREATE_WAIT:-600}"         # seconds for vm create --from-snapshot -> running (cross-node pull)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"                 # seconds for the snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node

# The guest records the ORIGINAL hostname into /var/lib/otherix-smoke/sentinel and
# echoes it to /dev/console - the active serial console (arch-agnostic). Both paths
# live literally inside the quoted cloud-config heredoc below.

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME -> prints the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# vm_node NAME -> prints the VM's current node name ("" if unscheduled/gone). The
# CP keeps .node = current_node_id, so this is the placement signal.
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

# assert_gone NAME -> poll until `vm get` no longer returns the VM (404)
assert_gone() {
  local name="$1" deadline; deadline=$(( SECONDS + PHASE_WAIT ))
  while (( SECONDS < deadline )); do
    otx vm get "$name" --output json >/dev/null 2>&1 || { pass "$name gone (404)"; return 0; }
    sleep 2
  done
  fail "$name still visible after delete within ${PHASE_WAIT}s"
}

# serial_count HANDLE STATE VMID PATTERN -> count of lines matching PATTERN in the
# VM's serial.log on the node identified by HANDLE/STATE (0 when the file is
# absent). The recreate's serial.log lives on the TARGET node (node-2), so this
# takes an explicit handle/state rather than hard-coding node-1.
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

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$DST_VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$SRC_VM" --wait --force >/dev/null 2>&1 || true
  # delete the snapshot (now unreferenced) so its blob can be reclaimed
  local sid; sid="$(snapshot_id || true)"
  [ -n "$sid" ] && otx snapshot delete "$sid" --wait >/dev/null 2>&1 || true
}
trap cleanup EXIT

# snapshot_id -> prints the snapshot uuid for $SNAP_NAME on $SRC_ID ("" if absent)
snapshot_id() {
  [ -n "${SRC_ID:-}" ] || return 0
  otx snapshot list --vm "$SRC_ID" --output json 2>/dev/null \
    | jq -r --arg n "$SNAP_NAME" 'first(.data[]? | select(.name==$n)) | .id // empty' 2>/dev/null || true
}

# --- the cloud-config (write-once disk sentinel + always-on serial echo) -----
# Mirrors vm-snapshots: an always-on systemd oneshot echoes the sentinel file's
# contents to /dev/console (arch-agnostic) on EVERY boot; the runcmd records the
# ORIGINAL hostname into the sentinel WRITE-ONCE. On pull-src's first boot the
# file is absent so it captures "pull-src"; on the recreate (pull-dst) the file
# already exists on the disk that came off the pulled snapshot blob, so the
# `[ -f ]` guard skips it and the original value is preserved. Quoted heredoc ->
# the guest shell vars stay literal.
#
# CRASH-CONSISTENCY: the snapshot is a crash-consistent blockdev-backup, NOT the
# guest page cache. The oneshot runs `sync` BEFORE it echoes, and the runcmd ends
# with `sync`: step 3 gates on seeing the echo, so "echo seen" implies the
# sentinel + unit + enable symlink are flushed to disk and thus captured by the
# snapshot taken afterward.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-pull-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix artifact-pull-smoke sentinel echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_SENTINEL $(cat /var/lib/otherix-smoke/sentinel 2>/dev/null || echo MISSING)" > /dev/console; echo "OTHERIX_HOSTNAME $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ mkdir, -p, /var/lib/otherix-smoke ]
  - [ sh, -c, '[ -f /var/lib/otherix-smoke/sentinel ] || hostname > /var/lib/otherix-smoke/sentinel' ]
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-pull-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- preconditions -----------------------------------------------------
echo "=== artifact-pull smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"

# The producer (node-1) must be ready.
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# Discover the consumer NODE2: a ready node that is NOT the producer. Read the
# ready-node set from the CP (mirrors vm-migration-live) rather than hard-coding
# node-2, so the smoke follows whatever the stack named the second node.
NODE2="$(otx node list --status ready --output json 2>/dev/null \
  | jq -r --arg src "$NODE1" '[.data[]? | select(.name != $src) | .name] | first // ""')"
[[ -n "$NODE2" ]] || fail "no second ready node found besides $NODE1 (run make local-dev-start for a two-node stack)"
[[ "$NODE2" != "$NODE1" ]] || fail "producer and consumer resolved to the same node ($NODE2)"
info "producer=$NODE1 consumer=$NODE2"

# default pool reconciled on BOTH the producer and the consumer (create binds on
# both; the recreate is steered to node-2 and must find the default pool there).
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
for n in "$NODE1" "$NODE2"; do
  deadline=$(( SECONDS + 60 )); ok=0
  while (( SECONDS < deadline )); do pool_ready "$n" && { ok=1; break; }; sleep 3; done
  (( ok == 1 )) || fail "default pool not ready on $n within 60s (CP auto-provision)"
done
pass "CP up (${CP_VERSION}); $NODE1 + $NODE2 ready; default pool ready on both"

# The dev stack maps node-1 -> handle/state index 1 and the second ready node ->
# index 2. The recreate is steered to NODE2, so its serial.log lives on index 2.
SRC_HANDLE="$SMOKE_HANDLE_1"; SRC_STATE="$SMOKE_STATE_1"
DST_HANDLE="$SMOKE_HANDLE_2"; DST_STATE="$SMOKE_STATE_2"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: create the source VM on node-1 -> running, lay down disk state ----
echo "=== step 1: create $SRC_VM on $NODE1 -> running ==="
printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
# confirm the source landed on node-1 (the producing node that will hold the blob)
SRC_NODE="$(vm_node "$SRC_VM")"
[[ "$SRC_NODE" == "$NODE1" ]] || fail "$SRC_VM landed on '$SRC_NODE', expected producer $NODE1"
info "$SRC_VM id=$SRC_ID node=$SRC_NODE"
pass "$SRC_VM created and running on producer $NODE1"

# The disk sentinel must be committed BEFORE the snapshot. Wait until the always-on
# oneshot echoed the sentinel with the ORIGINAL hostname ($SRC_VM) - that confirms
# the runcmd ran and the file is flushed to disk.
echo "=== step 2: confirm post-boot disk state on $SRC_VM ==="
wait_serial "$SRC_HANDLE" "$SRC_STATE" "$SRC_ID" "OTHERIX_SENTINEL ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $SRC_VM serial tail ---"; run_on "$SRC_HANDLE" sudo tail -40 "${SRC_STATE}/vms/${SRC_ID}/serial.log" 2>/dev/null || true; \
       fail "sentinel '$SRC_VM' not seen on $SRC_VM serial within ${GUEST_WAIT}s (runcmd did not commit disk state)"; }
pass "disk sentinel committed on $SRC_VM (value=$SRC_VM)"

# --- step 3: snapshot on node-1 -> ready (the blob lands ONLY on node-1, K=1) ---
echo "=== step 3: snapshot $SRC_VM --name $SNAP_NAME -> ready ==="
otx vm snapshot "$SRC_VM" --name "$SNAP_NAME" --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot did not finish within ${SNAP_WAIT}s"
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
pass "snapshot $SNAP_NAME ready (id=$SNAP_ID) - blob held ONLY by $NODE1 (K=1)"

# --- step 4: recreate STEERED to node-2 -> running (forces the cross-node pull) ---
echo "=== step 4: create $DST_VM --node $NODE2 --from-snapshot $SNAP_ID -> running ==="
# node-2 does NOT hold the blob (K=1, only node-1 has it), so the CP brokers a
# peer pull node-2 <- node-1 BEFORE it materializes the disk - the C1 seam. The
# observed holder inventory lags the snapshot by one heartbeat and is NOT polled;
# instead --wait blocks THROUGH the retryable blob_unavailable retries (see the
# header note) until status.phase==running, which is why RECREATE_WAIT is generous.
# No --image-url / --user-data: the disk materializes from the pulled snapshot blob
# and gets a fresh cidata seed whose hostname is the NEW VM name ($DST_VM).
otx vm create "$DST_VM" --from-snapshot "$SNAP_ID" --node "$NODE2" \
  --vcpus 2 --memory-mb 2048 \
  --wait --wait-timeout "${RECREATE_WAIT}s" \
  || fail "vm create --from-snapshot (steered to $NODE2) did not reach running within ${RECREATE_WAIT}s (cross-node pull failed?)"
assert_phase "$DST_VM" running
DST_ID="$(otx vm get "$DST_VM" --output json | jq -r '.id')"
[[ "$DST_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $DST_VM id (got '$DST_ID')"
info "$DST_VM id=$DST_ID"
pass "$DST_VM created and running from the cross-node-pulled snapshot"

# --- step 5: assert the recreate landed on node-2 (the steered placement held) ---
echo "=== step 5: assert $DST_VM current node == $NODE2 ==="
assert_node "$DST_VM" "$NODE2"
pass "$DST_VM placed on the non-producing node $NODE2 (the pull served the blob there)"

# --- step 6: assert the restored guest reports the ORIGINAL sentinel -----------
echo "=== step 6: assert disk state came off the pulled blob (sentinel == $SRC_VM) ==="
# The on-disk file came off the snapshot blob pulled from node-1, so the always-on
# oneshot on node-2 echoes the ORIGINAL value ($SRC_VM) even though this is a
# different VM on a different node. If the pull had served the wrong blob (or the
# disk were re-derived), the runcmd would have re-run write-once and recorded
# $DST_VM (or MISSING) - both caught here.
wait_serial "$DST_HANDLE" "$DST_STATE" "$DST_ID" "OTHERIX_SENTINEL ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ($NODE2) ---"; run_on "$DST_HANDLE" sudo tail -60 "${DST_STATE}/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "restored guest on $NODE2 did not report the original sentinel '$SRC_VM' - the cross-node pull did not deliver the original blob"; }
pass "disk came off the pulled blob: restored guest on $NODE2 reports original sentinel '$SRC_VM'"

# The fresh cidata seed re-applied the NEW VM name as the guest hostname.
wait_serial "$DST_HANDLE" "$DST_STATE" "$DST_ID" "OTHERIX_HOSTNAME ${DST_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ($NODE2) ---"; run_on "$DST_HANDLE" sudo tail -60 "${DST_STATE}/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "restored guest hostname is not '$DST_VM' - the cidata seed did not apply the new VM name"; }
pass "hostname re-applied: restored guest on $NODE2 hostname is '$DST_VM'"

# --- step 7: cleanup ---------------------------------------------------
echo "=== step 7: cleanup ==="
otx vm delete "$DST_VM" --wait --force --wait-timeout "${OP_WAIT}s" || fail "vm delete $DST_VM failed"
assert_gone "$DST_VM"
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" || fail "vm delete $SRC_VM failed"
assert_gone "$SRC_VM"
otx snapshot delete "$SNAP_ID" --wait --wait-timeout "${OP_WAIT}s" || fail "snapshot delete $SNAP_ID failed"
pass "pull-dst, pull-src, and snapshot deleted"

trap - EXIT
echo
echo "${GREEN}=== artifact-pull smoke PASSED ===${NC}"
echo "PASS"
