#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Pinned-image peer-pull smoke: a VM created from a digest-pinned image
# (--image-sha256 S) is staged onto its target node by a peer-to-peer pull from
# a node that already caches that exact image, instead of re-downloading it from
# the source URL. The pinned-image cache is a SEPARATE content-addressed tier,
# inventoried in etcd under image_blobs/<node>, fully isolated from the blob
# durability placement map: a cached image is never given a placement record and
# is never reclaimed. Driven end to end through the otherix CLI against the real
# three-node agents (qemu + QMP + the image cache tier + the CP-brokered peer pull).
#
# The proof:
#   1. PIN a digest. Resolve the test image's sha256 at runtime so the pin is
#      exact, then pin every create on it with --image-sha256 S.
#   2. FIRST create on node-1. node-1 holds no copy of S, so it DOWNLOADS from the
#      source URL. Assert: node-1 logs "image not cached, downloading from source"
#      for S; node-1 holds S in the image_blobs tier; placement/S is EMPTY (the
#      image tier must never enter the durability map).
#   3. SECOND create on node-2 from the SAME pinned image. node-2 holds no copy,
#      but node-1 does, so the CP brokers a peer pull node-2 <- node-1 BEFORE
#      materialize. Assert: node-1 SERVED it ("blob serve listener opened" for S);
#      node-2 got a CACHE HIT ("image cache hit, cloning without download" for S)
#      and did NOT log a source download for S; node-2 now also holds S.
#   4. ISOLATION across reconcile intervals. Hold past several placement.reconcile
#      ticks (dev tick ~10s) and re-assert placement/S is STILL empty and both
#      node-1 and node-2 still hold S in image_blobs (the image tier is never
#      folded into reconcile and never reclaimed).
#   5. FAIL-OPEN fallback (no peer holder -> source download -> boot). Variant
#      chosen: a SECOND, DISTINCT pinned image URL (digest S2 != S) created ONLY on
#      node-3, with NO node holding S2. This cleanly proves "no peer holder ->
#      agent downloads from source -> VM boots" without tearing a node down. Assert:
#      node-3 logs "image not cached, downloading from source" for S2 and boots.
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM smokes.
#
# We read the embedded dev etcd member directly (127.0.0.1:2379, no TLS) for the
# image_blobs and placement keys, exactly like the artifact-janitor smoke, and we
# read the agent logs via journalctl on each node, like the chaos smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or: make local-dev-cleanrestart)
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-image-cache   (or: bash dev/smoke/image-cache/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # downloads S from source (first create)
NODE2="node-2"                                # cache-hit consumer (peer-pulls S from node-1)
NODE3="node-3"                                # fail-open: downloads a DISTINCT S2 (no peer holder)
VM1="${VM1:-img-cache-1}"                     # first create on node-1 (source download of S)
VM2="${VM2:-img-cache-2}"                     # second create on node-2 (cache hit on S)
VM3="${VM3:-img-cache-3}"                     # fail-open create on node-3 (source download of S2)
# IMAGE_URL is the pinned image for the peer-pull path (steps 2-4). IMAGE_URL2 is
# a DISTINCT image (different digest) for the fail-open fallback (step 5): no node
# holds its digest, so node-3 must fall back to a source download.
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
IMAGE_URL2="${IMAGE_URL2:-https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-${SMOKE_ARCH}.img}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
# The second create blocks THROUGH the peer-pull; give it room for one or two 5s
# heartbeats (holder inventory propagation) + the blob transfer + a boot.
PULL_WAIT="${PULL_WAIT:-600}"                 # seconds for the cache-hit create -> running (cross-node pull)
# The fail-open image is a larger distinct cloudimg; allow a generous source fetch.
CREATE_WAIT2="${CREATE_WAIT2:-900}"           # seconds for the fail-open create -> running (large source fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"          # embedded etcd dev client endpoint (no TLS)
# The isolation hold must span SEVERAL placement.reconcile intervals (dev tick
# ~10s) so a buggy reconcile that folded the image tier in would have fired.
HOLD_WINDOW="${HOLD_WINDOW:-40}"              # seconds to hold-and-verify placement/S stays empty

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# etcd_values PREFIX -> values under PREFIX, one per line ("" when none)
etcd_values() { etcdctl --endpoints="$ETCD_EP" get "$1" --prefix --print-value-only 2>/dev/null | sed '/^$/d' || true; }
# etcd_keys PREFIX -> keys under PREFIX, one per line ("" when none)
etcd_keys() { etcdctl --endpoints="$ETCD_EP" get "$1" --prefix --keys-only 2>/dev/null | sed '/^$/d' || true; }

# image_blob_held NODE-UUID DIGEST -> 0 if that node's image_blobs inventory lists
# DIGEST, 1 otherwise. The image_blobs value is a JSON array of {digest,...}; the
# node uuid is the LAST path segment of the key.
image_blob_held() {
  local nid="$1" digest="$2"
  etcdctl --endpoints="$ETCD_EP" get "/otherix/image_blobs/$nid" --print-value-only 2>/dev/null \
    | jq -e --arg d "$digest" 'any(.[]?; .digest == $d)' >/dev/null 2>&1
}

# placement_empty DIGEST -> 0 if there are NO placement keys for DIGEST, 1 otherwise.
placement_empty() {
  [[ -z "$(etcd_keys "/otherix/placement/$1/")" ]]
}

# node_id NAME -> the node's uuid ("" on error)
node_id() { otx node get "$1" --output json 2>/dev/null | jq -r '.id // empty' 2>/dev/null || true; }

# agent_log_count HANDLE PATTERN -> count of journal lines matching PATTERN in the
# agent unit over the smoke window. Mirrors the chaos smokes' journalctl read.
agent_log_count() {
  local handle="$1" pat="$2" n
  n="$(run_on "$handle" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null \
        | grep -cE "$pat" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_agent_log HANDLE PATTERN TIMEOUT -> block until PATTERN appears in the
# agent journal on HANDLE; returns non-zero on timeout.
wait_agent_log() {
  local handle="$1" pat="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    (( $(agent_log_count "$handle" "$pat") > 0 )) && return 0
    sleep 3
  done
  return 1
}

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

# vm_id NAME -> the VM uuid ("" on error)
vm_id() { otx vm get "$1" --output json 2>/dev/null | jq -r '.id // empty' 2>/dev/null || true; }

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

# --- the cloud-config (always-on serial boot echo) ---------------------
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot, so each create's boot can be
# confirmed off the target node's serial.log.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-imgcache-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix image-cache-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_IMGCACHE_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-imgcache-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VM3" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM2" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM1" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== image-cache smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (it reads the embedded etcd image_blobs/placement keys)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 || fail "etcd not reachable at $ETCD_EP"

# All three nodes ready: node-1 downloads, node-2 peer-pulls, node-3 fail-open.
for n in "$NODE1" "$NODE2" "$NODE3"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start for a three-node stack"
done

# default DISK pool reconciled on every node the smoke creates a VM on.
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
for n in "$NODE1" "$NODE2" "$NODE3"; do
  deadline=$(( SECONDS + 60 )); ok=0
  while (( SECONDS < deadline )); do pool_ready "$n" && { ok=1; break; }; sleep 3; done
  (( ok == 1 )) || fail "default disk pool not ready on $n within 60s (CP auto-provision)"
done

NID1="$(node_id "$NODE1")"; NID2="$(node_id "$NODE2")"; NID3="$(node_id "$NODE3")"
[[ "$NID1" =~ ^[0-9a-f-]{36}$ && "$NID2" =~ ^[0-9a-f-]{36}$ && "$NID3" =~ ^[0-9a-f-]{36}$ ]] \
  || fail "could not resolve all node uuids (node-1=$NID1 node-2=$NID2 node-3=$NID3)"
pass "CP up (${CP_VERSION}); $NODE1/$NODE2/$NODE3 ready; default disk pool ready on all three"

# The journal read window starts now, so a stale serve/download from a previous
# run can never satisfy an assertion in this run.
JOURNAL_SINCE="$(date '+%Y-%m-%d %H:%M:%S')"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: pin the digest --------------------------------------------
echo "=== step 1: resolve and pin the image digest S ==="
info "fetching $IMAGE_URL to compute its sha256 (exact pin)"
S="$(curl -fsSL "$IMAGE_URL" | sha256sum | awk '{print $1}')"
[[ "$S" =~ ^[0-9a-f]{64}$ ]] || fail "could not compute sha256 of $IMAGE_URL (got '$S')"
pass "pinned digest S=$S"

# --- step 2: FIRST create on node-1 -> source download -----------------
echo "=== step 2: create $VM1 on $NODE1 pinned to S -> running (source download) ==="
printf '%s' "$CLOUD_INIT" | otx vm create "$VM1" \
  --image-url "$IMAGE_URL" --image-sha256 "$S" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM1 did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM1" running
VM1_ID="$(vm_id "$VM1")"
[[ "$VM1_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM1 id (got '$VM1_ID')"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$VM1_ID" "OTHERIX_IMGCACHE_BOOT ${VM1}" "$GUEST_WAIT" \
  || { echo "--- $VM1 serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "${SMOKE_STATE_1}/vms/${VM1_ID}/serial.log" 2>/dev/null || true; \
       fail "$VM1 did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$VM1 created, running, and booted on $NODE1"

# node-1 downloaded S from source (no peer held it).
wait_agent_log "$SMOKE_HANDLE_1" "image not cached, downloading from source.*${S}" 30 \
  || { run_on "$SMOKE_HANDLE_1" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'image (not cached|cache hit)' | tail -5 || true; \
       fail "$NODE1 did not log a source download for S - the first create should have fetched from the URL"; }
pass "$NODE1 logged 'image not cached, downloading from source' for S (first holder fetched from source)"

# node-1 holds S in the image cache tier.
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do image_blob_held "$NID1" "$S" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NODE1 does not list S in image_blobs - the pinned image was not inventoried in the cache tier"
pass "$NODE1 holds S in the image_blobs cache tier"

# The image tier must NEVER enter the durability placement map.
placement_empty "$S" || { echo "--- placement/$S keys ---"; etcd_keys "/otherix/placement/$S/"; \
  fail "placement/$S is NOT empty - a pinned-image cache blob leaked into the durability map"; }
pass "placement/S is EMPTY (the image cache tier stays out of the durability map)"

# --- step 3: SECOND create on node-2 from the SAME pin -> peer pull -----
echo "=== step 3: create $VM2 on $NODE2 pinned to the SAME S -> running (peer pull node-2 <- node-1) ==="
# node-2 holds no copy of S, but node-1 does, so the CP brokers a peer pull into
# node-2's image cache tier BEFORE the agent materializes the disk. The agent then
# CLONES from the staged cache (a hit), never re-downloading from the source URL.
printf '%s' "$CLOUD_INIT" | otx vm create "$VM2" \
  --image-url "$IMAGE_URL" --image-sha256 "$S" --arch "$ARCH" --node "$NODE2" \
  --vcpus 2 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${PULL_WAIT}s" \
  || fail "vm create $VM2 did not reach running within ${PULL_WAIT}s (peer pull failed?)"
assert_phase "$VM2" running
VM2_ID="$(vm_id "$VM2")"
[[ "$VM2_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM2 id (got '$VM2_ID')"
wait_serial "$SMOKE_HANDLE_2" "$SMOKE_STATE_2" "$VM2_ID" "OTHERIX_IMGCACHE_BOOT ${VM2}" "$GUEST_WAIT" \
  || { echo "--- $VM2 serial tail ($NODE2) ---"; run_on "$SMOKE_HANDLE_2" sudo tail -40 "${SMOKE_STATE_2}/vms/${VM2_ID}/serial.log" 2>/dev/null || true; \
       fail "$VM2 did not reach the boot marker on $NODE2 within ${GUEST_WAIT}s"; }
pass "$VM2 created, running, and booted on $NODE2"

# node-1 SERVED the blob (the CP opened its peer serve listener for S).
wait_agent_log "$SMOKE_HANDLE_1" "blob serve listener opened.*${S}" 30 \
  || { run_on "$SMOKE_HANDLE_1" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'blob serve listener' | tail -5 || true; \
       fail "$NODE1 did not log a peer serve for S - the holder was not asked to serve the blob"; }
pass "$NODE1 logged 'blob serve listener opened' for S (holder served the peer pull)"

# node-2 got a CACHE HIT (cloned from the staged peer pull, no source download).
wait_agent_log "$SMOKE_HANDLE_2" "image cache hit, cloning without download.*${S}" 30 \
  || { run_on "$SMOKE_HANDLE_2" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'image (not cached|cache hit)' | tail -5 || true; \
       fail "$NODE2 did not log a cache hit for S - it did not clone from the staged peer pull"; }
pass "$NODE2 logged 'image cache hit, cloning without download' for S (cloned from the peer pull)"

# node-2 must NOT have re-downloaded S from the source URL.
dl2="$(agent_log_count "$SMOKE_HANDLE_2" "image not cached, downloading from source.*${S}")"
(( dl2 == 0 )) || fail "$NODE2 logged $dl2 source download(s) for S - it re-downloaded instead of peer-pulling"
pass "$NODE2 did NOT log a source download for S (it peer-pulled, never re-fetched from the URL)"

# node-2 now also holds S in the image cache tier.
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do image_blob_held "$NID2" "$S" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NODE2 does not list S in image_blobs - the peer-pulled image was not inventoried"
pass "$NODE2 holds S in the image_blobs cache tier (both $NODE1 and $NODE2 now hold it)"

# --- step 4: isolation across reconcile intervals ----------------------
echo "=== step 4: hold past several placement.reconcile ticks; image tier stays isolated ==="
info "holding for ${HOLD_WINDOW}s (placement.reconcile tick ~10s) and re-asserting isolation"
deadline=$(( SECONDS + HOLD_WINDOW ))
while (( SECONDS < deadline )); do
  placement_empty "$S"        || { echo "--- placement/$S keys ---"; etcd_keys "/otherix/placement/$S/"; \
    fail "ISOLATION VIOLATED: placement/$S became non-empty - the image tier was folded into the durability map"; }
  image_blob_held "$NID1" "$S" || fail "ISOLATION VIOLATED: $NODE1 dropped S from image_blobs (a cached image was reclaimed)"
  image_blob_held "$NID2" "$S" || fail "ISOLATION VIOLATED: $NODE2 dropped S from image_blobs (a cached image was reclaimed)"
  sleep 5
done
pass "across ${HOLD_WINDOW}s of reconcile ticks: placement/S stayed EMPTY and both holders kept S in image_blobs (never reclaimed)"

# --- step 5: FAIL-OPEN fallback (no peer holder -> source download) -----
echo "=== step 5: fail-open: create $VM3 on $NODE3 from a DISTINCT pinned image S2 (no peer holder) ==="
# Variant chosen: a SECOND, DISTINCT image (digest S2 != S) created ONLY on node-3.
# No node holds S2, so the broker finds no holder and the create FALLS BACK to a
# source download - the fail-open path - and the VM still boots.
info "fetching $IMAGE_URL2 to compute its distinct sha256 S2"
S2="$(curl -fsSL "$IMAGE_URL2" | sha256sum | awk '{print $1}')"
[[ "$S2" =~ ^[0-9a-f]{64}$ ]] || fail "could not compute sha256 of $IMAGE_URL2 (got '$S2')"
[[ "$S2" != "$S" ]] || fail "S2 must differ from S for the no-peer-holder fallback to be meaningful"
info "fail-open digest S2=$S2 (distinct from S)"
# Sanity: no node currently holds S2 (the whole point of the fallback).
for nid in "$NID1" "$NID2" "$NID3"; do
  image_blob_held "$nid" "$S2" && fail "a node already holds S2 before the fail-open create - cannot prove the no-peer fallback"
done

printf '%s' "$CLOUD_INIT" | otx vm create "$VM3" \
  --image-url "$IMAGE_URL2" --image-sha256 "$S2" --arch "$ARCH" --node "$NODE3" \
  --vcpus 2 --memory-mib 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT2}s" \
  || fail "vm create $VM3 did not reach running within ${CREATE_WAIT2}s (fail-open source download failed?)"
assert_phase "$VM3" running
VM3_ID="$(vm_id "$VM3")"
[[ "$VM3_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM3 id (got '$VM3_ID')"
wait_serial "$SMOKE_HANDLE_3" "$SMOKE_STATE_3" "$VM3_ID" "OTHERIX_IMGCACHE_BOOT ${VM3}" "$GUEST_WAIT" \
  || { echo "--- $VM3 serial tail ($NODE3) ---"; run_on "$SMOKE_HANDLE_3" sudo tail -40 "${SMOKE_STATE_3}/vms/${VM3_ID}/serial.log" 2>/dev/null || true; \
       fail "$VM3 did not reach the boot marker on $NODE3 within ${GUEST_WAIT}s"; }
pass "$VM3 created, running, and booted on $NODE3 (fail-open path)"

# node-3 fell back to a SOURCE download for S2 (no peer held it).
wait_agent_log "$SMOKE_HANDLE_3" "image not cached, downloading from source.*${S2}" 30 \
  || { run_on "$SMOKE_HANDLE_3" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'image (not cached|cache hit)' | tail -5 || true; \
       fail "$NODE3 did not log a source download for S2 - the no-peer fallback should have fetched from the URL"; }
pass "$NODE3 logged 'image not cached, downloading from source' for S2 (fail-open: no peer -> source download -> boot)"

# S2 is a pinned image too, so it also stays out of the durability map.
placement_empty "$S2" || { echo "--- placement/$S2 keys ---"; etcd_keys "/otherix/placement/$S2/"; \
  fail "placement/$S2 is NOT empty - the fail-open pinned image leaked into the durability map"; }
pass "placement/S2 is EMPTY (the fail-open pinned image also stays out of the durability map)"

# --- cleanup -----------------------------------------------------------
echo "=== cleanup ==="
otx vm delete "$VM3" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM2" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM1" --wait --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== image-cache smoke PASSED ===${NC}"
echo "  pinned digest S=$S"
echo "  $NODE1 downloaded S from source, holds it in image_blobs, placement/S EMPTY"
echo "  $NODE2 peer-pulled S from $NODE1 (cache hit, no re-download), now also holds it"
echo "  placement/S stayed EMPTY across reconcile ticks; both holders kept S (never reclaimed)"
echo "  $NODE3 fail-open: distinct S2 with no peer holder -> source download -> booted"
echo "PASS"
