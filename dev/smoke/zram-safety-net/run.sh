#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# zram compressed-swap safety-net smoke - proves the agent's host-level zram net
# on a REAL node: enabling it in the node's agent config brings up a zram swap
# device (mem_limit-capped to a percentage of physical RAM), the control plane
# surfaces the observed capability on `node get`, pages spilled under bounded
# memory pressure land in zram COMPRESSED, and disabling it tears the device back
# down. This is real-kernel behaviour (zram module, swapon, cgroup pressure) that
# no mock agent can exercise.
#
# What it proves, end to end, on node-1:
#   enable  -> agent config zram.enabled=true, max_ram_percent=25; agent restart
#             brings up /dev/zramN as swap, mem_limit ~= 25% of MemTotal
#   report  -> `node get --output json` shows capabilities.compressed_swap.kind=zram
#   compress-> a scratch memory cgroup (memory.max=128M, swap unbounded) runs a
#             bounded ~200M allocator; the overflow spills to zram and mm_stat
#             shows compr_data_size < orig_data_size (the net compressed real pages)
#   disable -> config restored (zram off, the default); agent restart swaps the
#             device off; `node get` shows compressed_swap off and the agent's zram
#             device is gone from /proc/swaps (any pre-existing host zram is left be)
#
# All allocations are bounded and confined to a cgroup memory limit - the smoke
# never risks OOMing the node. The zram device number is discovered dynamically
# from /proc/swaps (never hardcoded zram0). The compression step needs cgroup-v2
# memory+swap controls; where those are unavailable it SKIPS with a clear message
# (not a failure), but the enable / report / mem_limit / disable asserts are hard.
#
# Per-node agent config path (the file this smoke patches to toggle zram):
#   Lima (macOS host):   /etc/otherix/agent.yaml            inside each Lima node VM
#   netns (Linux host):  /var/lib/otherix/dev/node1/agent.yaml   on the host FS
# Bootstrap writes agent.yaml only when absent and never overwrites it, so this
# smoke appends a zram block to the existing file (and restores a pristine backup
# on teardown) rather than regenerating the config. Both are reached uniformly
# through run_on <node-1 handle> (a Lima VM shell, or the node-1 netns which
# shares the host FS); an inner `sudo` is a real elevation on Lima and a harmless
# no-op inside the netns.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready. The dev node kernel must have the zram module available (the arm64
# Lima dev VM and the Linux netns host both ship it, plus zstd).
#
# Usage: make smoke-zram-safety-net   (or: bash dev/smoke/zram-safety-net/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
MAX_RAM_PERCENT=25                        # the zram mem_limit knob this smoke sets
READY_WAIT="${READY_WAIT:-150}"           # seconds for a node to return ready after a restart
CAP_WAIT="${CAP_WAIT:-90}"                # seconds for the compressed_swap capability to appear
PRESSURE_FILL="${PRESSURE_FILL:-6}"       # seconds to let the allocator fill before sampling mm_stat

# The node-1 agent config file, per platform (see the header block).
case "${SMOKE_PLATFORM}" in
  lima)  AGENT_CONF="/etc/otherix/agent.yaml" ;;
  netns) AGENT_CONF="$(smoke_state 1)/agent.yaml" ;;
  *)     echo "unsupported SMOKE_PLATFORM: ${SMOKE_PLATFORM}" >&2; exit 1 ;;
