# dev/smoke/lib.sh — platform abstraction shared by dev/smoke/*/run.sh.
#
# The three-node dev stack is backed by three Lima VMs on macOS and by three
# network namespaces (otns1/otns2/otns3) on native Linux. Smoke scripts source
# this file and target nodes through run_on / the per-node handles instead of
# calling limactl directly, so the same smoke runs unchanged on both platforms:
#
#   macOS : run_on <handle> <cmd...> -> limactl shell <handle> -- <cmd...>
#   Linux : run_on <handle> <cmd...> -> sudo ip netns exec <handle> <cmd...>
#
# Handles and state dirs (the agent state_path root, where vms/<id>/serial.log
# and wg/private.key live) are exposed per node:
#   SMOKE_HANDLE_1 / _2 / _3   node-1 / node-2 / node-3 execution handle
#   SMOKE_STATE_1  / _2 / _3   node-1 / node-2 / node-3 agent state_path root
#
# This file is sourced, not executed; it sets shell variables in the caller.
# shellcheck shell=bash
# shellcheck disable=SC2034  # SMOKE_* vars are the public API, consumed by sourcing scripts

case "$(uname -s)" in
    Darwin)
        SMOKE_PLATFORM="lima"
        SMOKE_HANDLE_1="${VM1:-otherix-dev-1}"
        SMOKE_HANDLE_2="${VM2:-otherix-dev-2}"
        SMOKE_HANDLE_3="${VM3:-otherix-dev-3}"
        SMOKE_STATE_1="/var/lib/otherix"
        SMOKE_STATE_2="/var/lib/otherix"
        SMOKE_STATE_3="/var/lib/otherix"
        ;;
    Linux)
        SMOKE_PLATFORM="netns"
        SMOKE_HANDLE_1="otns1"
        SMOKE_HANDLE_2="otns2"
        SMOKE_HANDLE_3="otns3"
        SMOKE_STATE_1="/var/lib/otherix/dev/node1"
        SMOKE_STATE_2="/var/lib/otherix/dev/node2"
        SMOKE_STATE_3="/var/lib/otherix/dev/node3"
        ;;
    *)
        echo "unsupported platform: $(uname -s)" >&2
        exit 1
        ;;
esac

# SMOKE_ARCH / SMOKE_IMAGE_URL — default VM arch + image for smokes that boot a
# VM. The dev node arch equals the host arch (the agent runs on the host on Linux,
# or in a same-arch Lima VM on macOS), so a VM must use the host arch to boot
# under KVM instead of slow cross-arch TCG. Smokes use these as the default for
# their overrideable ARCH / IMAGE_URL.
case "$(uname -m)" in
    x86_64|amd64)  SMOKE_ARCH="amd64" ;;
    aarch64|arm64) SMOKE_ARCH="arm64" ;;
    *)
        echo "unsupported host arch: $(uname -m)" >&2
        exit 1
        ;;
esac
SMOKE_IMAGE_URL="https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-${SMOKE_ARCH}.img"

# run_on <handle> <cmd...> — run a command on the node identified by <handle>
# (a Lima VM name on macOS, a netns name on Linux). On Linux the command runs as
# root inside the namespace, so an inner `sudo` in <cmd...> is a harmless no-op.
run_on() {
    local handle="$1"; shift
    case "${SMOKE_PLATFORM}" in
        lima)  limactl shell "${handle}" -- "$@" ;;
        netns) sudo ip netns exec "${handle}" "$@" ;;
    esac
}

# smoke_handle <1|2|3> / smoke_state <1|2|3> — accessors for the per-node handle
# and state_path root, for scripts that index nodes dynamically.
smoke_handle() { local v="SMOKE_HANDLE_$1"; printf '%s' "${!v}"; }
smoke_state()  { local v="SMOKE_STATE_$1";  printf '%s' "${!v}"; }

