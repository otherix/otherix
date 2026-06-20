#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Node-cache-hygiene smoke: three node-level cache-hygiene behaviours that keep a
# node's image caches bounded and tidy without ever touching a running VM or a
# referenced blob, proven end to end against REAL agents on a three-node stack and
# driven through the otherix CLI as an operator would.
#
# Three independent behaviours are exercised:
#
#   1. WITHIN-NODE IMPORT COALESCING. A batch of concurrent VM creates from the
#      SAME new UNPINNED image URL (--image-url U, NO --image-sha256) landing on
#      ONE node must download the image exactly ONCE on that node, not once per
#      VM: the concurrent imports of the same URL coalesce behind a single
#      download (the digest is unknown until the bytes arrive, so the per-digest
#      cache lock cannot dedupe the fetch; a per-URL coalescing group does). All
#      creates are pinned to node-1 with --node, so the single-node landing is
#      deterministic. Evidence: node-1's agent journal logs the line
#      "unpinned image not cached, downloading from source" for U EXACTLY ONCE for
#      the whole batch, while every VM in the batch still reaches running.
#
#   2. LEGACY BASENAME-CACHE SWEEP. The per-pool legacy basename image cache at
#      {poolRoot}/images/ is no longer written to (unpinned images now live in the
#      node-level digest image store), so any file left there is a frozen leftover.
#      On the pool's first registration after an agent restart the agent sweeps the
#      directory's regular files. This smoke plants a stray file under a node's
#      {poolRoot}/images/, restarts the agents, and asserts the file is gone while
#      the images/ directory itself survives (the sweep clears files, never the
#      directory).
#
#   3. IMAGE-STORE HYGIENE. The node image store keeps interrupted-upload temp
#      files under blobs/.staging. On agent start the artifact sweeper's boot pass
#      sweep(ctx, 0) clears ALL staging in both the artifact store and the image
#      store regardless of age. This smoke plants a stray .staging orphan in the
#      node image store's staging dir, restarts the agents, and asserts the orphan
#      is gone.
#
# Filesystem layout (read/written directly on the node, since an operator cannot
# inspect these node-local paths through the CLI):
#   - legacy basename cache:   <state_root>/pools/<pool>/images/
#   - node image store staging: <state_root>/images/blobs/.staging/
# The default disk pool name is "default" and its root is <state_root>/pools/default.
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-node-cache-hygiene   (or: bash dev/smoke/node-cache-hygiene/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE="node-1"                                 # the batch is pinned here so the landing is single-node and unambiguous
POOL="${POOL:-default}"                        # the cluster default disk pool (its root holds the legacy images/ cache)
BATCH=3                                        # number of concurrent creates from the SAME unpinned URL
VM_PREFIX="${VM_PREFIX:-nch-coalesce}"        # batch VM name prefix; per-VM name is <prefix>-<i>
# A NEW unpinned image URL the cluster has not seen, so the first import is a
# genuine source download (not a peer pull or a cache hit). The minimal cloudimg
# carries a query-string cache-buster so the URL is fresh per run while the bytes
# stay a valid qcow2; the agent keys the coalescing group on the exact URL string.
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}?nch=$(date +%s)}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-900}"             # seconds for a create -> running (incl. one cold image fetch shared by the batch)
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase
DOWNLOAD_WAIT="${DOWNLOAD_WAIT:-60}"          # seconds for the download log line to appear after the batch settles
READY_WAIT="${READY_WAIT:-120}"              # seconds for nodes to return to ready after the restart
SWEEP_WAIT="${SWEEP_WAIT:-90}"               # seconds for the post-restart sweeps to clear the planted probes

# This smoke creates and inspects entirely on the first node.
HANDLE="$SMOKE_HANDLE_1"
STATE="$SMOKE_STATE_1"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# vm_name I -> the batch VM name for index I
vm_name() { printf '%s-%d' "$VM_PREFIX" "$1"; }

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

