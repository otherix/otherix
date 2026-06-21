#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Unpinned peer-pull and pull-policy smoke: a VM created from an UNPINNED image
# URL (--image-url U, NO --image-sha256) is downloaded once, its URL-to-digest
# mapping is registered cluster-wide, and the cached image is then served to
# other nodes by a peer-to-peer pull instead of re-downloading from the source
# URL. The pull policy is the operator's escape hatch: --pull-policy always
# forces a fresh download from U even when peers already hold the image (because
# an unpinned URL is mutable and its bytes may have changed). Driven end to end
# through the otherix CLI against the real three-node agents (qemu + QMP + the
# image cache tier + the CP-brokered peer pull + the URL-to-digest registry).
#
# The proof, one VM per node so each holds at most one image:
#   1. RESOLVE the digest. The image is unpinned, but its content is fixed at a
#      moment in time, so fetch U once and compute its sha256 D for the
#      assertions (we never pass D as a pin; the agent resolves it on its own).
#   2. FIRST create on node-1 from the UNPINNED URL (if-not-present default). No
#      node holds the image and the registry is empty, so node-1 DOWNLOADS from
#      the source URL. Assert: node-1 logs "unpinned image not cached,
#      downloading from source" for U; the CP registered U -> D in the
#      image_url_digests registry; node-1 holds D in its image_blobs cache tier.
#   3. SECOND create on node-2 from the SAME unpinned URL (if-not-present). The
#      CP resolves the registry to D, finds node-1 holds it, and brokers a peer
#      pull node-2 <- node-1 BEFORE materialize. The agent then clones from the
#      staged cache, never re-downloading from U. Assert: node-1 SERVED it
#      ("blob serve listener opened" for D); node-2 did NOT log a source download
#      for U (it peer-pulled); node-2 now also holds D; the VM boots.
#   4. THIRD create on node-3 from the SAME unpinned URL with --pull-policy
#      always. The CP skips the registry and the hint, so the agent FORCE
#      DOWNLOADS from U even though node-1 and node-2 already hold D. Assert:
#      node-3 logs "unpinned image not cached, downloading from source" for U
#      despite the peer holders; the VM boots.
#   5. MANIFEST round-trip. `otherix vm get <always-vm> -o yaml` surfaces
#      imagePullPolicy: always, so the policy survives a create -> get round
#      trip and an operator can re-apply it from the manifest.
#
# The serial device is /dev/console (the active serial console on the cloud
# image, arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM
# smokes.
#
# We read the embedded dev etcd member directly (127.0.0.1:2379, no TLS) for the
# image_blobs inventory keys and the image_url_digests registry, exactly like
# the image-cache peer-pull smoke, and we read the agent logs via journalctl on
# each node, like the chaos smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start   (or: make local-dev-cleanrestart)
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-unpinned-peer-pull   (or: bash dev/smoke/unpinned-peer-pull/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # downloads U from source (first unpinned create)
NODE2="node-2"                                # peer-pull consumer (clones from node-1's cache)
NODE3="node-3"                                # --pull-policy always: force-downloads despite peers
VM1="${VM1:-unpin-1}"                         # first create on node-1 (source download of U)
VM2="${VM2:-unpin-2}"                         # second create on node-2 (peer pull, no download)
VM3="${VM3:-unpin-3}"                         # third create on node-3 (--pull-policy always)
# The UNPINNED image URL: passed as --image-url with NO --image-sha256, so the
# agent computes the digest itself and the CP registers the URL -> digest map.
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
# The second create blocks THROUGH the peer pull; give it room for one or two 5s
# heartbeats (holder inventory propagation) + the blob transfer + a boot.
PULL_WAIT="${PULL_WAIT:-600}"                 # seconds for the peer-pull create -> running (cross-node pull)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase
ETCD_EP="${ETCD_EP:-127.0.0.1:2379}"          # embedded etcd dev client endpoint (no TLS)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# etcd_keys PREFIX -> keys under PREFIX, one per line ("" when none)
etcd_keys() { etcdctl --endpoints="$ETCD_EP" get "$1" --prefix --keys-only 2>/dev/null | sed '/^$/d' || true; }

# url_digest_key URL -> the image_url_digests etcd key for URL. The key is
# /otherix/image_url_digests/<sha256hex(URL)> (the URL is hashed into a safe,
# fixed-width key), mirroring imageURLDigestKey in the store.
url_digest_key() {
  local sum
  sum="$(printf '%s' "$1" | sha256sum | awk '{print $1}')"
  printf '/otherix/image_url_digests/%s' "$sum"
}

# registry_digest URL -> the digest the CP registered for URL ("" if unregistered)
registry_digest() {
  etcdctl --endpoints="$ETCD_EP" get "$(url_digest_key "$1")" --print-value-only 2>/dev/null \
    | jq -r '.digest // empty' 2>/dev/null || true
}

# image_blob_held NODE-UUID DIGEST -> 0 if that node's image_blobs inventory lists
# DIGEST, 1 otherwise. The image_blobs value is a JSON array of {digest,...}; the
# node uuid is the LAST path segment of the key.
image_blob_held() {
  local nid="$1" digest="$2"
  etcdctl --endpoints="$ETCD_EP" get "/otherix/image_blobs/$nid" --print-value-only 2>/dev/null \
    | jq -e --arg d "$digest" 'any(.[]?; .digest == $d)' >/dev/null 2>&1
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
  - path: /etc/systemd/system/otherix-unpin-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix unpinned-peer-pull-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_UNPIN_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-unpin-sentinel.service ]
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
echo "=== unpinned-peer-pull smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v etcdctl >/dev/null || fail "etcdctl is required (it reads the embedded etcd image_blobs / image_url_digests keys)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
etcdctl --endpoints="$ETCD_EP" endpoint health >/dev/null 2>&1 || fail "etcd not reachable at $ETCD_EP"

# All three nodes ready: node-1 downloads, node-2 peer-pulls, node-3 force-downloads.
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

# --- step 1: resolve the digest D the agent will compute for U ----------
echo "=== step 1: resolve the content digest D for the unpinned image U ==="
info "fetching $IMAGE_URL to compute its sha256 D (for the assertions; NOT passed as a pin)"
D="$(curl -fsSL "$IMAGE_URL" | sha256sum | awk '{print $1}')"
[[ "$D" =~ ^[0-9a-f]{64}$ ]] || fail "could not compute sha256 of $IMAGE_URL (got '$D')"
pass "expected content digest D=$D"

# --- step 2: FIRST create on node-1 -> source download (registers U -> D) -
echo "=== step 2: create $VM1 on $NODE1 from UNPINNED $IMAGE_URL -> running (source download) ==="
# UNPINNED: no --image-sha256. node-1 holds nothing and the registry is empty,
# so the agent downloads from the source URL and the CP registers U -> D on
# success, seeding the peer-pull path for the second node.
printf '%s' "$CLOUD_INIT" | otx vm create "$VM1" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM1 did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM1" running
VM1_ID="$(vm_id "$VM1")"
[[ "$VM1_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM1 id (got '$VM1_ID')"
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$VM1_ID" "OTHERIX_UNPIN_BOOT ${VM1}" "$GUEST_WAIT" \
  || { echo "--- $VM1 serial tail ($NODE1) ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "${SMOKE_STATE_1}/vms/${VM1_ID}/serial.log" 2>/dev/null || true; \
       fail "$VM1 did not reach the boot marker within ${GUEST_WAIT}s"; }
pass "$VM1 created, running, and booted on $NODE1"

# node-1 downloaded the unpinned image from source (registry was empty, no peer held it).
wait_agent_log "$SMOKE_HANDLE_1" "unpinned image not cached, downloading from source.*${IMAGE_URL}" 30 \
  || { run_on "$SMOKE_HANDLE_1" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'unpinned image|image (not cached|cache hit)' | tail -5 || true; \
       fail "$NODE1 did not log an unpinned source download for U - the first create should have fetched from the URL"; }
pass "$NODE1 logged 'unpinned image not cached, downloading from source' for U (first holder fetched from source)"

# The CP registered U -> D in the URL-to-digest registry.
deadline=$(( SECONDS + 60 )); RD=""
while (( SECONDS < deadline )); do RD="$(registry_digest "$IMAGE_URL")"; [[ -n "$RD" ]] && break; sleep 3; done
[[ "$RD" == "$D" ]] || fail "CP registry maps U to '${RD:-none}', want D=$D (the URL -> digest registration is missing or wrong)"
pass "CP registered U -> D in image_url_digests (registry digest matches the computed D)"

# node-1 holds D in the image cache tier.
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do image_blob_held "$NID1" "$D" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NODE1 does not list D in image_blobs - the unpinned image was not inventoried in the cache tier"
pass "$NODE1 holds D in the image_blobs cache tier"

# --- step 3: SECOND create on node-2 from the SAME URL -> peer pull ------
echo "=== step 3: create $VM2 on $NODE2 from the SAME UNPINNED $IMAGE_URL -> running (peer pull node-2 <- node-1) ==="
# if-not-present (default). node-2 holds nothing, but the CP resolves the
# registry to D, finds node-1 holds it, and brokers a peer pull into node-2's
# image cache tier BEFORE materialize. The agent then CLONES from the staged
# cache, never re-downloading from U.
printf '%s' "$CLOUD_INIT" | otx vm create "$VM2" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE2" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${PULL_WAIT}s" \
  || fail "vm create $VM2 did not reach running within ${PULL_WAIT}s (peer pull failed?)"
assert_phase "$VM2" running
VM2_ID="$(vm_id "$VM2")"
[[ "$VM2_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM2 id (got '$VM2_ID')"
wait_serial "$SMOKE_HANDLE_2" "$SMOKE_STATE_2" "$VM2_ID" "OTHERIX_UNPIN_BOOT ${VM2}" "$GUEST_WAIT" \
  || { echo "--- $VM2 serial tail ($NODE2) ---"; run_on "$SMOKE_HANDLE_2" sudo tail -40 "${SMOKE_STATE_2}/vms/${VM2_ID}/serial.log" 2>/dev/null || true; \
       fail "$VM2 did not reach the boot marker on $NODE2 within ${GUEST_WAIT}s"; }
pass "$VM2 created, running, and booted on $NODE2"

# node-1 SERVED the blob (the CP opened its peer serve listener for D).
wait_agent_log "$SMOKE_HANDLE_1" "blob serve listener opened.*${D}" 30 \
  || { run_on "$SMOKE_HANDLE_1" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'blob serve listener' | tail -5 || true; \
       fail "$NODE1 did not log a peer serve for D - the holder was not asked to serve the blob"; }
pass "$NODE1 logged 'blob serve listener opened' for D (holder served the peer pull)"

# node-2 must NOT have re-downloaded the unpinned image from the source URL.
dl2="$(agent_log_count "$SMOKE_HANDLE_2" "unpinned image not cached, downloading from source.*${IMAGE_URL}")"
(( dl2 == 0 )) || fail "$NODE2 logged $dl2 unpinned source download(s) for U - it re-downloaded instead of peer-pulling"
pass "$NODE2 did NOT log an unpinned source download for U (it peer-pulled, never re-fetched from the URL)"

# node-2 now also holds D in the image cache tier (the peer-pulled blob was inventoried).
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do image_blob_held "$NID2" "$D" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NODE2 does not list D in image_blobs - the peer-pulled image was not inventoried"
pass "$NODE2 holds D in the image_blobs cache tier (both $NODE1 and $NODE2 now hold it)"

# --- step 4: THIRD create on node-3 with --pull-policy always -----------
echo "=== step 4: create $VM3 on $NODE3 from the SAME URL with --pull-policy always -> force download (no peer pull) ==="
# Sanity: peers DO hold D right now, so if-not-present WOULD peer-pull. With
# --pull-policy always the CP skips the registry and the hint, so the agent must
# FORCE DOWNLOAD from U regardless - the escape hatch for a mutated URL.
image_blob_held "$NID1" "$D" || fail "$NODE1 unexpectedly stopped holding D before the always create - cannot prove always BYPASSES a live peer"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM3" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE3" --pull-policy always \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM3 did not reach running within ${CREATE_WAIT}s (force download failed?)"
assert_phase "$VM3" running
VM3_ID="$(vm_id "$VM3")"
[[ "$VM3_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM3 id (got '$VM3_ID')"
wait_serial "$SMOKE_HANDLE_3" "$SMOKE_STATE_3" "$VM3_ID" "OTHERIX_UNPIN_BOOT ${VM3}" "$GUEST_WAIT" \
  || { echo "--- $VM3 serial tail ($NODE3) ---"; run_on "$SMOKE_HANDLE_3" sudo tail -40 "${SMOKE_STATE_3}/vms/${VM3_ID}/serial.log" 2>/dev/null || true; \
       fail "$VM3 did not reach the boot marker on $NODE3 within ${GUEST_WAIT}s"; }
pass "$VM3 created, running, and booted on $NODE3"

# node-3 FORCE downloaded from source despite node-1 and node-2 holding D.
wait_agent_log "$SMOKE_HANDLE_3" "unpinned image not cached, downloading from source.*${IMAGE_URL}" 30 \
  || { run_on "$SMOKE_HANDLE_3" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'unpinned image|image (not cached|cache hit)' | tail -5 || true; \
       fail "$NODE3 did not log an unpinned source download for U - --pull-policy always should have force-fetched from the URL despite the peer holders"; }
pass "$NODE3 logged 'unpinned image not cached, downloading from source' for U (--pull-policy always force-fetched, ignoring the peer holders)"

# --- step 5: manifest round-trip of the pull policy --------------------
echo "=== step 5: manifest round-trip: 'vm get $VM3 -o yaml' surfaces imagePullPolicy: always ==="
YAML="$(otx vm get "$VM3" -o yaml 2>/dev/null)" || fail "could not 'vm get $VM3 -o yaml'"
echo "$YAML" | grep -Eq '^[[:space:]]*imagePullPolicy:[[:space:]]*always[[:space:]]*$' \
  || { echo "--- $VM3 manifest ---"; printf '%s\n' "$YAML"; \
       fail "'vm get $VM3 -o yaml' does not contain 'imagePullPolicy: always' - the pull policy did not round-trip"; }
pass "$VM3 manifest contains imagePullPolicy: always (the pull policy round-trips through create -> get)"

# --- cleanup -----------------------------------------------------------
echo "=== cleanup ==="
otx vm delete "$VM3" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM2" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM1" --wait --force >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== unpinned-peer-pull smoke PASSED ===${NC}"
echo "  content digest D=$D"
echo "  $NODE1 downloaded the unpinned U from source; CP registered U -> D; $NODE1 holds D"
echo "  $NODE2 peer-pulled D from $NODE1 (no re-download of U), now also holds D"
echo "  $NODE3 with --pull-policy always force-downloaded U despite $NODE1/$NODE2 holding D"
echo "  'vm get $VM3 -o yaml' round-trips imagePullPolicy: always"
echo "PASS"
