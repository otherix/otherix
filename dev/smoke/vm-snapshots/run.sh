#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM snapshot smoke - proves the disk-only snapshot data path end to end through
# the `otherix` CLI against a REAL agent (qemu + QMP + the content-addressed blob
# store): create a VM, lay down post-boot disk state, snapshot it, delete the
# source VM, then recreate a brand-new VM FROM the snapshot and assert the disk
# state survived the snapshot -> recreate round-trip.
#
# What it proves, end to end, on node-1 (no network, SLIRP):
#   vm create snap-src      -> running   (qemu spawned, QMP responsive, guest boots)
#   <disk state>            -> a write-once runcmd records the ORIGINAL hostname
#                             (snap-src) into /var/lib/otherix-smoke/sentinel on
#                             the guest disk, on the FIRST boot only
#   vm snapshot <vm> --name s1 -> ready      (disk-only, content-addressed blobs)
#   vm delete snap-src      -> gone
#   vm create snap-restored --from-snapshot <s1> -> running (no image download)
#   ASSERT disk survived    -> the restored guest still reports the ORIGINAL
#                             sentinel value "snap-src" (the file came off the
#                             snapshot disk, NOT re-derived at recreate time)
#   ASSERT hostname applied  -> the restored guest hostname is "snap-restored"
#                             (the always-on cidata seed re-applied the NEW VM
#                             name as hostname on the recreate)
#
# Why the sentinel proves disk survival (and is not a self-fulfilling prophecy):
# the recreated VM gets a FRESH cidata seed whose instance-id is
# iid-otherix-snap-restored - a NEW instance-id, so cloud-init re-runs the
# per-instance modules (write_files + runcmd) on the recreate. The runcmd is
# therefore written WRITE-ONCE: `[ -f sentinel ] || echo "$(hostname) > sentinel"`.
# On snap-src's first boot the file is absent, so the runcmd records "snap-src".
# On snap-restored the file ALREADY EXISTS on the disk that came off the snapshot,
# so the guard skips the runcmd and the file keeps its original "snap-src" value.
# A separate always-on systemd oneshot echoes the file's contents + the live
# hostname to the serial console on EVERY boot, which the agent captures into
# <state_root>/vms/<id>/serial.log. If the snapshot had NOT preserved the disk,
# the file would either be absent or hold "snap-restored" - both caught here.
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring vm-migration-live-logs.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready with the cluster default pool reconciled.
#
# Usage: make smoke-vm-snapshots   (or: bash dev/smoke/vm-snapshots/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
SRC_VM="${SRC_VM:-snap-src}"              # the source VM; snapshotted then deleted
DST_VM="${DST_VM:-snap-restored}"         # the VM recreated from the snapshot
SNAP_NAME="${SNAP_NAME:-s1}"              # snapshot name, unique within SRC_VM
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"         # seconds for vm create -> running (incl. cold image fetch)
RECREATE_WAIT="${RECREATE_WAIT:-600}"     # seconds for vm create --from-snapshot -> running
GUEST_WAIT="${GUEST_WAIT:-720}"           # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"             # seconds for the snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                 # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"            # seconds to wait for the CP-projected status.phase

# The guest records the ORIGINAL hostname into /var/lib/otherix-smoke/sentinel and
# echoes it (plus the live hostname) to /dev/console - the active serial console
# (arch-agnostic). Both paths live literally inside the quoted cloud-config
# heredoc below (the guest shell, not this script, reads them).

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

# assert_gone NAME -> poll until `vm get` no longer returns the VM (404)
assert_gone() {
  local name="$1" deadline; deadline=$(( SECONDS + PHASE_WAIT ))
  while (( SECONDS < deadline )); do
    otx vm get "$name" --output json >/dev/null 2>&1 || { pass "$name gone (404)"; return 0; }
    sleep 2
  done
  fail "$name still visible after delete within ${PHASE_WAIT}s"
}

# serial_count VMID PATTERN -> count of lines matching PATTERN in the VM's
# serial.log (0 when the file is absent). The agent appends the guest console
# there; callers wait for a count rather than a single grep so a stale match
# across reboots does not race them.
serial_count() {
  local id="$1" pat="$2" n
  n="$(run_on "$SMOKE_HANDLE_1" sudo grep -c "$pat" "$(smoke_state 1)/vms/${id}/serial.log" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_file_gone PATH TIMEOUT -> block until PATH no longer exists on node-1.
# The agent's snapshot delete is async (fire-and-forget: the CP task finalizes
# once the agent ACCEPTS), so the blob/manifest unlink lands shortly AFTER the
# CLI --wait returns; poll rather than assert once to avoid racing it.
wait_file_gone() {
  local path="$1" to="$2" deadline; deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    run_on "$SMOKE_HANDLE_1" sudo test -f "$path" 2>/dev/null || return 0
    sleep 2
  done
  return 1
}

# wait_serial VMID PATTERN TIMEOUT -> block until PATTERN appears in the VM's
# serial.log; returns non-zero on timeout.
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
}
trap cleanup EXIT

