#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Image-cache eviction smoke: the node-level pinned-image cache store is bounded
# by artifacts.image_cache.max_bytes. When the store exceeds the ceiling, a
# periodic + nudge-triggered sweeper evicts the COLDEST cached images first (LRU
# by file mtime). It deletes ONLY image-store blobs, never an image being cloned
# and never anything else. Because a VM disk is a FULL byte copy of the image
# (not a backing-file clone), a running VM is unaffected when its source image is
# evicted. Driven end to end through the otherix CLI against the real three-node
# agents (qemu + QMP + the image cache tier + the eviction sweeper).
#
# The proof, on a SINGLE node so LRU is unambiguous:
#   1. SIZE the dev ceiling. max_bytes is set in dev/config/agent-macos.yaml to
#      650 MiB, between the two test cloud images: the ~586 MiB server image fits
#      alone, but the ~217 MiB minimal image PLUS the server image do not. So
#      caching the second image must evict the first.
#   2. Create VM-A from image A (the minimal cloudimg, digest SA). A is created
#      FIRST, so its cached blob has the OLDER mtime and is the eviction victim.
#      Assert: the node holds SA in its image cache tier.
#   3. Create VM-B from image B (the server cloudimg, digest SB) on the SAME node.
#      B is created SECOND (warmer mtime) and its Put fires the eviction nudge.
#      Assert: the agent logs an "image cache eviction pass" with evicted>=1, the
#      node NO LONGER holds SA, and it STILL holds SB (the coldest went, the warm
#      one survived).
#   4. DECOUPLING (the key safety property): VM-A, created from the now-evicted
#      image A, is STILL RUNNING and healthy. Its disk is an independent full
#      copy, so evicting the source image cannot affect it.
#   5. RECOVERABLE: recreate VM-A2 from the SAME image A. No node holds SA after
#      the eviction, so the agent re-fetches it from the source URL and the VM
#      boots. Assert: the agent logs "image not cached, downloading from source"
#      for SA, and VM-A2 reaches running and boots.
#
# The serial device is /dev/console (the active serial console on the cloud
# image, arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM
# smokes.
#
# We read the embedded dev etcd member directly (127.0.0.1:2379, no TLS) for the
# image_blobs inventory keys, exactly like the image-cache peer-pull smoke, and
# we read the agent logs via journalctl on the node, like the chaos smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree with
# the small dev ceiling in place:
#   make build && make local-dev-cleanrestart   (picks up the shrunk max_bytes)
# node-1 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-image-cache-eviction   (or: bash dev/smoke/image-cache-eviction/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE="node-1"                                 # all creates land here so LRU is single-node and unambiguous
VMA="${VMA:-img-evict-a}"                      # first create: image A (victim of eviction)
VMB="${VMB:-img-evict-b}"                      # second create: image B (survives; its Put nudges the sweeper)
VMA2="${VMA2:-img-evict-a2}"                   # recreate from image A after eviction (re-fetch from source)
# Image A is the SMALLER minimal cloudimg (~217 MiB), image B the LARGER server
# cloudimg (~586 MiB). The dev ceiling (650 MiB) is set so B fits alone but A+B
# does not, and evicting A alone (the coldest) restores the cap with B intact.
IMAGE_A="${IMAGE_A:-${SMOKE_IMAGE_URL}}"        # minimal cloudimg (host arch)
IMAGE_B="${IMAGE_B:-https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-${SMOKE_ARCH}.img}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-900}"              # seconds for a create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase
EVICT_WAIT="${EVICT_WAIT:-60}"               # seconds to wait for the eviction pass + inventory to drop SA
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"          # embedded etcd dev client endpoint (no TLS)

# This smoke runs entirely on the first node.
HANDLE="$SMOKE_HANDLE_1"
STATE="$SMOKE_STATE_1"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# image_blob_held NODE-UUID DIGEST -> 0 if that node's image_blobs inventory
# lists DIGEST, 1 otherwise. The image_blobs value is a JSON array of
# {digest,...}; the node uuid is the last path segment of the key.
image_blob_held() {
  local nid="$1" digest="$2"
  etcdctl --endpoints="$ETCD_EP" get "/otherix/image_blobs/$nid" --print-value-only 2>/dev/null \
    | jq -e --arg d "$digest" 'any(.[]?; .digest == $d)' >/dev/null 2>&1
}

# wait_blob_held NODE-UUID DIGEST TIMEOUT -> block until the node's image_blobs
# inventory lists DIGEST; non-zero on timeout.
wait_blob_held() {
  local nid="$1" digest="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do image_blob_held "$nid" "$digest" && return 0; sleep 3; done
  return 1
}

