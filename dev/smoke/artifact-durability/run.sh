#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Artifact durability smoke: a snapshot's content-addressed blob is replicated to
# reach the artifact pool's replication factor, survives the loss of a holder
# node, and re-replicates onto a surviving node - driven through the otherix CLI
# as an operator would.
#
#   1. create an artifact pool with replication factor 2 (--members all)
#   2. vm create on node-1, lay down a write-once guest marker, snapshot into the pool
#   3. poll snapshot durability -> "durable" (the reconcile loop placed a 2nd replica)
#   4. find the 2nd holder node (the non-node-1 node holding the blob), stop its agent
#   5. poll durability -> leaves "durable" (the holder loss was observed)
#   6. poll durability -> "durable" again (re-replicated onto the third node)
#   7. recreate-from-snapshot on the surviving third node boots from the re-replicated blob
#
# The blob lives in the agent's content-addressed store at
# <state_root>/artifacts/blobs/<digest> (the default artifacts root sits under the
# agent state root). We read the snapshot's disk[0] digest from the CP projection
# and probe each candidate node's blob store to identify the 2nd holder.
#
# When the 2nd holder's agent is stopped the node goes unreachable and drops from
# the live-holder set. With a third eligible node available, the reconcile loop
# re-places the blob there: durability transiently LEAVES "durable" (one live
# holder, room to grow) and RETURNS to "durable" once the third node holds the
# blob. We assert that leave-then-return, then prove a fresh VM recreated on the
# survivor boots from the re-replicated blob.
#
# The serial device is /dev/console (the active serial console on the cloud image,
# arch-agnostic: amd64 ttyS0 / arm64 ttyAMA0), mirroring the other VM smokes.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1/node-2/node-3 ready with the cluster default disk pool reconciled.
#
# Usage: make smoke-artifact-durability   (or: bash dev/smoke/artifact-durability/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # producer node (create + snapshot pinned here)
SRC_VM="${SRC_VM:-dur-src}"                   # source VM on node-1; snapshotted
DST_VM="${DST_VM:-dur-dst}"                   # VM recreated from the snapshot on the survivor
POOL="${POOL:-dur-$(date +%s)}"               # this smoke's OWN artifact pool, K=2
SNAP_NAME="${SNAP_NAME:-s1}"                  # snapshot name, unique within SRC_VM
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
RECREATE_WAIT="${RECREATE_WAIT:-600}"         # seconds for vm create --from-snapshot -> running
GUEST_WAIT="${GUEST_WAIT:-720}"               # seconds to wait for a guest boot sentinel (TCG is slow)
SNAP_WAIT="${SNAP_WAIT:-600}"                 # seconds for the snapshot task -> ready
OP_WAIT="${OP_WAIT:-180}"                     # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
FIRST_DURABLE_WAIT="${FIRST_DURABLE_WAIT:-180}"  # seconds for the first replica placement (reconcile ~10s)
LOSS_WAIT="${LOSS_WAIT:-120}"                 # seconds for the holder loss to be observed (unreachable ~20s)
REREPLICATE_WAIT="${REREPLICATE_WAIT:-300}"   # seconds to re-replicate onto the survivor (unreachable + reconcile + pull + heartbeat)

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
wait_left_durable() {
  local id="$1" to="$2" deadline d o
  deadline=$(( SECONDS + to ))
  while (( SECONDS < deadline )); do
    d="$(snap_field "$id" '.durability')"; o="$(snap_field "$id" '.observed_replicas')"
    [[ "$d" != "durable" || "${o:-0}" -lt 2 ]] && { pass "holder loss observed (durability=$d observed=$o)"; return 0; }
    sleep 2
  done
  fail "durability stayed durable after holder loss within ${to}s"
}

# stop_agent IDX -> stop the agent on the node at handle/state index IDX
# (lima: a systemd unit; netns: the agent process inside the namespace).
stop_agent() {
  local idx="$1" h; h="$(smoke_handle "$idx")"
  case "${SMOKE_PLATFORM}" in
    lima)  run_on "$h" sudo systemctl stop otherix-agent ;;
    netns) run_on "$h" sudo pkill -f "otherix-agent.*node-${idx}" || true ;;
  esac
}

