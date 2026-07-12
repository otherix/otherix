#!/usr/bin/env bash
# Real-agent smoke: memory overcommit lands a VM ONLY on a zram-equipped node.
#
# A VM whose requested memory exceeds every node's strict physical availability
# fits only on the node that carries a qualifying zram compressed-swap net (the
# scheduler grants that node additive headroom bounded by the zram size); a node
# without the net stays strict and rejects the same request.
#
# Assumes memory overcommit is already deployed (run `make local-dev-deploy` first).
# Mutates the dev control-plane config (overcommit_ratio) and node-1 zram at
# runtime and restores both at teardown.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=/dev/null
source dev/smoke/lib.sh

OTX="${OTX:-./bin/otherix}"
API_CFG="dev/config/api.yaml"
ZNODE="node-1"      # gets a zram net
ZHANDLE="$(smoke_handle 1)"
SNODE="node-2"      # strict, no zram
OFFNODE="node-3"    # cordoned out of the scenario
VMPOS="ovc-pos"
VMNEG="ovc-neg"
MEM_MIB=8500        # > node strict avail (~7922), < node-1 total+headroom (~9902)
RATIO="1.5"

otx() { "$OTX" "$@"; }
pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
info() { printf '\033[33m..\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
node_field() { otx node get "$1" --output json 2>/dev/null | jq -r "$2" 2>/dev/null; }
vm_phase()   { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null; }
vm_node()    { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // "null"' 2>/dev/null; }
vm_reason()  { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.reason // "-"' 2>/dev/null; }

teardown() {
  info "teardown"
  otx vm delete "$VMPOS" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  otx vm delete "$VMNEG" --force --wait --wait-timeout 90s >/dev/null 2>&1 || true
  otx node uncordon "$OFFNODE" >/dev/null 2>&1 || true
  # Restore strict CP config and bounce the stack back to it.
  git checkout -- "$API_CFG" >/dev/null 2>&1 || true
  make local-dev-restart >/dev/null 2>&1 || true
  make dev-zram-off NODE="$ZHANDLE" >/dev/null 2>&1 || true
}
trap teardown EXIT

echo "=== preconditions ==="
for n in "$ZNODE" "$SNODE"; do
  [[ "$(node_field "$n" '.status')" == "ready" ]] || fail "$n not ready"
done
pass "$ZNODE + $SNODE ready"

echo "=== step 1: provision a zram net on $ZNODE only ==="
make dev-zram-on NODE="$ZHANDLE" ZRAM_SIZE="ram / 2" >/dev/null 2>&1 || fail "dev-zram-on failed"
deadline=$(( SECONDS + 90 )); ok=0
while (( SECONDS < deadline )); do
  sz="$(node_field "$ZNODE" '.capabilities.compressed_swap.size_mib // 0')"
  [[ "${sz:-0}" -ge 256 ]] && { ok=1; info "$ZNODE compressed_swap.size_mib=$sz"; break; }
  sleep 3
done
(( ok == 1 )) || fail "$ZNODE never reported a zram net via heartbeat"
[[ "$(node_field "$SNODE" '.capabilities.compressed_swap.size_mib // 0')" -lt 256 ]] || info "note: $SNODE unexpectedly has a zram net"
pass "zram net on $ZNODE"

echo "=== step 2: enable memory overcommit on the control plane (ratio $RATIO) ==="
# Flip only the memory overcommit_ratio; the zram floor/confidence keys already
# ship in the dev config (strict defaults).
perl -0pi -e "s/(memory:\s*\n\s*enabled: true\s*\n\s*overcommit_ratio: )1\.0/\${1}$RATIO/" "$API_CFG"
grep -qE "overcommit_ratio: $RATIO" "$API_CFG" || fail "could not set overcommit_ratio=$RATIO in $API_CFG"
make local-dev-restart >/dev/null 2>&1 || fail "control-plane restart failed"
# Wait for the CP to come back and observe eligibility on the zram node.
deadline=$(( SECONDS + 90 )); ok=0
while (( SECONDS < deadline )); do
  elig="$(node_field "$ZNODE" '.memory_overcommit.eligible // false')"
  head="$(node_field "$ZNODE" '.memory_overcommit.headroom_mib // 0')"
  [[ "$elig" == "true" && "${head:-0}" -gt 0 ]] && { ok=1; info "$ZNODE memory_overcommit: eligible=$elig headroom_mib=$head"; break; }
  sleep 3
done
(( ok == 1 )) || fail "$ZNODE never became overcommit-eligible after enabling the ratio"
[[ "$(node_field "$SNODE" '.memory_overcommit.eligible // false')" == "false" ]] || fail "$SNODE is eligible but has no zram net"
pass "overcommit live: $ZNODE eligible, $SNODE strict"

echo "=== step 3: cordon $OFFNODE out of the scenario ==="
otx node cordon "$OFFNODE" >/dev/null 2>&1 || info "cordon $OFFNODE (best-effort)"
pass "$OFFNODE cordoned"

echo "=== step 4: POSITIVE - a ${MEM_MIB}MiB VM lands on the zram node ==="
otx vm create "$VMPOS" --image-url "$SMOKE_IMAGE_URL" --arch "$SMOKE_ARCH" \
  --vcpus 2 --memory-mib "$MEM_MIB" --wait --wait-timeout 600s \
  || fail "$VMPOS did not reach running (overcommit placement failed)"
landed="$(vm_node "$VMPOS")"
[[ "$landed" == "$ZNODE" ]] || fail "$VMPOS landed on '$landed', want $ZNODE (only the zram node has headroom for ${MEM_MIB}MiB)"
pass "$VMPOS running on $ZNODE (overcommit granted by the zram net)"

echo "=== step 5: NEGATIVE - the same request pinned to the strict node is rejected ==="
otx vm create "$VMNEG" --image-url "$SMOKE_IMAGE_URL" --arch "$SMOKE_ARCH" --node "$SNODE" \
  --vcpus 2 --memory-mib "$MEM_MIB" >/dev/null 2>&1 || true
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do
  ph="$(vm_phase "$VMNEG")"; rs="$(vm_reason "$VMNEG")"
  if [[ "$ph" == "running" ]]; then
    fail "$VMNEG reached running on the strict node $SNODE (overcommit leaked to a node with no zram net)"
  fi
  if [[ "$rs" == "insufficient_resources" || "$rs" == "no_eligible_nodes" ]]; then
    ok=1; info "$VMNEG stays pending: reason=$rs"; break
  fi
  sleep 3
done
(( ok == 1 )) || fail "$VMNEG did not surface an insufficient-resources reason on $SNODE (got phase=$(vm_phase "$VMNEG") reason=$(vm_reason "$VMNEG"))"
pass "$VMNEG rejected on strict $SNODE"

printf '\n\033[32m=== scheduler-memory-overcommit smoke PASSED ===\033[0m\n'