# agent_log_count MSG SUBSTR -> count of agent journal lines on the node over the
# smoke window that contain BOTH the fixed message MSG and the fixed substring
# SUBSTR. Fixed-string matching (grep -F) so an image URL's regex metacharacters
# (the '?' query separator and '.' in the host/path) are matched literally rather
# than interpreted as a pattern.
agent_log_count() {
  local msg="$1" sub="$2" n
  n="$(run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null \
        | grep -F "$msg" | grep -cF "$sub" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

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

# wait_nodes_ready TIMEOUT -> block until all three nodes report ready AND the
# default disk pool is reconciled on the create node; fail on timeout.
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
wait_nodes_ready() {
  local to="$1" n deadline rdy ok
  for n in node-1 node-2 node-3; do
    deadline=$(( SECONDS + to )); rdy=0
    while (( SECONDS < deadline )); do
      [[ "$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)" == "ready" ]] && { rdy=1; break; }
      sleep 3
    done
    (( rdy == 1 )) || fail "$n did not reach ready within ${to}s"
  done
  deadline=$(( SECONDS + to )); ok=0
  while (( SECONDS < deadline )); do pool_ready "$NODE" && { ok=1; break; }; sleep 3; done
  (( ok == 1 )) || fail "default disk pool not ready on $NODE within ${to}s"
}

# --- the cloud-config (always-on serial boot echo) ---------------------
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot, so each batch create's boot can be
# confirmed off the node's serial.log.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-nch-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix node-cache-hygiene-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_NCH_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-nch-sentinel.service ]
  - [ sync ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- planted-probe paths (cleaned up on exit) --------------------------
LEGACY_DIR="${STATE}/pools/${POOL}/images"                 # legacy basename cache for the default pool
LEGACY_PROBE="${LEGACY_DIR}/zz-legacy-sweep-probe.img"      # stray file the basename sweep must remove
STAGING_DIR="${STATE}/images/blobs/.staging"               # node image store staging dir
STAGING_PROBE="${STAGING_DIR}/zz-orphan-probe.tmp"          # stray .staging orphan the boot sweep must remove

cleanup() {
  echo "--- cleanup ---"
  local i
  for (( i = 1; i <= BATCH; i++ )); do
    otx vm delete "$(vm_name "$i")" --wait --force >/dev/null 2>&1 || true
  done
  # Remove any planted probes that survived (e.g. on an early failure before the
  # restart swept them). Best-effort; never recurses, never removes a directory.
  run_on "$HANDLE" sudo rm -f "$LEGACY_PROBE" "$STAGING_PROBE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== node-cache-hygiene smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"

# All three nodes must be ready (the restart bounces every agent, and the batch
# pins to node-1; the other two must come back up for the stack to be healthy).
for n in node-1 node-2 node-3; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start for a three-node stack"
done

# default DISK pool reconciled on node-1 (the batch VMs resolve to it, and its
# root is where the legacy basename cache lives).
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready "$NODE" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default disk pool not ready on $NODE within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); node-1/node-2/node-3 ready; default disk pool ready on $NODE"

# The journal read window starts now, so a stale download from a previous run can
# never satisfy the coalescing assertion in this run.
JOURNAL_SINCE="$(date '+%Y-%m-%d %H:%M:%S')"

cleanup >/dev/null 2>&1 || true   # best-effort delete-first of stale leftovers

# --- step 1: within-node import coalescing -----------------------------
echo "=== step 1: $BATCH concurrent creates from one NEW unpinned URL on $NODE -> ONE download ==="
info "unpinned image URL U=$IMAGE_URL"
# Launch the batch concurrently: each create blocks on --wait, so we background
# them and join. They share the SAME unpinned URL and land on the SAME node, so
# their imports must coalesce behind a single download.
pids=()
for (( i = 1; i <= BATCH; i++ )); do
  name="$(vm_name "$i")"
  ( printf '%s' "$CLOUD_INIT" | otx vm create "$name" \
      --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE" \
      --vcpus 1 --memory-mb 1024 --user-data - \
      --wait --wait-timeout "${CREATE_WAIT}s" ) &
  pids+=( "$!" )
done
info "launched $BATCH concurrent creates (${VM_PREFIX}-1..${VM_PREFIX}-$BATCH); waiting for all to settle"
batch_ok=1
for p in "${pids[@]}"; do wait "$p" || batch_ok=0; done
(( batch_ok == 1 )) || fail "at least one batch create did not reach running within ${CREATE_WAIT}s"

# Every VM in the batch must be running and have booted.
for (( i = 1; i <= BATCH; i++ )); do
  name="$(vm_name "$i")"
  assert_phase "$name" running
  id="$(vm_id "$name")"
  [[ "$id" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $name id (got '$id')"
  wait_serial "$id" "OTHERIX_NCH_BOOT" "$GUEST_WAIT" \
    || { echo "--- $name serial tail ---"; run_on "$HANDLE" sudo tail -40 "${STATE}/vms/${id}/serial.log" 2>/dev/null || true; \
         fail "$name did not reach the boot marker within ${GUEST_WAIT}s"; }
done
pass "all $BATCH batch VMs created, running, and booted on $NODE"

# The owning node downloaded the unpinned image EXACTLY ONCE for the whole batch.
# The download log line is emitted only by the coalescing leader; the followers
# clone from the cache the leader populated. Give the line a moment to flush, then
# count it.
DL_MSG="unpinned image not cached, downloading from source"
deadline=$(( SECONDS + DOWNLOAD_WAIT )); dl=0
while (( SECONDS < deadline )); do
  dl="$(agent_log_count "$DL_MSG" "$IMAGE_URL")"
  (( dl >= 1 )) && break
  sleep 3
done
(( dl >= 1 )) || { run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -E 'unpinned image|image (not cached|cache hit)' | tail -5 || true; \
  fail "$NODE never logged an unpinned source download for U - the batch never fetched the image"; }
(( dl == 1 )) || { run_on "$HANDLE" sudo journalctl -u otherix-agent --since "$JOURNAL_SINCE" --no-pager 2>/dev/null | grep -F "$IMAGE_URL" | tail -10 || true; \
  fail "$NODE logged $dl unpinned source downloads for U - $BATCH concurrent creates did NOT coalesce into one download"; }
pass "$NODE downloaded U exactly ONCE for $BATCH concurrent creates (within-node import coalescing)"

# --- step 2: plant the legacy basename-cache probe + the image-store staging orphan ---
echo "=== step 2: plant the legacy basename-cache file and the image-store staging orphan on $NODE ==="
# (a) a stray file in the pool's legacy basename image cache directory. The
#     directory exists once the pool is reconciled; ensure it, then plant.
run_on "$HANDLE" sudo install -d -m 0750 "$LEGACY_DIR" \
  || fail "could not ensure legacy images dir $LEGACY_DIR on $NODE"
run_on "$HANDLE" sudo sh -c "printf 'stale-legacy-image-bytes' > '$LEGACY_PROBE'" \
  || fail "could not plant legacy cache probe $LEGACY_PROBE on $NODE"
run_on "$HANDLE" sudo test -f "$LEGACY_PROBE" \
  || fail "planted legacy cache probe $LEGACY_PROBE not present after write on $NODE"
info "planted legacy basename-cache probe $LEGACY_PROBE"

# (b) a stray .staging orphan in the node image store staging dir.
run_on "$HANDLE" sudo install -d -m 0750 "$STAGING_DIR" \
  || fail "could not ensure image store staging dir $STAGING_DIR on $NODE"
run_on "$HANDLE" sudo sh -c "printf 'partial-image-upload-bytes' > '$STAGING_PROBE'" \
  || fail "could not plant image store staging orphan $STAGING_PROBE on $NODE"
run_on "$HANDLE" sudo test -f "$STAGING_PROBE" \
  || fail "planted image store staging orphan $STAGING_PROBE not present after write on $NODE"
info "planted image store staging orphan $STAGING_PROBE"
pass "both probes planted on $NODE (legacy basename cache file + image store staging orphan)"

# --- step 3: restart the agents -> the sweeps fire on agent start ------
echo "=== step 3: restart api + agents (an operator restart triggers the on-start sweeps) ==="
# The legacy basename sweep fires on the pool's first registration after restart;
# the image-store staging sweep fires on the artifact sweeper's boot pass. Bounce
# api + agents in place; state is preserved.
make local-dev-restart || fail "make local-dev-restart failed"
wait_nodes_ready "$READY_WAIT"
pass "node-1/node-2/node-3 ready again after the restart"

# --- step 4: assert the legacy basename file was swept (dir survives) --
echo "=== step 4: the legacy basename-cache file is swept, the images/ directory survives ==="
deadline=$(( SECONDS + SWEEP_WAIT )); swept=0
while (( SECONDS < deadline )); do
  run_on "$HANDLE" sudo test -f "$LEGACY_PROBE" 2>/dev/null || { swept=1; break; }
  sleep 3
done
(( swept == 1 )) \
  || fail "legacy basename-cache probe $LEGACY_PROBE survived the post-restart sweep on $NODE"
# The directory itself must remain (the sweep clears files, never the directory).
run_on "$HANDLE" sudo test -d "$LEGACY_DIR" 2>/dev/null \
  || fail "legacy images directory $LEGACY_DIR was removed by the sweep on $NODE (it must survive)"
pass "legacy basename-cache file swept on $NODE; the images/ directory survived"

# --- step 5: assert the image-store staging orphan was swept -----------
echo "=== step 5: the image-store staging orphan is swept by the boot pass ==="
deadline=$(( SECONDS + SWEEP_WAIT )); swept=0
while (( SECONDS < deadline )); do
  run_on "$HANDLE" sudo test -f "$STAGING_PROBE" 2>/dev/null || { swept=1; break; }
  sleep 3
done
(( swept == 1 )) \
  || fail "image store staging orphan $STAGING_PROBE survived the boot sweep on $NODE"
pass "image store staging orphan swept on $NODE (the artifact sweeper boot pass cleared it)"

# --- step 6: cleanup ---------------------------------------------------
echo "=== step 6: cleanup ==="
for (( i = 1; i <= BATCH; i++ )); do
  otx vm delete "$(vm_name "$i")" --wait --force >/dev/null 2>&1 || true
done
run_on "$HANDLE" sudo rm -f "$LEGACY_PROBE" "$STAGING_PROBE" >/dev/null 2>&1 || true
pass "best-effort cleanup done"

trap - EXIT
echo
echo "${GREEN}=== node-cache-hygiene smoke PASSED ===${NC}"
echo "  $BATCH concurrent creates from one new unpinned URL on $NODE downloaded the image ONCE (coalesced)"
echo "  a stray file in the pool's legacy basename image cache was swept on agent restart (images/ dir survived)"
echo "  a stray .staging orphan in the node image store was swept by the artifact sweeper boot pass"
echo "PASS"