esac
AGENT_CONF_BAK="${AGENT_CONF}.zram-smoke-bak"
SCRATCH_CGROUP="/sys/fs/cgroup/otherix-zram-smoke"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
skip() { echo "${YEL}SKIP${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# on1 <cmd...> - run a command on node-1 (Lima VM shell or node-1 netns). Prefix
# with `sudo` at the call site for privileged reads/writes; sudo is a real
# elevation on Lima and a no-op inside the netns (already root).
on1() { run_on "$SMOKE_HANDLE_1" "$@"; }

# node_status NAME -> the CP-observed node status ("" if unknown).
node_status() { otx node get "$1" --output json 2>/dev/null | jq -r '.status' 2>/dev/null || true; }

# wait_node_ready NAME -> poll until the node reports ready.
wait_node_ready() {
  local name="$1" deadline; deadline=$(( SECONDS + READY_WAIT ))
  while (( SECONDS < deadline )); do
    [[ "$(node_status "$name")" == "ready" ]] && { pass "$name ready"; return 0; }
    sleep 3
  done
  fail "$name did not reach ready within ${READY_WAIT}s"
}

# compressed_swap_kind NAME -> the CP-reported compressed-swap kind ("off" when
# the capability is absent/null, i.e. the net is not active).
compressed_swap_kind() {
  otx node get "$1" --output json 2>/dev/null \
    | jq -r '.capabilities.compressed_swap.kind // "off"' 2>/dev/null || true
}

# zram_swap_devices -> every /dev/zramN present in the node's /proc/swaps, one per
# line ("" when none). Discovered dynamically, never hardcoded.
zram_swap_devices() {
  on1 cat /proc/swaps 2>/dev/null | awk '/\/dev\/zram/ {print $1}' || true
}

# zram_delta_device -> the first /dev/zramN in the node's current /proc/swaps that
# is NOT in BASELINE_ZRAM (the pre-existing host devices snapshotted before we
# enabled zram) - i.e. the agent's hot-added device. "" when none appeared. Keying
# on the delta keeps the smoke correct on a shared Linux host that runs its own
# system zram swap (systemd-zram-generator); on Lima the baseline is empty and the
# delta is simply the single new device.
zram_delta_device() {
  local dev
  while read -r dev; do
    [[ -z "$dev" ]] && continue
    case " ${BASELINE_ZRAM:-} " in
      *" ${dev} "*) ;;                 # pre-existing baseline device, ignore
      *) printf '%s\n' "$dev"; return 0 ;;
    esac
  done < <(zram_swap_devices)
}

# Restore the pristine (zram-off) config on node-1 and bounce the stack so the
# agent re-observes and swaps the zram device off. Best-effort; used by both the
# explicit disable step and the failure trap.
teardown_zram() {
  on1 sudo cp "$AGENT_CONF_BAK" "$AGENT_CONF" 2>/dev/null || true
  on1 sudo rm -f "$AGENT_CONF_BAK" 2>/dev/null || true
  on1 sudo rmdir "$SCRATCH_CGROUP" 2>/dev/null || true
  make local-dev-restart >/dev/null 2>&1 || true
}