# start_agent IDX -> restart the agent on the node at handle/state index IDX, so
# the smoke leaves the stack in a usable state even after it downed a holder. On
# netns the per-node agent is brought back by the multi-node dev script.
start_agent() {
  local idx="$1" h; h="$(smoke_handle "$idx")"
  case "${SMOKE_PLATFORM}" in
    lima)  run_on "$h" sudo systemctl start otherix-agent || true ;;
    netns) sudo bash dev/scripts/linux-multinode.sh start "$idx" >/dev/null 2>&1 || true ;;
  esac
}

# --- the cloud-config (always-on serial echo so we can see the recreate boot) ---
# An always-on systemd oneshot echoes a marker plus the live hostname to
# /dev/console (arch-agnostic) on EVERY boot. The source step waits for the
# marker so the disk has post-boot state before we snapshot it; the recreate step
# asserts the restored guest reaches the marker, i.e. it actually boots off the
# re-replicated blob.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-dur-sentinel.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix artifact-durability-smoke boot echo
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sync; echo "OTHERIX_DUR_BOOT $(hostname)" > /dev/console'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-dur-sentinel.service ]
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

# HOLDER_IDX is set once we down a node; restart it on exit so the stack stays usable.
HOLDER_IDX=""

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$DST_VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$SRC_VM" --wait --force >/dev/null 2>&1 || true
  local sid; sid="$(snapshot_id || true)"
  [ -n "$sid" ] && otx snapshot delete "$sid" --wait >/dev/null 2>&1 || true
  otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
  # restart any agent this smoke downed, so the stack is left usable.
  [ -n "$HOLDER_IDX" ] && { info "restarting agent on node-$HOLDER_IDX"; start_agent "$HOLDER_IDX"; }
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== artifact-durability smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

# All three nodes must be ready: node-1 produces, one of node-2/node-3 becomes the
# 2nd holder, the remaining node is the re-replication survivor.
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
echo "=== step 1: artifact-pool create $POOL --replication-factor 2 --members all ==="
otx artifact-pool create "$POOL" --replication-factor 2 --members all \
  || fail "artifact-pool create $POOL failed"
otx artifact-pool get "$POOL" --output json >/dev/null \
  || fail "artifact-pool get $POOL failed after create"
pass "artifact-pool $POOL created (K=2)"

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
wait_serial "$SMOKE_HANDLE_1" "$SMOKE_STATE_1" "$SRC_ID" "OTHERIX_DUR_BOOT ${SRC_VM}" "$GUEST_WAIT" \
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
DIGEST="$(snap_field "$SNAP_ID" '.disks[0].sha256')"
[[ "$DIGEST" =~ ^[0-9a-f]{64}$ ]] || fail "could not resolve disk[0] sha256 for snapshot $SNAP_ID (got '$DIGEST')"
info "snapshot id=$SNAP_ID disk[0] digest=$DIGEST"
pass "snapshot $SNAP_NAME ready (id=$SNAP_ID) tagged into artifact pool $POOL"

# --- step 4: the reconcile loop places a 2nd replica -> durable --------
echo "=== step 4: poll snapshot durability -> durable (2nd replica placed) ==="
wait_durable "$SNAP_ID" "$FIRST_DURABLE_WAIT"
pass "snapshot reached durable (K=2 holders) - the reconcile loop placed a 2nd replica"

# --- step 5: identify the 2nd holder node ------------------------------
echo "=== step 5: find the 2nd holder node (non-node-1 node holding the blob) ==="
# The producer is node-1 (index 1). The 2nd replica landed on node-2 or node-3.
# Probe each candidate's content-addressed blob store for the digest.
HOLDER_IDX=""
deadline=$(( SECONDS + 60 ))
while (( SECONDS < deadline )) && [[ -z "$HOLDER_IDX" ]]; do
  for i in 2 3; do
    if run_on "$(smoke_handle "$i")" sudo test -f "$(smoke_state "$i")/artifacts/blobs/$DIGEST" 2>/dev/null; then
      HOLDER_IDX="$i"; break
    fi
  done
  [[ -z "$HOLDER_IDX" ]] && sleep 3
done
[[ -n "$HOLDER_IDX" ]] || fail "no 2nd holder found among node-2/node-3 for digest $DIGEST"
SURVIVOR_IDX=$([[ "$HOLDER_IDX" == "2" ]] && echo 3 || echo 2)
HOLDER_NODE="node-$HOLDER_IDX"; SURVIVOR_NODE="node-$SURVIVOR_IDX"
info "2nd holder is $HOLDER_NODE; survivor (re-replication target) is $SURVIVOR_NODE"
pass "2nd holder identified: $HOLDER_NODE holds the blob"