# --- the cloud-config (write-once disk sentinel + always-on serial echo) -----
# write_files installs an always-on systemd oneshot ordered After=multi-user.target
# that on EVERY boot echoes "OTHERIX_SENTINEL <contents-of-sentinel-file>" and
# "OTHERIX_HOSTNAME <live-hostname>" to /dev/console (arch-agnostic). The runcmd
# records the ORIGINAL hostname into the sentinel file WRITE-ONCE: on snap-src's
# first boot the file is absent so it captures "snap-src"; on the recreate the
# file already exists on the disk that came off the snapshot, so the `[ -f ]`
# guard skips it and the original value is preserved. Quoted heredoc -> the guest
# shell vars stay literal (not expanded here).
#
# CRASH-CONSISTENCY: the snapshot is a crash-consistent blockdev-backup of the
# block device, NOT the guest page cache - recent writes that have not been
# flushed to the virtual disk are NOT captured. So the oneshot runs `sync` BEFORE
# it echoes, and the runcmd ends with `sync`: because step 2 gates on seeing the
# echo, "echo seen" implies the sentinel + the service unit + its enable symlink
# are flushed to disk, so the snapshot (taken after step 2) captures them and the
# recreate boots the oneshot. Without the sync the recreate disk is missing the
# just-written files and step 6 fails - which is the real crash-consistency
# limitation, surfaced here as a test-harness requirement, not a product bug.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-snap-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix snapshot-smoke sentinel echo
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
  - [ systemctl, enable, --now, otherix-snap-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- preconditions -----------------------------------------------------
echo "=== vm-snapshots smoke: preconditions ==="
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

# --- step 1: create the source VM -> running, lay down disk state ------
echo "=== step 1: create $SRC_VM on $NODE1 -> running ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers
printf '%s' "$CLOUD_INIT" | otx vm create "$SRC_VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $SRC_VM did not reach running within ${CREATE_WAIT}s"
assert_phase "$SRC_VM" running
SRC_ID="$(otx vm get "$SRC_VM" --output json | jq -r '.id')"
[[ "$SRC_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $SRC_VM id (got '$SRC_ID')"
info "$SRC_VM id=$SRC_ID"
pass "$SRC_VM created and running"

# The disk state must exist BEFORE the snapshot. Wait until the always-on oneshot
# has echoed the sentinel with the ORIGINAL hostname ($SRC_VM) - that confirms the
# runcmd ran and the file is committed to the disk.
echo "=== step 2: confirm post-boot disk state on $SRC_VM ==="
wait_serial "$SRC_ID" "OTHERIX_SENTINEL ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $SRC_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "$(smoke_state 1)/vms/${SRC_ID}/serial.log" 2>/dev/null || true; \
       fail "sentinel '$SRC_VM' not seen on $SRC_VM serial within ${GUEST_WAIT}s (runcmd did not commit disk state)"; }
pass "disk sentinel committed on $SRC_VM (value=$SRC_VM)"

# --- step 3: snapshot -> ready -----------------------------------------
echo "=== step 3: snapshot $SRC_VM --name $SNAP_NAME -> ready ==="
otx vm snapshot "$SRC_VM" --name "$SNAP_NAME" --wait --wait-timeout "${SNAP_WAIT}s" \
  || fail "vm snapshot did not finish within ${SNAP_WAIT}s"

# Resolve the snapshot id and confirm it reached ready (the create --wait already
# blocked on the task, but assert the resource status explicitly). The global
# `snapshot list --vm <id>` is the operator path; filter to this VM by uuid.
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

# --- step 4: delete the source VM -> gone ------------------------------
echo "=== step 4: delete $SRC_VM -> gone ==="
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" || fail "vm delete $SRC_VM failed"
assert_gone "$SRC_VM"
pass "$SRC_VM deleted"

# --- step 5: recreate FROM the snapshot -> running ---------------------
echo "=== step 5: create $DST_VM --from-snapshot $SNAP_ID -> running ==="
# arch/firmware come from the snapshot; only cpu/mem/name are supplied. No
# --image-url and no --user-data: the recreate path materializes the disk from
# the content-addressed snapshot blob and builds a fresh always-on cidata seed
# whose hostname is the NEW VM name ($DST_VM).
otx vm create "$DST_VM" --from-snapshot "$SNAP_ID" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 \
  --wait --wait-timeout "${RECREATE_WAIT}s" \
  || fail "vm create --from-snapshot did not reach running within ${RECREATE_WAIT}s"
assert_phase "$DST_VM" running
DST_ID="$(otx vm get "$DST_VM" --output json | jq -r '.id')"
[[ "$DST_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $DST_VM id (got '$DST_ID')"
info "$DST_VM id=$DST_ID"
pass "$DST_VM created and running from snapshot"

# --- step 6: assert disk survived + hostname re-applied ----------------
echo "=== step 6: assert disk state survived snapshot -> recreate ==="
# The on-disk file came off the snapshot, so the always-on oneshot echoes the
# ORIGINAL value ($SRC_VM) even though this is a different VM. If the snapshot had
# not preserved the disk, the runcmd would have re-run write-once and recorded
# $DST_VM (or MISSING) - both caught here.
wait_serial "$DST_ID" "OTHERIX_SENTINEL ${SRC_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "$(smoke_state 1)/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "restored guest did not report the original sentinel '$SRC_VM' - disk state did NOT survive the snapshot"; }
pass "disk state survived: restored guest reports original sentinel '$SRC_VM'"

# The fresh cidata seed re-applied the NEW VM name as the guest hostname.
wait_serial "$DST_ID" "OTHERIX_HOSTNAME ${DST_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -60 "$(smoke_state 1)/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "restored guest hostname is not '$DST_VM' - the cidata seed did not apply the new VM name"; }
pass "hostname re-applied: restored guest hostname is '$DST_VM'"

# --- step 7: delete the standalone snapshot AFTER its source VM is gone --
# This is the regression gate for the placement-map blob GC: SRC_VM (the snapshot's
# source) was deleted back in step 4, so the snapshot is now fully standalone. The
# old GC resolved the holder node via the VM and failed (node_unreachable: load vm:
# store: not found), leaking the blob; the placement-map fix must (a) finish the
# delete task SUCCESSfully and (b) physically remove the content-addressed blob +
# manifest on node-1. Delete DST_VM first so the snapshot's blob is unshared.
echo "=== step 7: delete $DST_VM, then the standalone snapshot (source VM already gone) ==="
otx vm delete "$DST_VM" --wait --force --wait-timeout "${OP_WAIT}s" || fail "vm delete $DST_VM failed"
assert_gone "$DST_VM"

# Locate the snapshot's agent-side manifest ({vm_uuid}__{name}.json) and its blob
# digests on node-1 BEFORE the delete, so we can assert they are physically gone
# AFTER. The manifest is keyed by the (now-deleted) SRC_VM's uuid - exactly the
# state the fix must handle.
MAN_PATH="$(run_on "$SMOKE_HANDLE_1" sudo find "$(smoke_state 1)" -path "*/snapshots/manifests/${SRC_ID}__${SNAP_NAME}.json" 2>/dev/null | head -1 || true)"
[ -n "$MAN_PATH" ] || fail "snapshot manifest ${SRC_ID}__${SNAP_NAME}.json not found on $NODE1 (snapshot was never materialized?)"
SNAP_DIR="$(dirname "$(dirname "$MAN_PATH")")"   # .../snapshots
DIGESTS="$(run_on "$SMOKE_HANDLE_1" sudo cat "$MAN_PATH" 2>/dev/null | jq -r '.disks[].sha256' || true)"
[ -n "$DIGESTS" ] || fail "could not read blob digests from $MAN_PATH on $NODE1"
for d in $DIGESTS; do
  run_on "$SMOKE_HANDLE_1" sudo test -f "$SNAP_DIR/${d}.qcow2" \
    || fail "blob $d.qcow2 missing on $NODE1 before delete (snapshot not materialized as expected)"
done
info "snapshot $SNAP_NAME blobs present on $NODE1: $(echo "$DIGESTS" | tr '\n' ' ')"

# The delete MUST succeed even though SRC_VM is gone (the bug failed here).
otx snapshot delete "$SNAP_ID" --wait --wait-timeout "${OP_WAIT}s" \
  || fail "snapshot delete failed after source VM deleted (placement-map GC regression)"
pass "snapshot delete succeeded with source VM already gone"

# The manifest and every blob must be physically reclaimed on node-1 (the agent
# unlink is async, so poll for absence rather than assert once).
wait_file_gone "$MAN_PATH" "$OP_WAIT" \
  || fail "snapshot manifest still present on $NODE1 ${OP_WAIT}s after delete: $MAN_PATH"
for d in $DIGESTS; do
  wait_file_gone "$SNAP_DIR/${d}.qcow2" "$OP_WAIT" \
    || fail "orphaned blob $d.qcow2 LEAKED on $NODE1 ${OP_WAIT}s after delete - placement-map GC did not reclaim it"
done
pass "snapshot blobs + manifest reclaimed on $NODE1 (no leak)"

trap - EXIT
echo
echo "${GREEN}=== vm-snapshots smoke PASSED ===${NC}"
