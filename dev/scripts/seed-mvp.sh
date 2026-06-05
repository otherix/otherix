#!/usr/bin/env bash
# seed-mvp — operator-driven Step 4 bootstrap orchestration.
#
# The agent runs continuously in State A polling-loop mode
# (systemd-managed) and we drive bootstrap by invoking
# `otherix-agent bootstrap` directly on the agent host, passing
# token + ca-fingerprint + cp-url + node-name + advertised-endpoint
# + migration-host as CLI flags. No bootstrap.env, no EnvironmentFile,
# no manual restart — the polling loop picks up the four files
# written by bootstrap within 5 seconds.
#
# Two-node dev stack (macOS / Lima): node-1 on otherix-dev-1 and node-2
# on otherix-dev-2, each with its own CP->agent advertised endpoint
# (127.0.0.1:9443 / :9444 via the per-VM Lima port-forward) and its own
# WireGuard advertised endpoint (its user-v2 IP, baked into the staged
# config by `copy-config-lima`). node-2 is bootstrap-only for the base seed: it
# heartbeats, gets its overlay IP, peers into the WG mesh, and (like every node)
# has the cluster default pool auto-provisioned on it by the CP, and creates no
# VM on it (the WG mesh smoke needs no VM, and the networking smoke pins its VM
# to node-1). On native Linux the stack stays single-node (node-1).
#
# The storage pool is NOT created here: the CP auto-provisions the cluster
# default pool (`default`) on every node as it reaches ready (PR #15), and that
# is the cluster default - so seed-mvp neither runs `pool create` nor sets the
# default. There is no template entity: a VM is created directly from an image
# URL (`vm create --image-url ... --arch ...`) and the agent materializes the
# image into the pool's basename-keyed cache on first use (IfNotPresent).
#
# Idempotent - re-runs are safe:
#   - `otherix config add cluster --force` revokes prior token, mints fresh;
#   - `otherix-agent bootstrap` re-issues cert material with --force and
#     never overwrites the staged config (operator-tuned settings survive).
#
# Required env:
#   OTHERIX_BOOTSTRAP_ADMIN_EMAIL    — admin email used for CP bootstrap
#   OTHERIX_BOOTSTRAP_ADMIN_PASSWORD — admin password
#
# Optional env (with defaults):
#   OTHERIX_LIMA_INSTANCE_1 — node-1 Lima VM (default: otherix-dev-1)
#   OTHERIX_LIMA_INSTANCE_2 — node-2 Lima VM (default: otherix-dev-2)
#   OTHERIX_CP_URL          — CP base URL for CLI auth (default: http://localhost:8080)
#   OTHERIX_NODE_ARCH       — node architecture (auto from uname)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CLI="${REPO_ROOT}/bin/otherix"

if [ ! -x "${CLI}" ]; then
    echo "otherix CLI not found at ${CLI} — run 'make build-cli' first" >&2
    exit 1
fi

: "${OTHERIX_LIMA_INSTANCE_1:=otherix-dev-1}"
: "${OTHERIX_LIMA_INSTANCE_2:=otherix-dev-2}"
: "${OTHERIX_CP_URL:=http://localhost:8080}"
: "${OTHERIX_NODE_ARCH:=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')}"

if [ -z "${OTHERIX_BOOTSTRAP_ADMIN_EMAIL:-}" ] || [ -z "${OTHERIX_BOOTSTRAP_ADMIN_PASSWORD:-}" ]; then
    echo "OTHERIX_BOOTSTRAP_ADMIN_EMAIL and OTHERIX_BOOTSTRAP_ADMIN_PASSWORD must be set" >&2
    echo "(same env vars the CP boot hook uses to seed the admin row)" >&2
    exit 1
fi

# node_id_by_name reads the node's UUID through the CLI (admin-authed by the
# time this runs). Prints the id on stdout, or nothing when the node is not yet
# visible. jq-free: the node JSON's "id" is a UUID string.
node_id_by_name() {
    local name="$1"
    "${CLI}" node get "${name}" --output json 2>/dev/null \
        | grep -oE '"id"[[:space:]]*:[[:space:]]*"[0-9a-fA-F-]{36}"' \
        | head -1 \
        | grep -oE '[0-9a-fA-F-]{36}'
}