# --- step 6: stop the 2nd holder's agent -> holder loss ----------------
echo "=== step 6: stop the agent on $HOLDER_NODE (holder loss) ==="
stop_agent "$HOLDER_IDX"
# also wipe its artifact store so a return would force a real re-pull (defense in
# depth; the stop alone drops it from the live-holder set once it goes unreachable).
run_on "$(smoke_handle "$HOLDER_IDX")" sudo rm -rf "$(smoke_state "$HOLDER_IDX")/artifacts/blobs" 2>/dev/null || true
info "stopped agent on $HOLDER_NODE and wiped its blob store"
wait_left_durable "$SNAP_ID" "$LOSS_WAIT"
pass "holder loss observed: durability left durable after $HOLDER_NODE went down"

# --- step 7: re-replicate onto the survivor -> durable again -----------
echo "=== step 7: poll snapshot durability -> durable again (re-replicated onto $SURVIVOR_NODE) ==="
wait_durable "$SNAP_ID" "$REREPLICATE_WAIT"
# confirm the survivor now physically holds the blob.
deadline=$(( SECONDS + 60 )); held=0
while (( SECONDS < deadline )); do
  if run_on "$(smoke_handle "$SURVIVOR_IDX")" sudo test -f "$(smoke_state "$SURVIVOR_IDX")/artifacts/blobs/$DIGEST" 2>/dev/null; then
    held=1; break
  fi
  sleep 3
done
(( held == 1 )) || fail "survivor $SURVIVOR_NODE does not physically hold the blob after re-replication"
pass "re-replicated: $SURVIVOR_NODE now holds the blob and durability is durable again"

# --- step 8: recreate on the survivor boots from the re-replicated blob ----
echo "=== step 8: vm create $DST_VM --from-snapshot $SNAP_ID --node $SURVIVOR_NODE -> running ==="
otx vm create "$DST_VM" --from-snapshot "$SNAP_ID" --node "$SURVIVOR_NODE" \
  --vcpus 2 --memory-mb 2048 \
  --wait --wait-timeout "${RECREATE_WAIT}s" \
  || fail "vm create --from-snapshot (steered to $SURVIVOR_NODE) did not reach running within ${RECREATE_WAIT}s"
assert_phase "$DST_VM" running
DST_ID="$(otx vm get "$DST_VM" --output json | jq -r '.id')"
[[ "$DST_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $DST_VM id (got '$DST_ID')"
info "$DST_VM id=$DST_ID"
wait_serial "$(smoke_handle "$SURVIVOR_IDX")" "$(smoke_state "$SURVIVOR_IDX")" "$DST_ID" "OTHERIX_DUR_BOOT ${DST_VM}" "$GUEST_WAIT" \
  || { echo "--- $DST_VM serial tail ($SURVIVOR_NODE) ---"; run_on "$(smoke_handle "$SURVIVOR_IDX")" sudo tail -60 "$(smoke_state "$SURVIVOR_IDX")/vms/${DST_ID}/serial.log" 2>/dev/null || true; \
       fail "restored VM $DST_VM did not boot from the re-replicated blob on $SURVIVOR_NODE within ${GUEST_WAIT}s"; }
pass "$DST_VM recreated on $SURVIVOR_NODE and booted from the re-replicated blob"

# --- step 9: cleanup ---------------------------------------------------
echo "=== step 9: cleanup ==="
otx vm delete "$DST_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
assert_gone "$DST_VM" || true
otx vm delete "$SRC_VM" --wait --force --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx snapshot delete "$SNAP_ID" --wait --wait-timeout "${OP_WAIT}s" >/dev/null 2>&1 || true
otx artifact-pool delete "$POOL" --force >/dev/null 2>&1 || true
# restart the downed holder agent so the stack is left usable.
info "restarting agent on $HOLDER_NODE"
start_agent "$HOLDER_IDX"
pass "best-effort cleanup done; $HOLDER_NODE agent restarted"

trap - EXIT
echo
echo "${GREEN}=== artifact-durability smoke PASSED ===${NC}"
echo "  2nd holder: $HOLDER_NODE   survivor (re-replica): $SURVIVOR_NODE"
echo "  durable -> holder loss observed -> durable again -> recreate booted on the survivor"
echo "PASS"
