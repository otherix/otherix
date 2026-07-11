#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Live VM migration STATISTICS smoke - drives `otherix vm migrate` (LIVE) across
# the two-node dev stack through the `otherix` CLI as a real operator, then
# asserts the migration object carries the final QEMU `query-migrate` statistics
# and that `otherix migration get` renders the `statistics:` section.
#
# What it proves, end to end, on ONE running VM:
#   create on the source node    -> running
#   migrate (live) to the target -> migration record reaches phase 'completed'
#   migration get (json)         -> .stats is present with ram.total > 0 and
#                                    total_time_ms > 0 (captured from the source
#                                    agent's final query-migrate at completion)
#   migration get (text)         -> prints a `statistics:` section
#   delete                       -> gone
#
# The sibling vm-migration-live smoke proves the live cutover keeps the guest
# running (heartbeat continuity); THIS smoke proves the stats capture path
# (agent final query-migrate -> task result -> CP commitCutover -> etcd ->
# migration get) end to end against a real agent. ram.total and total_time_ms are
# the robust teeth: ram.total is the guest RAM (always > 0) and total_time_ms is
# QEMU's elapsed migration time (always > 0 for a real migration). disk.* and
# dirty_pages_rate can legitimately be 0 (mirror finished between reporter ticks,
# or a converged RAM stream), so they are surfaced but not hard-asserted.
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
#
# Usage: make smoke-vm-migration-live-stats
#   (or: bash dev/smoke/vm-migration-live-stats/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"                                # source node (create is pinned here)
VM="${VM_NAME:-migsmokestats}"                # the single migration VM; delete-firsted
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"             # seconds for vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"           # seconds for the live migrate cutover (disk copy on TCG is slow)
PHASE_WAIT="${PHASE_WAIT:-90}"                # seconds to wait for the CP-projected status.phase / node
MIG_PHASE_WAIT="${MIG_PHASE_WAIT:-120}"       # seconds to wait for the migration record -> completed

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# vm_phase NAME -> prints the CP-observed status.phase ("" if the VM is gone)
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# vm_node NAME -> prints the VM's current node name ("" if unscheduled/gone)
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // ""' 2>/dev/null || true; }

# assert_phase NAME WANT [TIMEOUT]
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

# assert_node NAME WANT [TIMEOUT]
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

# assert_gone NAME
assert_gone() {
  local name="$1" deadline; deadline=$(( SECONDS + PHASE_WAIT ))
  while (( SECONDS < deadline )); do
    otx vm get "$name" --output json >/dev/null 2>&1 || { pass "$name gone (404)"; return 0; }
    sleep 2
  done
  fail "$name still visible after delete within ${PHASE_WAIT}s"
}

