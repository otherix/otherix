#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# zram compressed-swap safety-net smoke - proves the agent OBSERVES a
# host-provisioned zram swap on a REAL node and surfaces it as a node capability.
# The agent no longer configures zram; the OPERATOR provisions it (here via
# systemd-zram-generator, as root on the node) and the agent's heartbeat collector
# reports whatever /proc/swaps + /sys/block/zramN currently show. This smoke plays
# that operator: it installs and configures systemd-zram-generator on node-1, then
# asserts the control plane surfaces compressed_swap.kind=zram WITHOUT any agent
# restart (Observe runs every heartbeat), that spilled pages land COMPRESSED under
# bounded memory pressure, and that de-provisioning returns the node to
# compressed_swap off. This is real-kernel behaviour (zram module, swapon, cgroup
# pressure) that no mock agent can exercise.
#
# What it proves, end to end, on node-1:
#   provision -> operator installs systemd-zram-generator, drops
#               /etc/systemd/zram-generator.conf ([zram0], zram-size=ram/4,
#               zstd), daemon-reload, starts systemd-zram-setup@zram0; a
#               /dev/zramN swap comes up. No agent change, no agent restart.
#   observe   -> the agent's next heartbeat reports the device; `node get
#               --output json` shows capabilities.compressed_swap.kind=zram with
#               size_mib > 0, and /proc/swaps on the node lists /dev/zramN.
#   compress  -> a scratch memory cgroup (memory.max=128M, swap unbounded) runs a
#               bounded ~200M allocator; the overflow spills to zram and the
#               device's mm_stat shows compr_data_size < orig_data_size (real
#               pages compressed).
#   teardown  -> operator stops the unit, removes the conf, daemon-reload,
#               swapoff + hot_remove the device; `node get` returns to
#               compressed_swap off (any pre-existing host zram is left be).
#
# All allocations are bounded and confined to a cgroup memory limit - the smoke
# never risks OOMing the node. The zram device number is discovered dynamically
# from /proc/swaps as a DELTA against the set present before provisioning (never
# hardcoded zram0), so a host already running its own system zram swap is not
# disturbed. The compression step needs cgroup-v2 memory+swap controls; where
# those are unavailable it SKIPS with a clear message (not a failure), but the
# provision / observe asserts are hard.
#
# Provisioning and teardown run as ROOT on the node (the operator role), reached
# through run_on <node-1 handle>: a Lima VM shell where an inner `sudo` is a real
# elevation, or the node-1 netns which is already root (sudo is a no-op). The agent
# user itself stays unprivileged and only observes.
#
# systemd-zram-generator lives in the universe pocket; a transient mirror/network
# error installing it SKIPS the whole smoke with a clear message rather than
# hard-failing (the kernel/observer path under test is unaffected by a flaky apt).
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
ZRAM_CONF="/etc/systemd/zram-generator.conf"   # operator drop-in this smoke installs
ZRAM_UNIT="systemd-zram-setup@zram0"           # the generator's setup unit for [zram0]
CAP_WAIT="${CAP_WAIT:-90}"                # seconds for the observed capability to appear/clear
PRESSURE_FILL="${PRESSURE_FILL:-6}"       # seconds to let the allocator fill before sampling mm_stat
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
# provisioned) - i.e. the device this smoke created. "" when none appeared. Keying
# on the delta keeps the smoke correct on a shared Linux host that runs its own
# system zram swap; on Lima the baseline is empty and the delta is simply the
# single new device.
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

# De-provision zram on node-1: stop the setup unit, remove the conf we installed,
# daemon-reload, then swapoff + hot_remove any zram swap device NOT in the
# baseline. Best-effort and idempotent - safe to run even if provisioning never
# happened (e.g. the apt-install SKIP path), and it only ever touches the conf we
# wrote and the swap devices we created (baseline devices are left be).
teardown_zram() {
  on1 sudo systemctl stop "$ZRAM_UNIT" 2>/dev/null || true
  # Stopping the unit is harmless and idempotent. Everything below only runs once
  # we have actually provisioned (CONF_WRITTEN=1) - and a smoke-created zram device
  # can ONLY exist after that point, so guarding here keeps teardown from ever
  # touching a pre-existing host zram (or its config) when the smoke aborts before
  # provisioning or before the baseline snapshot.
  (( CONF_WRITTEN == 1 )) || return 0
  on1 sudo rm -f "$ZRAM_CONF" 2>/dev/null || true
  on1 sudo systemctl daemon-reload 2>/dev/null || true
  # swapoff + hot_remove every non-baseline /dev/zram swap. hot_remove takes the
  # bare device number on /sys/class/zram-control/hot_remove.
  on1 sudo bash -s "$BASELINE_ZRAM" 2>/dev/null <<'NODE_EOF' || true
set -u
BASELINE=" $1 "
while read -r dev _; do
  case "$dev" in /dev/zram*) ;; *) continue ;; esac
  case "$BASELINE" in *" $dev "*) continue ;; esac
  swapoff "$dev" 2>/dev/null || true
  echo "${dev#/dev/zram}" > /sys/class/zram-control/hot_remove 2>/dev/null || true
done < /proc/swaps
NODE_EOF
  on1 sudo rmdir "$SCRATCH_CGROUP" 2>/dev/null || true
}