# wait_blob_gone NODE-UUID DIGEST TIMEOUT -> block until the node's image_blobs
# inventory NO LONGER lists DIGEST; non-zero on timeout.
wait_blob_gone() {
  local nid="$1" digest="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do image_blob_held "$nid" "$digest" || return 0; sleep 3; done
  return 1
}

# node_id NAME -> the node's uuid ("" on error)
node_id() { otx node get "$1" --output json 2>/dev/null | jq -r '.id // empty' 2>/dev/null || true; }

# agent_log_count PATTERN -> count of agent journal lines matching PATTERN on the
# node over the smoke window. Mirrors the chaos smokes' journalctl read.
agent_log_count() {
  local pat="$1" n
  n="$(run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null \
        | grep -cE "$pat" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_agent_log PATTERN TIMEOUT -> block until PATTERN appears in the agent
# journal on the node; non-zero on timeout.
wait_agent_log() {
  local pat="$1" to="$2" deadline; deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    (( $(agent_log_count "$pat") > 0 )) && return 0
    sleep 3
  done
  return 1
}

# image_cache_root -> the node's pinned-image cache blobs dir (sibling of the
# artifacts root). The dev artifacts root defaults to /var/lib/otherix/artifacts,
# so the image store is /var/lib/otherix/images.
IMAGE_BLOBS_DIR="/var/lib/otherix/images/blobs"

# reset_cached_image DIGEST -> delete a leftover cached blob (+ sidecar) for
# DIGEST from the node's image store, so a re-run starts from a clean cache and
# the LRU ordering this smoke establishes is the only thing in play. The next
# heartbeat re-reports the inventory without it.
reset_cached_image() {
  local digest="$1"
  run_on "$HANDLE" sudo rm -f "${IMAGE_BLOBS_DIR}/${digest}" "${IMAGE_BLOBS_DIR}/${digest}.sha256" 2>/dev/null || true
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

# serial_count VMID PATTERN -> count of lines matching PATTERN in the VM's
# serial.log on the node (0 when the file is absent).
serial_count() {
  local id="$1" pat="$2" n
  n="$(run_on "$HANDLE" sudo grep -c "$pat" "${STATE}/vms/${id}/serial.log" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_serial VMID PATTERN TIMEOUT -> block until PATTERN appears in the VM's
# serial.log on the node; non-zero on timeout.
wait_serial() {
  local id="$1" pat="$2" to="$3" deadline; deadline=$(( SECONDS + to ))
  info "watching ${STATE}/vms/${id}/serial.log for '$pat' (<= ${to}s)"
  while (( SECONDS < deadline )); do
    (( $(serial_count "$id" "$pat") > 0 )) && return 0
    sleep 5
  done
  return 1
}

# --- the cloud-config (always-on serial boot echo) ---------------------
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot, so each create's boot can be
# confirmed off the node's serial.log.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-imgevict-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix image-cache-eviction-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_IMGEVICT_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-imgevict-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VMA2" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VMB" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VMA" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== image-cache-eviction smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (it reads the embedded etcd image_blobs keys)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 || fail "etcd not reachable at $ETCD_EP"

st="$(otx node get "$NODE" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE not ready (got '${st:-none}'); run make local-dev-cleanrestart for a three-node stack"

# default DISK pool reconciled on the node this smoke creates VMs on.
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready "$NODE" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default disk pool not ready on $NODE within 60s (CP auto-provision)"

NID="$(node_id "$NODE")"
[[ "$NID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $NODE uuid (got '$NID')"
pass "CP up (${CP_VERSION}); $NODE ready; default disk pool ready"

# The journal read window starts now, so a stale eviction/download from a
# previous run can never satisfy an assertion in this run.
JOURNAL_SINCE="$(date '+%Y-%m-%d %H:%M:%S')"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: resolve the two distinct pinned digests -------------------
echo "=== step 1: resolve the two pinned image digests (A=victim, B=survivor) ==="
info "fetching image A ($IMAGE_A) to compute its sha256"
SA="$(curl -fsSL "$IMAGE_A" | sha256sum | awk '{print $1}')"
[[ "$SA" =~ ^[0-9a-f]{64}$ ]] || fail "could not compute sha256 of image A (got '$SA')"
info "fetching image B ($IMAGE_B) to compute its sha256"
SB="$(curl -fsSL "$IMAGE_B" | sha256sum | awk '{print $1}')"
[[ "$SB" =~ ^[0-9a-f]{64}$ ]] || fail "could not compute sha256 of image B (got '$SB')"
[[ "$SA" != "$SB" ]] || fail "image A and image B must be distinct images (got the same digest)"
pass "pinned digests: A(victim) SA=$SA  B(survivor) SB=$SB"

# Start from a clean cache: drop any leftover blobs for the two test digests
# (e.g. from a prior run) so the LRU ordering this smoke establishes is the only
# thing in play. The VMs were already deleted by the delete-first cleanup above.
info "clearing any leftover cached blobs for the two test digests on $NODE"
reset_cached_image "$SA"
reset_cached_image "$SB"
if ! wait_blob_gone "$NID" "$SA" 60; then fail "$NODE still reports SA in image_blobs after reset (a create may still be cloning it)"; fi
if ! wait_blob_gone "$NID" "$SB" 60; then fail "$NODE still reports SB in image_blobs after reset (a create may still be cloning it)"; fi
pass "$NODE image cache is clear of both test digests"

# --- step 2: create VM-A from image A (the eviction victim) ------------
echo "=== step 2: create $VMA on $NODE from image A pinned to SA -> running ==="
# A is created FIRST, so its cached blob gets the OLDER mtime: when the cache
# later exceeds the ceiling, A is the coldest and is evicted first.
printf '%s' "$CLOUD_INIT" | otx vm create "$VMA" \
  --image-url "$IMAGE_A" --image-sha256 "$SA" --arch "$ARCH" --node "$NODE" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VMA did not reach running within ${CREATE_WAIT}s"
assert_phase "$VMA" running
VMA_ID="$(vm_id "$VMA")"
[[ "$VMA_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VMA id (got '$VMA_ID')"
wait_serial "$VMA_ID" "OTHERIX_IMGEVICT_BOOT ${VMA}" "$GUEST_WAIT" \
  || { echo "--- $VMA serial tail ---"; run_on "$HANDLE" sudo tail -40 "${STATE}/vms/${VMA_ID}/serial.log" 2>/dev/null || true; \
       fail "$VMA did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$VMA created, running, and booted on $NODE"

# The node now caches image A.
wait_blob_held "$NID" "$SA" 60 \
  || fail "$NODE does not list SA in image_blobs - image A was not cached"
pass "$NODE holds SA (image A) in the image cache tier"

# --- step 3: create VM-B from image B -> caching B evicts cold A --------
echo "=== step 3: create $VMB on $NODE from image B pinned to SB -> caching B evicts the cold A ==="
# B's Put pushes the store over the ceiling and fires the eviction nudge. The
# sweeper evicts the COLDEST cached image (A) and keeps the warm one (B).
printf '%s' "$CLOUD_INIT" | otx vm create "$VMB" \
  --image-url "$IMAGE_B" --image-sha256 "$SB" --arch "$ARCH" --node "$NODE" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VMB did not reach running within ${CREATE_WAIT}s"
assert_phase "$VMB" running
VMB_ID="$(vm_id "$VMB")"
[[ "$VMB_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VMB id (got '$VMB_ID')"
wait_serial "$VMB_ID" "OTHERIX_IMGEVICT_BOOT ${VMB}" "$GUEST_WAIT" \
  || { echo "--- $VMB serial tail ---"; run_on "$HANDLE" sudo tail -40 "${STATE}/vms/${VMB_ID}/serial.log" 2>/dev/null || true; \
       fail "$VMB did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$VMB created, running, and booted on $NODE"

# The node now caches image B.
wait_blob_held "$NID" "$SB" 60 \
  || fail "$NODE does not list SB in image_blobs - image B was not cached"
pass "$NODE holds SB (image B) in the image cache tier"

# --- step 4: assert the eviction pass evicted the coldest (A) -----------
echo "=== step 4: the eviction sweeper evicted the coldest image (A), kept the warm one (B) ==="
# The agent emits JSON logs, so the count field is "evicted":<n> (with the colon).
wait_agent_log 'image cache eviction pass.*"evicted":[1-9]' "$EVICT_WAIT" \
  || { run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'image cache eviction' | tail -5 || true; \
       fail "$NODE did not log an 'image cache eviction pass' with evicted>=1 within ${EVICT_WAIT}s"; }
# Surface the pass line for the operator-readable record.
run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null \
  | grep -E 'image cache eviction pass.*"evicted":[1-9]' | tail -1 || true
pass "$NODE logged an 'image cache eviction pass' with evicted>=1"

# SA (the cold image A) is GONE from the cache tier.
wait_blob_gone "$NID" "$SA" "$EVICT_WAIT" \
  || fail "$NODE still holds SA after the eviction pass - the cold image A was NOT evicted"
pass "SA (image A) is GONE from $NODE's image cache tier (the coldest was evicted)"

# SB (the warm image B) REMAINS.
image_blob_held "$NID" "$SB" \
  || fail "$NODE no longer holds SB after the eviction pass - the warm image B was wrongly evicted"
pass "SB (image B) REMAINS in $NODE's image cache tier (the warm one survived)"

# --- step 5: decoupling - VM-A is unaffected by losing its source image -
echo "=== step 5: DECOUPLING - $VMA still runs though image A was evicted ==="
# VM-A's disk is a full byte copy of image A, not a backing-file clone, so losing
# the cached source image cannot affect it.
assert_phase "$VMA" running
# A fresh boot echo confirms the guest is alive (reboot to force a new serial
# line so the assertion cannot be satisfied by the original boot marker alone).
echo "  rebooting $VMA to confirm it is healthy after its source image was evicted"
PRE="$(serial_count "$VMA_ID" "OTHERIX_IMGEVICT_BOOT ${VMA}")"
otx vm reboot "$VMA" --wait --wait-timeout "${PHASE_WAIT}s" >/dev/null 2>&1 || fail "$VMA reboot failed - guest is not healthy"
deadline=$(( SECONDS + GUEST_WAIT )); ok=0
while (( SECONDS < deadline )); do
  (( "$(serial_count "$VMA_ID" "OTHERIX_IMGEVICT_BOOT ${VMA}")" > PRE )) && { ok=1; break; }
  sleep 5
done
(( ok == 1 )) || fail "$VMA did not produce a fresh boot marker after reboot - guest unhealthy post-eviction"
assert_phase "$VMA" running
pass "$VMA still runs and reboots cleanly after image A was evicted (disk is an independent copy)"

# --- step 5b: free node capacity before the re-fetch -
# VM-A and VM-B have served their purpose (decoupling proven; the survivor image
# is asserted from the cache, not from a running VM). The node has a small fixed
# vCPU budget and each VM consumes part of it, so a third VM would not schedule
# until these are removed. Removing the VMs does not touch the image cache.
echo "=== step 5b: remove VM-A and VM-B to free $NODE for the re-fetch VM ==="
otx vm delete "$VMA" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VMB" --wait --force >/dev/null 2>&1 || true
pass "removed VM-A and VM-B; $NODE has capacity for the re-fetch VM"

# --- step 6: recoverable - recreate from the evicted image A re-fetches -
echo "=== step 6: RECOVERABLE - recreate $VMA2 from image A re-fetches it from source ==="
# No node holds SA after the eviction, so the broker finds no peer holder and the
# agent re-downloads image A from the source URL.
SINCE_RECREATE="$(date '+%Y-%m-%d %H:%M:%S')"
printf '%s' "$CLOUD_INIT" | otx vm create "$VMA2" \
  --image-url "$IMAGE_A" --image-sha256 "$SA" --arch "$ARCH" --node "$NODE" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VMA2 did not reach running within ${CREATE_WAIT}s (re-fetch of image A failed?)"
assert_phase "$VMA2" running
VMA2_ID="$(vm_id "$VMA2")"
[[ "$VMA2_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VMA2 id (got '$VMA2_ID')"
wait_serial "$VMA2_ID" "OTHERIX_IMGEVICT_BOOT ${VMA2}" "$GUEST_WAIT" \
  || { echo "--- $VMA2 serial tail ---"; run_on "$HANDLE" sudo tail -40 "${STATE}/vms/${VMA2_ID}/serial.log" 2>/dev/null || true; \
       fail "$VMA2 did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$VMA2 created, running, and booted on $NODE"

# The agent re-fetched image A from the source URL (no node held SA after the
# eviction, so there was no peer to pull from).
n="$(run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$SINCE_RECREATE" --no-pager 2>/dev/null \
      | grep -cE "image not cached, downloading from source.*${SA}" 2>/dev/null)" || true
[[ "$n" =~ ^[0-9]+$ ]] || n=0
(( n > 0 )) \
  || { run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$SINCE_RECREATE" --no-pager 2>/dev/null | grep -E 'image (not cached|cache hit)' | tail -5 || true; \
       fail "$NODE did not log a source download for SA on recreate - the evicted image was not re-fetched"; }
pass "$NODE logged 'image not cached, downloading from source' for SA (the evicted image was re-fetched and booted)"

# --- cleanup -----------------------------------------------------------
echo "=== cleanup ==="
otx vm delete "$VMA2" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VMB" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VMA" --wait --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== image-cache-eviction smoke PASSED ===${NC}"
echo "  pinned digests: A(victim) SA=$SA  B(survivor) SB=$SB"
echo "  caching B over the ceiling fired an eviction pass that evicted the coldest image A"
echo "  SA gone from the cache tier, SB still present"
echo "  $VMA stayed running and rebooted cleanly though its source image was evicted (independent disk copy)"
echo "  recreating from image A re-fetched it from source and booted"
echo "PASS"