cleanup() {
  echo "--- cleanup ---"
  if [ -n "${SMOKE_KEEP:-}" ]; then
    info "SMOKE_KEEP set; leaving VM ${VM} (id=${VMID:-?}) in place for inspection"
    return
  fi
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-migration-live-stats smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# Discover the migration TARGET: a ready node that is NOT the source.
TARGET="$(otx node list --status ready --output json 2>/dev/null \
  | jq -r --arg src "$NODE1" '[.data[]? | select(.name != $src) | .name] | first // ""')"
[[ -n "$TARGET" ]] || fail "no second ready node found besides $NODE1 (run make local-dev-start for a two-node stack)"
info "source=$NODE1 target=$TARGET"

# default pool reconciled on BOTH the source and the target.
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
for n in "$NODE1" "$TARGET"; do
  deadline=$(( SECONDS + 60 )); ok=0
  while (( SECONDS < deadline )); do pool_ready "$n" && { ok=1; break; }; sleep 3; done
  (( ok == 1 )) || fail "default pool not ready on $n within 60s (CP auto-provision)"
done
pass "CP up (${CP_VERSION}); $NODE1 + $TARGET ready; default pool ready on both"

# --- step 1: create on the source -> running ---------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of a stale leftover
otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mib 2048 --disk-gib 10 \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
info "VM id=$VMID"
SOURCE="$(vm_node "$VM")"
[[ "$SOURCE" == "$NODE1" ]] || fail "VM landed on '$SOURCE', expected source $NODE1"
pass "created and running on source $SOURCE"

# --- step 2: live migrate to the target --------------------------------
echo "=== step 2: migrate $VM (live) $SOURCE -> $TARGET ==="
otx vm migrate "$VM" --node "$TARGET" \
  --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "vm migrate (live) did not complete within ${MIGRATE_WAIT}s"
# The cutover committed: the VM is re-pinned to the target node. This smoke's
# subject is the STATS capture path, so it gates on the migration record +
# stats (steps 3-4), not on guest liveness after resume - the dedicated
# vm-migration-live smoke owns the "guest kept running" (heartbeat continuity)
# assertion. Gating phase=running here would couple this stats smoke to the
# target-resume path, which is a separate concern.
assert_node "$VM" "$TARGET"
pass "VM migrated: current node changed to $TARGET (cutover committed)"

# --- step 3: the migration record reached completed (live=true) --------
echo "=== step 3: migration record -> completed (live) ==="
MIGID="$(otx migration list --vm "$VMID" --output json 2>/dev/null \
  | jq -r '.data[0].id // ""')"
[[ "$MIGID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve migration id for VM $VMID (got '${MIGID:-none}')"
info "migration id=$MIGID"
deadline=$(( SECONDS + MIG_PHASE_WAIT )); mphase=""
while (( SECONDS < deadline )); do
  mphase="$(otx migration get "$MIGID" --output json 2>/dev/null | jq -r '.phase // ""' || true)"
  [[ "$mphase" == "completed" ]] && break
  case "$mphase" in failed|cancelled) fail "migration $MIGID reached terminal '$mphase', expected completed" ;; esac
  sleep 2
done
[[ "$mphase" == "completed" ]] || fail "migration $MIGID phase: want 'completed' got '${mphase:-none}' after ${MIG_PHASE_WAIT}s"
pass "migration $MIGID phase=completed"

# --- step 4: THE TEETH - the statistics are captured on the object -----
echo "=== step 4: migration statistics present and sane ==="
# The stats land via the source agent's final query-migrate -> outgoing-live task
# Result -> CP commitCutover -> UpdateMigrationStats -> etcd. They are persisted
# best-effort AFTER the cutover commits, so allow a brief settle window.
deadline=$(( SECONDS + 30 )); STATS="null"
while (( SECONDS < deadline )); do
  STATS="$(otx migration get "$MIGID" --output json 2>/dev/null | jq -c '.stats // null' || true)"
  [[ "$STATS" != "null" && -n "$STATS" ]] && break
  sleep 2
done
[[ "$STATS" != "null" && -n "$STATS" ]] || fail "migration $MIGID .stats absent after completion (want the captured query-migrate stats)"
info "stats: $STATS"

RAM_TOTAL="$(jq -r '.ram.total // 0' <<<"$STATS")"
RAM_XFER="$(jq -r '.ram.transferred // 0' <<<"$STATS")"
TOTAL_TIME="$(jq -r '.total_time_ms // 0' <<<"$STATS")"
DOWNTIME="$(jq -r '.downtime_ms // 0' <<<"$STATS")"
SETUP_TIME="$(jq -r '.setup_time_ms // 0' <<<"$STATS")"
DISK_TOTAL="$(jq -r '.disk.total // 0' <<<"$STATS")"
DIRTY="$(jq -r '.ram.dirty_pages_rate // 0' <<<"$STATS")"
info "ram.total=$RAM_TOTAL ram.transferred=$RAM_XFER disk.total=$DISK_TOTAL total_time_ms=$TOTAL_TIME downtime_ms=$DOWNTIME setup_time_ms=$SETUP_TIME dirty_pages_rate=$DIRTY"

# Robust teeth: ram.total is the guest RAM (always > 0); total_time_ms is QEMU's
# elapsed migration time (always > 0 for a real migration). disk.* and
# dirty_pages_rate may legitimately be 0, so they are surfaced but not asserted.
[[ "$RAM_TOTAL" =~ ^[0-9]+$ && "$RAM_TOTAL" -gt 0 ]] 2>/dev/null \
  || fail "stats.ram.total not a positive integer (got '$RAM_TOTAL')"
[[ "$TOTAL_TIME" =~ ^[0-9]+$ && "$TOTAL_TIME" -gt 0 ]] 2>/dev/null \
  || fail "stats.total_time_ms not a positive integer (got '$TOTAL_TIME')"
pass "stats sane: ram.total=$RAM_TOTAL bytes, total_time_ms=$TOTAL_TIME ms"

# The text formatter renders the statistics section.
echo "--- migration get (text) ---"
TEXT="$(otx migration get "$MIGID")"
echo "$TEXT"
grep -q '^statistics:' <<<"$TEXT" || fail "migration get (text) missing the 'statistics:' section"
pass "migration get renders the statistics section"

# --- step 5: delete -> gone --------------------------------------------
echo "=== step 5: delete ==="
otx vm delete "$VM" --wait --force --wait-timeout "${PHASE_WAIT}s" || fail "vm delete failed"
assert_gone "$VM"
pass "delete -> gone"

trap - EXIT
echo
echo "${GREEN}=== vm-migration-live-stats smoke PASSED ===${NC}"