# smoke_require_node_cmd <cmd> — fail early with an actionable message if a tool a
# smoke runs ON a node is missing. On Linux the smoke runs it inside the netns,
# which shares the host filesystem, so the binary must be on the host PATH; on
# macOS the Lima VM provides it (provisioned in the VM image), so this is a no-op.
smoke_require_node_cmd() {
    [ "${SMOKE_PLATFORM}" = "netns" ] || return 0
    command -v "$1" >/dev/null 2>&1 && return 0
    echo "✗ '$1' not found on the host — required by this smoke (it runs inside the netns)" >&2
    case "$1" in
        wg) echo "  install it: sudo apt-get install -y wireguard-tools" >&2 ;;
    esac
    exit 1
}

# CP_BASE — base URL of the dev control-plane user API. The dev stack serves the
# user /v1 API over TLS (dev/config/api.yaml server.tls.enabled), so the default
# is https. The cluster cert is signed by the self-generated dev cluster CA, so
# the readiness helpers below pass -k: a liveness probe is not a trust check, and
# it runs before any CA has been pinned. The operator-facing CLI path (the seeded
# `dev` cluster) pins the real CA inline and does verify.
CP_BASE="${CP_BASE:-https://localhost:8080}"

# cp_ready — succeed when the CP liveness endpoint answers, fail otherwise. Used
# by smoke preflights in place of a raw curl so the scheme/flags live in one spot.
cp_ready() { curl -fsSk "${CP_BASE}/healthz" >/dev/null 2>&1; }

# cp_version — print the CP build version from /healthz (best-effort; empty on
# failure). Smokes use it only for a banner line.
cp_version() { curl -fsSk "${CP_BASE}/healthz" | jq -r '.version' 2>/dev/null; }

# --- orphaned-VM hygiene -------------------------------------------------
#
# A force-delete removes the control-plane VM row without instructing the agent
# to tear the VM down, so the agent keeps the qemu process and replays it from
# its own meta.json on every restart. Those orphans accumulate across runs and
# actively corrupt later ones: they hold vCPU and memory the scheduler then
# refuses to allocate (a pinned create silently stays pending), and a VM created
# with the same name collides with the orphan on the agent's name-addressed
# surface. Smokes therefore reap them BEFORE they create anything - at that
# point any VM the agent runs but the CP does not know is unambiguously stale,
# so there is no race with a create that is still in flight.

# smoke_cp_vm_ids - the VM ids the control plane currently knows, one per line.
smoke_cp_vm_ids() {
    "${OTX:-./bin/otherix}" vm list --output json 2>/dev/null | jq -r '.data[]?.id // empty' 2>/dev/null
}

# smoke_node_vm_uuids IDX - the uuids of qemu processes running on that node.
smoke_node_vm_uuids() {
    run_on "$(smoke_handle "$1")" sh -c \
        "pgrep -a qemu-system 2>/dev/null | grep -oE '\-uuid [0-9a-f-]+' | awk '{print \$2}'" 2>/dev/null
}

# smoke_reap_orphan_vms - stop every qemu the CP does not know about and drop
# the on-node state that would otherwise replay it. Prints what it reaped.
# Killing alone is not enough: the agent rebuilds its registry from
# <state>/vms/<uuid>/meta.json at startup and restarts the VM, so the state
# directory and the pool disk must go too, with the agent stopped across the
# removal so it cannot race the reap.
smoke_reap_orphan_vms() {
    local known idx handle uuids u reaped=0
    known="$(smoke_cp_vm_ids)"
    for idx in 1 2 3; do
        handle="$(smoke_handle "$idx")"
        uuids="$(smoke_node_vm_uuids "$idx")"
        [ -n "$uuids" ] || continue
        for u in $uuids; do
            printf '%s\n' "$known" | grep -qx "$u" && continue
            echo "   reaping orphan vm $u on node-$idx (no control-plane row)"
            run_on "$handle" sudo systemctl stop otherix-agent >/dev/null 2>&1 || true
            run_on "$handle" sudo sh -c "pkill -f 'qemu-system.*$u' || true" >/dev/null 2>&1 || true
            run_on "$handle" sudo sh -c "rm -rf '$(smoke_state "$idx")/vms/$u' /var/lib/otherix/pools/*/vms/$u" >/dev/null 2>&1 || true
            reaped=1
        done
        [ "$reaped" = 1 ] && run_on "$handle" sudo systemctl start otherix-agent >/dev/null 2>&1 || true
    done
    [ "$reaped" = 1 ] && sleep 5
    return 0
}