# Platform dispatch — Lima (macOS host, two VMs) vs direct filesystem (Linux
# native, single agent).
case "$(uname -s)" in
    Darwin) PLATFORM=lima ;;
    Linux)  PLATFORM=native ;;
    *)      echo "unsupported platform: $(uname -s)" >&2; exit 1 ;;
esac

NODE_NAME_1="${OTHERIX_NODE_NAME:-node-1}"
NODE_NAME_2="node-2"
# POOL_NAME is display-only: the cluster default pool is provisioned by the CP
# from its code default (default_pool_name "default") and auto-created on every
# node as it reaches ready. seed-mvp no longer creates a pool or sets the
# cluster default.
POOL_NAME="default"

# Per-platform CP URL the agent sees + per-platform migration host.
case "${PLATFORM}" in
    lima)
        CP_URL_FROM_AGENT="https://host.lima.internal:8443"
        MIGRATION_HOST="0.0.0.0"
        ;;
    native)
        CP_URL_FROM_AGENT="https://localhost:8443"
        MIGRATION_HOST="127.0.0.1"
        ;;
esac

echo ">> seed-mvp"
echo "   platform        : ${PLATFORM}"
echo "   architecture    : ${OTHERIX_NODE_ARCH}"
echo "   node 1          : ${NODE_NAME_1}"
[ "${PLATFORM}" = "lima" ] && echo "   node 2          : ${NODE_NAME_2}"
echo "   default pool    : ${POOL_NAME} (auto-provisioned by the CP on ready nodes)"
echo "   CP url          : ${OTHERIX_CP_URL}"
if [ "${PLATFORM}" = "lima" ]; then
    echo "   Lima instances  : ${OTHERIX_LIMA_INSTANCE_1} (node-1) / ${OTHERIX_LIMA_INSTANCE_2} (node-2)"
fi

# --- Step 1: wait for CP to become reachable ----------------------------------

echo ""
echo ">> Step 1 — waiting for CP at ${OTHERIX_CP_URL}/healthz"
for i in $(seq 1 30); do
    if curl -fsS "${OTHERIX_CP_URL}/healthz" >/dev/null 2>&1; then
        echo "   ✓ CP reachable"
        break
    fi
    if [ "${i}" -eq 30 ]; then
        echo "   ✗ CP not reachable after 30s — is 'make run-api-dev' running?" >&2
        exit 1
    fi
    sleep 1
done

# --- Step 2: configure CLI cluster -------------------------------------------

echo ""
echo ">> Step 2 — configuring CLI cluster (mints long-lived API token)"
"${CLI}" config add cluster \
    --name dev \
    --server "${OTHERIX_CP_URL}" \
    --login "${OTHERIX_BOOTSTRAP_ADMIN_EMAIL}" \
    --password "${OTHERIX_BOOTSTRAP_ADMIN_PASSWORD}" \
    --force

