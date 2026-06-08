#!/usr/bin/env bash
# linux-multinode.sh — privileged lifecycle of the native-Linux two-node dev
# topology. The ONLY sudo surface of the Linux dev flow.
#
# Subcommands (all require root):
#   up                  create bridge + 2 netns + veth + host NAT + state dirs
#   down [--wipe]       tear it all down; --wipe also removes /opt/otherix/dev
#   bootstrap N TOK FP  render node-N config, redeem join token inside otnsN
#   start N             launch node-N agent in its net+mount namespace
#   stop                kill both agents
#   restart             stop + start both
#
# Node N in {1,2}: netns otnsN, underlay IP 10.77.0.N, host veth veth-otnsN,
# state dir /opt/otherix/dev/nodeN, node name node-N.
#
# netns isolates the agent's fixed-name network resources (otwg0, otherix-nat,
# otvx*/otb*); the mount namespace + private bind mount in `start` isolates the
# cluster-default pool path (/opt/otherix/pools -> /opt/otherix/dev/nodeN/pools).
# No agent Go code is involved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

AGENT_BIN="${REPO_ROOT}/bin/otherix-agent"
TEMPLATE="${REPO_ROOT}/dev/config/agent-linux-multinode.yaml"

BRIDGE="otdev0"
BRIDGE_IP="10.77.0.254"
PREFIX="24"
SUBNET="10.77.0.0/24"
NAT_TABLE="otherix-dev-nat"
STATE_ROOT="/opt/otherix/dev"
POOL_MOUNTPOINT="/opt/otherix/pools"

node_ns()   { echo "otns$1"; }
node_ip()   { echo "10.77.0.$1"; }
node_veth() { echo "veth-otns$1"; }
node_dir()  { echo "${STATE_ROOT}/node$1"; }

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        echo "✗ must run as root (use sudo): $0 $*" >&2
        exit 1
    fi
}

host_egress_iface() {
    ip route show default 2>/dev/null | awk '/default/ {print $5; exit}'
}

preflight() {
    local missing=0
    for bin in ip nft unshare; do
        command -v "${bin}" >/dev/null 2>&1 || { echo "✗ missing dependency: ${bin}" >&2; missing=1; }
    done
    if ! command -v qemu-system-x86_64 >/dev/null 2>&1 \
        && ! command -v qemu-system-aarch64 >/dev/null 2>&1; then
        echo "✗ missing dependency: qemu-system-x86_64 or qemu-system-aarch64" >&2
        missing=1
    fi
    [ -e /dev/kvm ] || { echo "✗ /dev/kvm not present (KVM unavailable)" >&2; missing=1; }
    if ! lsmod 2>/dev/null | grep -qw wireguard && ! modprobe wireguard 2>/dev/null; then
        echo "✗ wireguard kernel module not loadable" >&2
        missing=1
    fi
    if ! ls /usr/share/OVMF/OVMF_CODE*.fd >/dev/null 2>&1 \
        && ! ls /usr/share/AAVMF/AAVMF_CODE.fd >/dev/null 2>&1; then
        echo "! warning: no OVMF/AAVMF firmware found — UEFI VMs may fail to boot" >&2
    fi
    [ -x "${AGENT_BIN}" ] || { echo "✗ agent binary not built: ${AGENT_BIN} (run 'make build-agent')" >&2; missing=1; }
    [ "${missing}" -eq 0 ] || { echo "✗ preflight failed; install the missing dependencies" >&2; exit 1; }
}

up() {
    preflight

    if ! ip link show "${BRIDGE}" >/dev/null 2>&1; then
        ip link add "${BRIDGE}" type bridge
        ip addr add "${BRIDGE_IP}/${PREFIX}" dev "${BRIDGE}"
        ip link set "${BRIDGE}" up
    fi

    local n ns ip hveth
    for n in 1 2; do
        ns="$(node_ns "${n}")"; ip="$(node_ip "${n}")"; hveth="$(node_veth "${n}")"
        ip netns list | grep -qw "${ns}" || ip netns add "${ns}"
        ip netns exec "${ns}" ip link set lo up
        # Create the veth pair once; then (re)apply L3 unconditionally with
        # idempotent replace ops so a half-built node — veth present but IP/route
        # missing from a prior interrupted up — is repaired on the next up.
        if ! ip link show "${hveth}" >/dev/null 2>&1; then
            ip link add "${hveth}" type veth peer name eth0 netns "${ns}"
        fi
        ip link set "${hveth}" master "${BRIDGE}"
        ip link set "${hveth}" up
        ip netns exec "${ns}" ip addr replace "${ip}/${PREFIX}" dev eth0
        ip netns exec "${ns}" ip link set eth0 up
        ip netns exec "${ns}" ip route replace default via "${BRIDGE_IP}"
        mkdir -p "$(node_dir "${n}")/certs" \
                 "$(node_dir "${n}")/vms" \
                 "$(node_dir "${n}")/pools/default/images" \
                 "$(node_dir "${n}")/pools/default/vms" \
                 "$(node_dir "${n}")/wg"
        chmod 0750 "$(node_dir "${n}")/certs"
    done

    mkdir -p "${POOL_MOUNTPOINT}"

    sysctl -wq net.ipv4.ip_forward=1
    if ! nft list table ip "${NAT_TABLE}" >/dev/null 2>&1; then
        local egress; egress="$(host_egress_iface)"
        if [ -z "${egress}" ]; then
            echo "! warning: no default route found — VM internet egress (image pull) will not work" >&2
        else
            nft add table ip "${NAT_TABLE}"
            nft add chain ip "${NAT_TABLE}" postrouting '{ type nat hook postrouting priority 100 ; }'
            nft add rule ip "${NAT_TABLE}" postrouting ip saddr "${SUBNET}" oifname "${egress}" masquerade
        fi
    fi

    echo ">> linux-multinode up: bridge ${BRIDGE} ${BRIDGE_IP}, netns otns1/otns2, NAT ${NAT_TABLE}"
}

