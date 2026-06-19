#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM lifecycle smoke - drives the full lifecycle transition matrix through the
# `otherix` CLI against a REAL agent (qemu + QMP), closing a real-agent
# coverage gap: otherwise only `vm create` + `vm delete` are exercised end to end;
# start/stop/poweroff/reboot/pause/resume/reset are unit-tested only, never on a
# live agent. This smoke proves each verb actually drives qemu on the node.
#
# What it proves, end to end, on ONE VM pinned to node-1 (no network, SLIRP):
#   create   -> running     (qemu spawned, QMP responsive)
#   pause    -> paused       (QMP stop)               | PID unchanged
#   resume   -> running      (QMP cont)               | PID unchanged
#   reset    -> running      (QMP system_reset)       | PID UNCHANGED (in place)
#   stop     -> stopped      (graceful ACPI powerdown)  [needs a booted guest]
#   start    -> running      (re-spawn)
#   reboot   -> running      (graceful stop + re-spawn) [needs a booted guest] | PID CHANGED
#   poweroff -> stopped      (hard QMP quit / SIGKILL)
#   delete   -> gone
#
# Two of the verbs - graceful `stop` and `reboot` - issue ACPI system_powerdown
# and the agent waits up to its shutdown grace for the guest to honour it; if the
# guest has not booted far enough the task fails with stop_timeout. So before each
# graceful op we wait for an every-boot guest-up sentinel. The sentinel is a
# systemd oneshot (cloud-init below) that echoes OTHERIX_GUEST_UP to the serial
# console on EVERY boot; the agent persists that console to
# <state_root>/vms/<id>/serial.log (append-only, so we count occurrences and wait
# for the count to INCREASE rather than grep for a possibly-stale match).
#
# The QMP-only verbs (pause/resume/reset/poweroff/start) need no booted guest -
# `running` means "qemu up + QMP answering", reached in seconds even under the
# slow TCG emulation a dev Mac uses (no nested KVM). Only the two graceful waits
# pay the full guest-boot cost; budget GUEST_WAIT generously on TCG.
#
# reset vs reboot both end in `running` on the CP, so the CP phase alone cannot
# tell them apart. We read the agent's per-VM pidfile to assert the real
# distinction: reset (QMP system_reset) PRESERVES the qemu process (PID
# unchanged); reboot REPLACES it (PID changes).
#
# PREREQUISITES: a seeded two-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# node-1 ready with the cluster default pool reconciled.
#
# Usage: make smoke-vm-lifecycle   (or: bash dev/smoke/vm-lifecycle/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
NODE1="node-1"
VM="${VM_NAME:-vmlc-smoke}"               # the single lifecycle VM; delete-firsted
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"         # seconds for vm create -> running (incl. cold image fetch)
GUEST_WAIT="${GUEST_WAIT:-720}"           # seconds to wait for a full guest boot (TCG is slow)
OP_WAIT="${OP_WAIT:-180}"                 # seconds for an async lifecycle task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-90}"            # seconds to wait for the CP-projected status.phase

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

# guest_up_count -> number of OTHERIX_GUEST_UP lines in the VM's serial.log so
# far (0 when the file is absent). Append-only log -> the count is monotonic, so
# callers wait for it to INCREASE rather than match a possibly-stale sentinel.
guest_up_count() {
  local n
  n="$(run_on "$SMOKE_HANDLE_1" sudo grep -c "OTHERIX_GUEST_UP" "$SERIAL" 2>/dev/null)" || true
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  printf '%s' "$n"
}

# wait_guest_up BASE -> block until guest_up_count exceeds BASE (a fresh boot
# reached multi-user.target, so the guest now honours ACPI powerdown).
wait_guest_up() {
  local base="$1" deadline; deadline=$(( SECONDS + GUEST_WAIT ))
  info "waiting for a guest boot to complete (sentinel count > ${base}, <= ${GUEST_WAIT}s)"
  while (( SECONDS < deadline )); do
    (( $(guest_up_count) > base )) && { pass "guest booted (ACPI-ready)"; return 0; }
    sleep 5
  done
  echo "--- $VM serial tail ---"; run_on "$SMOKE_HANDLE_1" sudo tail -40 "$SERIAL" 2>/dev/null || true
  fail "guest did not reach a booted state within ${GUEST_WAIT}s (graceful stop/reboot needs it)"
}

# qemu_pid -> the agent-recorded qemu PID for the VM ("" if the pidfile is gone)
qemu_pid() { run_on "$SMOKE_HANDLE_1" sudo cat "$PIDF" 2>/dev/null | tr -dc '0-9' || true; }

cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- the lifecycle cloud-config (every-boot guest-up sentinel) ---------
# A systemd oneshot, ordered After=multi-user.target and WantedBy it, so it runs
# at the tail of EVERY boot (the point at which logind handles the ACPI power
# button, i.e. graceful stop/reboot will be honoured). It echoes the sentinel to
# the arch-correct serial device (ttyS0 on amd64, ttyAMA0 on arm64), which the
# agent captures into serial.log. Quoted heredoc -> the $SC shell var stays
# literal for the guest, not expanded here.
read -r -d '' CLOUD_INIT <<'EOF' || true
#cloud-config
write_files:
  - path: /etc/systemd/system/otherix-guest-up.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Otherix lifecycle-smoke guest-up sentinel
      After=multi-user.target
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'SC=/dev/ttyS0; [ -e /dev/ttyAMA0 ] && SC=/dev/ttyAMA0; echo OTHERIX_GUEST_UP > "$SC"'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, otherix-guest-up.service ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# --- preconditions -----------------------------------------------------
echo "=== vm-lifecycle smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
curl -fsS http://localhost:8080/healthz >/dev/null || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(curl -fsS http://localhost:8080/healthz | jq -r '.version')"
info "CP version: ${CP_VERSION}"
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# default pool reconciled on node-1 (the VM is created without --pool and resolves
# to the cluster default, auto-provisioned per node on ready - PR #15).
pool_ready() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 ready; default pool ready"

# --- step 1: create -> running ----------------------------------------
echo "=== step 1: create $VM on $NODE1 -> running ==="
cleanup >/dev/null 2>&1 || true   # best-effort delete-first of a stale leftover
printf '%s' "$CLOUD_INIT" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create did not reach running within ${CREATE_WAIT}s"
assert_phase "$VM" running
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
SERIAL="$(smoke_state 1)/vms/${VMID}/serial.log"
PIDF="$(smoke_state 1)/vms/${VMID}/qemu.pid"
info "VM id=$VMID  serial=$SERIAL"
pass "created and running"

# --- step 2: QMP-only transitions (no booted guest needed) -------------
echo "=== step 2: QMP-only transitions (pause/resume/reset) ==="
PID_BEFORE="$(qemu_pid)"; [[ "$PID_BEFORE" =~ ^[0-9]+$ ]] || fail "no qemu pid for running VM"
info "qemu pid=$PID_BEFORE"

otx vm pause "$VM" >/dev/null || fail "vm pause failed"
assert_phase "$VM" paused
otx vm resume "$VM" >/dev/null || fail "vm resume failed"
assert_phase "$VM" running

# reset = QMP system_reset: the guest reboots IN PLACE, the qemu process (and its
# PID) is preserved. Assert phase stays running AND the PID did not change.
otx vm reset "$VM" >/dev/null || fail "vm reset failed"
assert_phase "$VM" running
PID_AFTER_RESET="$(qemu_pid)"
[[ "$PID_AFTER_RESET" == "$PID_BEFORE" ]] \
  || fail "reset changed the qemu pid ($PID_BEFORE -> $PID_AFTER_RESET); system_reset must preserve the process"
pass "pause/resume/reset OK (reset preserved qemu pid $PID_BEFORE)"

# --- step 3: graceful stop -> start (needs a booted guest) -------------
echo "=== step 3: graceful stop + start ==="
# The reset above re-booted the guest; wait for that boot to finish so the ACPI
# powerdown the graceful stop sends is honoured (else the task fails stop_timeout).
wait_guest_up 0
otx vm stop "$VM" --wait --wait-timeout "${OP_WAIT}s" || fail "graceful vm stop failed (stop_timeout?)"
assert_phase "$VM" stopped
otx vm start "$VM" --wait --wait-timeout "${OP_WAIT}s" || fail "vm start failed"
assert_phase "$VM" running
pass "graceful stop -> stopped, start -> running"

# --- step 4: graceful reboot (needs a booted guest, replaces the process)
echo "=== step 4: graceful reboot ==="
# The start above began a fresh boot; wait for it before the graceful reboot,
# whose stop-phase also issues ACPI powerdown.
base="$(guest_up_count)"; wait_guest_up "$base"
PID_BEFORE_REBOOT="$(qemu_pid)"; [[ "$PID_BEFORE_REBOOT" =~ ^[0-9]+$ ]] || fail "no qemu pid before reboot"
otx vm reboot "$VM" --wait --wait-timeout "${OP_WAIT}s" || fail "vm reboot failed"
assert_phase "$VM" running
PID_AFTER_REBOOT="$(qemu_pid)"
[[ "$PID_AFTER_REBOOT" =~ ^[0-9]+$ ]] || fail "no qemu pid after reboot"
[[ "$PID_AFTER_REBOOT" != "$PID_BEFORE_REBOOT" ]] \
  || fail "reboot kept the qemu pid ($PID_BEFORE_REBOOT); reboot must replace the process"
pass "reboot -> running (qemu pid replaced $PID_BEFORE_REBOOT -> $PID_AFTER_REBOOT)"

# --- step 5: hard poweroff -> stopped ----------------------------------
echo "=== step 5: hard poweroff ==="
otx vm poweroff "$VM" --wait --wait-timeout "${OP_WAIT}s" || fail "vm poweroff failed"
assert_phase "$VM" stopped
pass "poweroff -> stopped"

# --- step 6: delete -> gone --------------------------------------------
echo "=== step 6: delete ==="
otx vm delete "$VM" --wait --force --wait-timeout "${OP_WAIT}s" || fail "vm delete failed"
assert_gone "$VM"
pass "delete -> gone"

trap - EXIT
echo
echo "${GREEN}=== vm-lifecycle smoke PASSED ===${NC}"