# --- bootstrap_node: mint token, run bootstrap, start agent, wait heartbeat ---
#
# Args: 1=node-name  2=advertised-endpoint  3=listen-addr  4=lima-instance("" native)
bootstrap_node() {
    local node_name="$1" advertised="$2" listen="$3" lima_vm="$4"

    echo ""
    echo ">> bootstrap ${node_name} (advertised ${advertised})"

    local bundle token fp
    bundle="$("${CLI}" node join-token create --node-name "${node_name}" --ttl 10m --output json)"
    token="$(echo "${bundle}" | jq -r '.token')"
    fp="$(echo "${bundle}" | jq -r '.ca_fingerprint_sha256')"
    if [ -z "${token}" ] || [ "${token}" = "null" ]; then
        echo "   ✗ token mint failed for ${node_name} — JSON: ${bundle}" >&2
        exit 1
    fi

    if [ -n "${lima_vm}" ]; then
        # No sudo — /opt/otherix/certs + /etc/otherix are chown'd to the Lima
        # user in the provision script, so cert material lands with the
        # ownership the systemd unit (User=$LIMA_USER) expects.
        limactl shell "${lima_vm}" -- /usr/local/bin/otherix-agent bootstrap \
            --token "${token}" \
            --ca-fingerprint "sha256:${fp}" \
            --cp-url "${CP_URL_FROM_AGENT}" \
            --node-name "${node_name}" \
            --advertised-endpoint "${advertised}" \
            --migration-host "${MIGRATION_HOST}" \
            --migration-port-range-start 49152 \
            --migration-port-range-end 49251 \
            --listen "0.0.0.0:9443" \
            --force
        # The unit auto-starts on Lima boot; restart is a safety net against a
        # stuck State A loop at first deploy.
        limactl shell "${lima_vm}" -- sudo systemctl restart otherix-agent
    else
        mkdir -p "${HOME}/.config/otherix/certs"
        "${REPO_ROOT}/bin/otherix-agent" bootstrap \
            --token "${token}" \
            --ca-fingerprint "sha256:${fp}" \
            --cp-url "${CP_URL_FROM_AGENT}" \
            --node-name "${node_name}" \
            --advertised-endpoint "${advertised}" \
            --migration-host "${MIGRATION_HOST}" \
            --migration-port-range-start 49152 \
            --migration-port-range-end 49251 \
            --listen "${listen}" \
            --cert-dir "${HOME}/.config/otherix/certs" \
            --config-path "${HOME}/.config/otherix/agent.yaml" \
            --force
        systemctl --user start otherix-agent || systemctl --user restart otherix-agent
    fi

    local id=""
    for i in $(seq 1 60); do
        id="$(node_id_by_name "${node_name}" || true)"
        if [ -n "${id}" ]; then
            echo "   ✓ ${node_name} present after ${i}s — id=${id}"
            return 0
        fi
        sleep 1
    done
    echo "   ✗ ${node_name} bootstrap did not complete after 60s" >&2
    if [ -n "${lima_vm}" ]; then
        echo "   inspect: limactl shell ${lima_vm} sudo journalctl -u otherix-agent --no-pager | tail -50" >&2
    else
        echo "   inspect: journalctl --user -u otherix-agent --no-pager | tail -50" >&2
    fi
    exit 1
}

# --- Step 3: bootstrap node-1 (and node-2 on Lima) ---------------------------

if [ "${PLATFORM}" = "lima" ]; then
    bootstrap_node "${NODE_NAME_1}" "https://127.0.0.1:9443" "0.0.0.0:9443" "${OTHERIX_LIMA_INSTANCE_1}"
    bootstrap_node "${NODE_NAME_2}" "https://127.0.0.1:9444" "0.0.0.0:9443" "${OTHERIX_LIMA_INSTANCE_2}"
else
    bootstrap_node "${NODE_NAME_1}" "https://127.0.0.1:9443" "127.0.0.1:9443" ""
fi

# --- Done --------------------------------------------------------------------
# There is no template step: a VM is created directly from an image URL. The
# cluster default pool (${POOL_NAME}) is auto-provisioned by the CP on every
# node as it reaches ready, and the agent materializes the image into that
# pool's basename-keyed cache on the first `vm create --image-url ... --arch ...`
# (IfNotPresent), so no warm-up is required.

echo ""
echo ">> seed-mvp complete"
echo "   node 1   : ${NODE_NAME_1}"
[ "${PLATFORM}" = "lima" ] && echo "   node 2   : ${NODE_NAME_2}"
echo "   pool     : ${POOL_NAME} (cluster default, CP-auto-provisioned on ready nodes)"
echo ""
echo "next steps:"
echo "  ${CLI} node list"
echo "  ${CLI} node get ${NODE_NAME_1}   # WireGuard fabric block + peers"
echo "  ${CLI} create -f ${REPO_ROOT}/dev/manifests/demo-vm.yaml --wait   # demo VM (edit url/arch for amd64)"