CONF_PATCHED=0
cleanup() {
  echo "--- cleanup ---"
  (( CONF_PATCHED == 1 )) && teardown_zram
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== zram-safety-net smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
info "CP version: $(cp_version)"
[[ "$(node_status "$NODE1")" == "ready" ]] || fail "$NODE1 not ready; run make local-dev-start"
on1 test -f "$AGENT_CONF" || fail "$NODE1 agent config not found at $AGENT_CONF"
pass "CP up; $NODE1 ready; agent config present at $AGENT_CONF"

# Snapshot any pre-existing host zram swap devices BEFORE we enable ours, so every
# device assertion below keys on the agent's device as a delta (a Linux dev host
# running systemd-zram-generator has its own zram swap that must not be mistaken
# for ours, nor false-FAIL the teardown check).
BASELINE_ZRAM="$(zram_swap_devices | tr '\n' ' ')"
info "pre-existing zram swap devices (baseline): ${BASELINE_ZRAM:-none}"

# --- step 1: enable zram on node-1 -------------------------------------
echo "=== step 1: enable zram on $NODE1 (max_ram_percent=$MAX_RAM_PERCENT) ==="
# A backup already present means a PRIOR run was hard-killed after appending the
# zram block but before its trap restored - the on-disk config is already patched.
# Restore FROM the stale backup (the pristine config) instead of backing up the
# polluted one, otherwise teardown would later "restore" zram-on and leave it
# durably enabled.
if on1 test -f "$AGENT_CONF_BAK"; then
  info "stale backup found ($AGENT_CONF_BAK) - a prior run died mid-flight; restoring pristine config"
  on1 sudo cp "$AGENT_CONF_BAK" "$AGENT_CONF" || fail "could not restore pristine config from $AGENT_CONF_BAK"
else
  on1 sudo cp "$AGENT_CONF" "$AGENT_CONF_BAK" || fail "could not back up $AGENT_CONF"
fi
CONF_PATCHED=1
# Append a top-level zram block (column 0 -> a new top-level mapping key, valid
# regardless of the preceding block). Leading blank line guards against a config
# whose last line has no trailing newline.
printf '\n%s\n%s\n%s\n' 'zram:' '  enabled: true' "  max_ram_percent: ${MAX_RAM_PERCENT}" \
  | on1 sudo tee -a "$AGENT_CONF" >/dev/null || fail "could not patch $AGENT_CONF"
info "patched $AGENT_CONF -> zram.enabled=true; restarting the stack"
make local-dev-restart >/dev/null 2>&1 || fail "make local-dev-restart failed"
wait_node_ready "$NODE1"

# --- step 2: assert the net is active ----------------------------------
echo "=== step 2: assert the compressed-swap net is active ==="
# CP surface: the capability propagates on the next heartbeat after the restart.
deadline=$(( SECONDS + CAP_WAIT )); seen=0
while (( SECONDS < deadline )); do
  [[ "$(compressed_swap_kind "$NODE1")" == "zram" ]] && { seen=1; break; }
  sleep 3
done
(( seen == 1 )) || fail "compressed_swap.kind never became 'zram' on $NODE1 within ${CAP_WAIT}s"
otx node get "$NODE1" --output json | jq -e '.capabilities.compressed_swap.kind == "zram"' >/dev/null \
  || fail "node get does not report compressed_swap.kind == zram"
pass "CP reports compressed_swap.kind == zram on $NODE1"

# Node host: the agent hot-added a NEW /dev/zram swap (delta vs the baseline).
ZDEV="$(zram_delta_device)"
[[ "$ZDEV" == /dev/zram* ]] || fail "no new /dev/zram appeared in $NODE1 /proc/swaps beyond the baseline (got '${ZDEV:-none}')"
ZNAME="$(basename "$ZDEV")"          # e.g. zram1
info "agent zram swap device: $ZDEV"

# mem_limit is set and roughly 25% of MemTotal (generous band: not exact bytes).
MEMLIMIT="$(on1 cat "/sys/block/${ZNAME}/mem_limit" 2>/dev/null | tr -dc '0-9')" || true
if [[ ! "$MEMLIMIT" =~ ^[0-9]+$ ]] || (( MEMLIMIT == 0 )); then
  fail "/sys/block/${ZNAME}/mem_limit is unset or zero (got '${MEMLIMIT:-none}')"
fi
read -r _ MEMTOTAL_KB _ < <(on1 grep -m1 MemTotal /proc/meminfo)
[[ "$MEMTOTAL_KB" =~ ^[0-9]+$ ]] || fail "could not read MemTotal from $NODE1"
MEMTOTAL_B=$(( MEMTOTAL_KB * 1024 ))
# Accept 10%..40% of physical RAM (target 25%, wide tolerance for rounding /
# page alignment / the kernel's own accounting).
BAND_LO=$(( MEMTOTAL_B * 10 / 100 )); BAND_HI=$(( MEMTOTAL_B * 40 / 100 ))
(( MEMLIMIT >= BAND_LO && MEMLIMIT <= BAND_HI )) \
  || fail "mem_limit ${MEMLIMIT}B not within 10-40% of MemTotal ${MEMTOTAL_B}B (target ~25%)"
pass "mem_limit=${MEMLIMIT}B is ~$(( MEMLIMIT * 100 / MEMTOTAL_B ))% of MemTotal (band 10-40%)"

# --- step 3: prove compression under BOUNDED pressure ------------------
echo "=== step 3: pages spill to zram COMPRESSED under a bounded cgroup limit ==="
# Runs entirely on the node. Creates a scratch cgroup-v2 (memory.max=128M, swap
# unbounded), joins it, runs a ~200M newline-less allocator (head|tail must buffer
# the whole stream), so the overflow above 128M spills to zram swap. Samples the
# device mm_stat while pages are resident, then tears the cgroup down. Prints a
# final "SKIP <reason>" (cgroup controls unavailable) or "ORIG=<n> COMPR=<n>".
PRESSURE_OUT="$(on1 sudo bash -s "/sys/block/${ZNAME}/mm_stat" "$PRESSURE_FILL" <<'NODE_EOF'
set -u
MMSTAT="$1"; FILL="$2"
CG=/sys/fs/cgroup/otherix-zram-smoke

[ -e /sys/fs/cgroup/cgroup.controllers ] || { echo "SKIP no cgroup-v2 unified hierarchy on this node"; exit 0; }
grep -qw memory /sys/fs/cgroup/cgroup.controllers || { echo "SKIP memory controller not present at cgroup root"; exit 0; }
mkdir -p "$CG" || { echo "SKIP cannot create scratch cgroup"; exit 0; }
# Delegate the memory controller into the subtree so the child gets memory.max.
if [ -e /sys/fs/cgroup/cgroup.subtree_control ]; then
  grep -qw memory /sys/fs/cgroup/cgroup.subtree_control \
    || echo "+memory" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
fi
[ -e "$CG/memory.max" ] || { echo "SKIP memory controls absent in scratch cgroup"; rmdir "$CG" 2>/dev/null || true; exit 0; }
echo 134217728 > "$CG/memory.max" 2>/dev/null || { echo "SKIP cannot set memory.max"; rmdir "$CG" 2>/dev/null || true; exit 0; }
echo max > "$CG/memory.swap.max" 2>/dev/null || true
# Join this shell to the scratch cgroup; the allocator child inherits membership.
echo $$ > "$CG/cgroup.procs" 2>/dev/null || { echo "SKIP cannot join scratch cgroup"; rmdir "$CG" 2>/dev/null || true; exit 0; }

# Bounded allocator: head emits ~200MB of NUL bytes (no newlines); tail must hold
# the whole newline-less "line" resident (~200MB), capped at 128M by the cgroup so
# the overflow spills to zram swap. timeout bounds it hard.
timeout 15 sh -c 'head -c 200M /dev/zero | tail' >/dev/null 2>&1 &
ALLOC=$!
sleep "$FILL"
# Sample zram stats while the pages are still swapped out. mm_stat fields:
# orig_data_size compr_data_size mem_used_total mem_limit ...
read -r ORIG COMPR _ < "$MMSTAT"
kill "$ALLOC" 2>/dev/null || true
wait "$ALLOC" 2>/dev/null || true
# Leave the scratch cgroup before removing it.
echo $$ > /sys/fs/cgroup/cgroup.procs 2>/dev/null || true
rmdir "$CG" 2>/dev/null || true
echo "ORIG=${ORIG:-0} COMPR=${COMPR:-0}"
NODE_EOF
)" || fail "pressure step failed to run on $NODE1"