# Trap-referenced state, initialised before the trap is installed. BASELINE_ZRAM
# is captured for real in preconditions (before any provisioning); the device
# loop above never runs until CONF_WRITTEN=1, which is strictly after that capture.
BASELINE_ZRAM=""
CONF_WRITTEN=0
cleanup() {
  echo "--- cleanup ---"
  teardown_zram
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== zram-safety-net smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
info "CP version: $(cp_version)"
[[ "$(node_status "$NODE1")" == "ready" ]] || fail "$NODE1 not ready; run make local-dev-start"
pass "CP up; $NODE1 ready"

# Snapshot any pre-existing host zram swap devices BEFORE we provision ours, so
# every device assertion below keys on the delta and cleanup only touches what this
# smoke created (a host already running its own zram swap must not be mistaken for
# ours, nor false-FAIL the teardown check).
BASELINE_ZRAM="$(zram_swap_devices | tr '\n' ' ')"
info "pre-existing zram swap devices (baseline): ${BASELINE_ZRAM:-none}"

# --- step 1: operator provisions zram on node-1 ------------------------
echo "=== step 1: provision host zram on $NODE1 (systemd-zram-generator) ==="
# Install the package (idempotent). universe pocket, so a flaky mirror/network is a
# SKIP of the whole smoke, not a FAIL: the kernel/observer path under test is
# unaffected by apt weather.
if on1 sudo dpkg -s systemd-zram-generator >/dev/null 2>&1; then
  info "systemd-zram-generator already installed"
else
  info "installing systemd-zram-generator (apt-get update + install)"
  if ! on1 sudo bash -c 'apt-get update && apt-get install -y systemd-zram-generator' >/dev/null 2>&1; then
    skip "could not install systemd-zram-generator (transient mirror/network?); nothing was provisioned"
    exit 0
  fi
fi

# Drop the operator config: a single [zram0] swap sized to a quarter of RAM,
# zstd-compressed, high swap priority. Written by the operator (root), never by the
# agent.
on1 sudo tee "$ZRAM_CONF" >/dev/null <<'NODE_EOF' || fail "could not write $ZRAM_CONF on $NODE1"
[zram0]
zram-size = ram / 4
compression-algorithm = zstd
swap-priority = 100
NODE_EOF
CONF_WRITTEN=1
on1 sudo systemctl daemon-reload || fail "systemctl daemon-reload failed on $NODE1"
on1 sudo systemctl start "$ZRAM_UNIT" || fail "systemctl start $ZRAM_UNIT failed on $NODE1"
info "provisioned $ZRAM_CONF and started $ZRAM_UNIT"

# The device must actually come up as swap before we assert observation.
ZDEV="$(zram_delta_device)"
[[ "$ZDEV" == /dev/zram* ]] \
  || fail "no new /dev/zram appeared in $NODE1 /proc/swaps beyond the baseline (got '${ZDEV:-none}')"
ZNAME="$(basename "$ZDEV")"          # e.g. zram0
pass "host zram provisioned: $ZDEV up as swap on $NODE1"

# --- step 2: assert the agent OBSERVES it ------------------------------
echo "=== step 2: assert the CP surfaces the observed compressed-swap net ==="
# No agent restart: Observe runs every heartbeat, so the capability appears on the
# next heartbeat after the device came up.
deadline=$(( SECONDS + CAP_WAIT )); seen=0
while (( SECONDS < deadline )); do
  [[ "$(compressed_swap_kind "$NODE1")" == "zram" ]] && { seen=1; break; }
  sleep 3
done
(( seen == 1 )) || fail "compressed_swap.kind never became 'zram' on $NODE1 within ${CAP_WAIT}s"
otx node get "$NODE1" --output json | jq -e '.capabilities.compressed_swap.kind == "zram"' >/dev/null \
  || fail "node get does not report compressed_swap.kind == zram"
SIZE_MIB="$(otx node get "$NODE1" --output json | jq -r '.capabilities.compressed_swap.size_mib // 0')"
[[ "$SIZE_MIB" =~ ^[0-9]+$ ]] || fail "compressed_swap.size_mib not an integer (got '${SIZE_MIB:-none}')"
(( SIZE_MIB > 0 )) || fail "compressed_swap.size_mib is not > 0 (got $SIZE_MIB)"
on1 grep -q '/dev/zram' /proc/swaps || fail "/dev/zram not present in $NODE1 /proc/swaps"
pass "CP observes compressed_swap.kind == zram, size_mib=${SIZE_MIB} on $NODE1"

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

# --- step 4: de-provision -> observed off ------------------------------
echo "=== step 4: de-provision zram on $NODE1 -> device gone, capability clears ==="
teardown_zram
CONF_WRITTEN=0                          # de-provisioned; the trap has nothing left to do
deadline=$(( SECONDS + CAP_WAIT )); off=0
while (( SECONDS < deadline )); do
  [[ "$(compressed_swap_kind "$NODE1")" != "zram" ]] && { off=1; break; }
  sleep 3
done
(( off == 1 )) || fail "compressed_swap still reports 'zram' on $NODE1 after de-provision within ${CAP_WAIT}s"
LEFT="$(zram_delta_device)"
[[ -z "$LEFT" ]] || fail "a smoke-created zram swap device is still present after de-provision: $LEFT"
pass "compressed_swap off on $NODE1 and no smoke zram device beyond the baseline in /proc/swaps"

trap - EXIT
echo
echo "${GREEN}=== zram-safety-net smoke PASSED ===${NC}"