down() {
    local wipe="${1:-}"
    stop_all || true

    local n ns hveth
    for n in 1 2; do
        ns="$(node_ns "${n}")"
        ip netns list | grep -qw "${ns}" && ip netns del "${ns}"
    done
    for n in 1 2; do
        hveth="$(node_veth "${n}")"
        ip link show "${hveth}" >/dev/null 2>&1 && ip link del "${hveth}"
    done
    ip link show "${BRIDGE}" >/dev/null 2>&1 && ip link del "${BRIDGE}"
    nft list table ip "${NAT_TABLE}" >/dev/null 2>&1 && nft delete table ip "${NAT_TABLE}"

    if [ "${wipe}" = "--wipe" ]; then
        rm -rf "${STATE_ROOT}"
        rmdir "${POOL_MOUNTPOINT}" 2>/dev/null || true
    fi
    echo ">> linux-multinode down${wipe:+ (wiped state)}"
}

do_bootstrap() {
    local n="$1" token="$2" fp="$3"
    local ns ip dir
    ns="$(node_ns "${n}")"; ip="$(node_ip "${n}")"; dir="$(node_dir "${n}")"

    # Render the per-node config BEFORE bootstrap. The agent writes a config only
    # when absent (cmd/agent/bootstrap.go:205), so our rendered file — with the
    # wireguard block — survives; bootstrap only (re)writes cert material.
    sed -e "s|__NODE_IP__|${ip}|g" -e "s|__NODE_DIR__|${dir}|g" \
        "${TEMPLATE}" > "${dir}/agent.yaml"

    ip netns exec "${ns}" "${AGENT_BIN}" bootstrap \
        --token "${token}" \
        --ca-fingerprint "sha256:${fp}" \
        --cp-url "https://${BRIDGE_IP}:8443" \
        --node-name "node-${n}" \
        --advertised-endpoint "https://${ip}:9443" \
        --migration-host "${ip}" \
        --migration-port-range-start 49152 \
        --migration-port-range-end 49251 \
        --listen "0.0.0.0:9443" \
        --cert-dir "${dir}/certs" \
        --config-path "${dir}/agent.yaml" \
        --force
    echo ">> node-${n} bootstrapped (cert material in ${dir}/certs)"
}

do_start() {
    local n="$1"
    local ns dir
    ns="$(node_ns "${n}")"; dir="$(node_dir "${n}")"

    # ip netns exec -> unshare(mount) -> sh -> exec agent: a single process tree,
    # so the recorded PID IS the agent. The private bind redirects the
    # cluster-default pool path to this node's storage; it is invisible to the
    # host and to the peer agent and vanishes when the process exits.
    nohup ip netns exec "${ns}" \
        unshare --mount --propagation private \
        sh -c "mount --bind '${dir}/pools' '${POOL_MOUNTPOINT}' && exec '${AGENT_BIN}' serve --config '${dir}/agent.yaml'" \
        > "${dir}/agent.log" 2>&1 &
    echo $! > "${dir}/agent.pid"
    echo ">> node-${n} agent started (pid $(cat "${dir}/agent.pid"))"
}

stop_all() {
    local n pidf pid
    for n in 1 2; do
        pidf="$(node_dir "${n}")/agent.pid"
        [ -f "${pidf}" ] || continue
        pid="$(cat "${pidf}")"
        if kill -0 "${pid}" 2>/dev/null; then
            kill "${pid}" 2>/dev/null || true
            for _ in $(seq 1 10); do
                kill -0 "${pid}" 2>/dev/null || break
                sleep 1
            done
            kill -9 "${pid}" 2>/dev/null || true
        fi
        rm -f "${pidf}"
    done
    echo ">> agents stopped"
}

restart() {
    stop_all
    do_start 1
    do_start 2
}

main() {
    local cmd="${1:-}"; shift || true
    require_root "${cmd}"
    case "${cmd}" in
        up)        up ;;
        down)      down "${1:-}" ;;
        bootstrap) do_bootstrap "$1" "$2" "$3" ;;
        start)     do_start "$1" ;;
        stop)      stop_all ;;
        restart)   restart ;;
        *) echo "usage: $0 {up|down [--wipe]|bootstrap N TOKEN FP|start N|stop|restart}" >&2; exit 1 ;;
    esac
}

main "$@"
