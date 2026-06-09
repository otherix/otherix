# dev/smoke/lib.sh — platform abstraction shared by dev/smoke/*/run.sh.
#
# The two-node dev stack is backed by two Lima VMs on macOS and by two network
# namespaces (otns1/otns2) on native Linux. Smoke scripts source this file and
# target nodes through run_on / the per-node handles instead of calling limactl
# directly, so the same smoke runs unchanged on both platforms:
#
#   macOS : run_on <handle> <cmd...> -> limactl shell <handle> -- <cmd...>
#   Linux : run_on <handle> <cmd...> -> sudo ip netns exec <handle> <cmd...>
#
# Handles and state dirs (the agent state_path root, where vms/<id>/serial.log
# and wg/private.key live) are exposed per node:
#   SMOKE_HANDLE_1 / SMOKE_HANDLE_2   node-1 / node-2 execution handle
#   SMOKE_STATE_1  / SMOKE_STATE_2    node-1 / node-2 agent state_path root
#
# This file is sourced, not executed; it sets shell variables in the caller.
# shellcheck shell=bash
# shellcheck disable=SC2034  # SMOKE_* vars are the public API, consumed by sourcing scripts

case "$(uname -s)" in
    Darwin)
        SMOKE_PLATFORM="lima"
        SMOKE_HANDLE_1="${VM1:-otherix-dev-1}"
        SMOKE_HANDLE_2="${VM2:-otherix-dev-2}"
        SMOKE_STATE_1="/var/lib/otherix"
        SMOKE_STATE_2="/var/lib/otherix"
        ;;
    Linux)
        SMOKE_PLATFORM="netns"
        SMOKE_HANDLE_1="otns1"
        SMOKE_HANDLE_2="otns2"
        SMOKE_STATE_1="/var/lib/otherix/dev/node1"
        SMOKE_STATE_2="/var/lib/otherix/dev/node2"
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

# smoke_handle <1|2> / smoke_state <1|2> — accessors for the per-node handle and
# state_path root, for scripts that index nodes dynamically.
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