RESULT="$(printf '%s\n' "$PRESSURE_OUT" | tail -1)"
case "$RESULT" in
  SKIP*)
    skip "compression proof skipped: ${RESULT#SKIP }"
    ;;
  ORIG=*)
    ORIG="${RESULT#ORIG=}"; ORIG="${ORIG%% *}"
    COMPR="${RESULT##*COMPR=}"
    [[ "$ORIG" =~ ^[0-9]+$ && "$COMPR" =~ ^[0-9]+$ ]] \
      || fail "could not parse zram mm_stat from pressure output: '$RESULT'"
    (( ORIG > 0 )) || fail "no pages spilled to zram (orig_data_size=0); pressure did not reach swap"
    (( COMPR < ORIG )) \
      || fail "zram did not compress: compr_data_size=$COMPR not < orig_data_size=$ORIG"
    pass "pages compressed in zram: orig_data_size=$ORIG compr_data_size=$COMPR"
    ;;
  *)
    fail "unexpected pressure-step output: '$RESULT'"
    ;;
esac

# --- step 4: disable zram -> device torn down --------------------------
echo "=== step 4: disable zram on $NODE1 -> device gone ==="
teardown_zram
CONF_PATCHED=0                          # config restored; the trap has nothing left to do
wait_node_ready "$NODE1"
deadline=$(( SECONDS + CAP_WAIT )); off=0
while (( SECONDS < deadline )); do
  [[ "$(compressed_swap_kind "$NODE1")" != "zram" ]] && { off=1; break; }
  sleep 3
done
(( off == 1 )) || fail "compressed_swap still reports 'zram' on $NODE1 after disable within ${CAP_WAIT}s"
LEFT="$(zram_delta_device)"
[[ -z "$LEFT" ]] || fail "the agent's zram swap device is still present after disable: $LEFT"
pass "compressed_swap off on $NODE1 and no agent zram device beyond the baseline in /proc/swaps"

trap - EXIT
echo
echo "${GREEN}=== zram-safety-net smoke PASSED ===${NC}"
