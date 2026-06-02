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
# config by `copy-config-lima`). node-2 is bootstrap-only — it heartbeats,
# gets its overlay IP, and peers into the WG mesh, but carries no pool or
# template (the WG mesh smoke needs no VM, and the networking smoke pins
# its VM to node-1). On native Linux the stack stays single-node (node-1).
#
# Pool creation goes through `otherix pool create` — the CP returns
# the pool in `declared_pools` on heartbeat and the agent reconciler
# registers it locally. No SQL INSERT, no hardcoded UUID.
#
# Idempotent — re-runs are safe:
#   - `otherix config add cluster --force` revokes prior token, mints fresh;
#   - `otherix-agent bootstrap` re-issues cert material with --force and
#     never overwrites the staged config (operator-tuned settings survive);
#   - `otherix pool create` / cluster default-pool set are upsert-idempotent;
#   - `otherix template create` falls through on 409 to materialise step.
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

# Ubuntu Noble minimal cloudimg — ~150 MiB vs ~664 MiB for the server
# variant, ~4x faster smoke iteration.
case "${OTHERIX_NODE_ARCH}" in
    amd64)
        OTHERIX_TEMPLATE_URL="${OTHERIX_TEMPLATE_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img}"
        ;;
    arm64)
        OTHERIX_TEMPLATE_URL="${OTHERIX_TEMPLATE_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-arm64.img}"
        ;;
    *)
        echo "unsupported architecture: ${OTHERIX_NODE_ARCH}" >&2
        exit 1
        ;;
esac

NODE_NAME_1="${OTHERIX_NODE_NAME:-node-1}"
NODE_NAME_2="node-2"
POOL_NAME="${OTHERIX_POOL_NAME:-pool-mvp}"
TEMPLATE_NAME="${OTHERIX_TEMPLATE_NAME:-ubuntu-noble-${OTHERIX_NODE_ARCH}-mvp}"
POOL_PATH="${OTHERIX_POOL_PATH:-/opt/otherix/pools/default}"

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
echo "   pool name       : ${POOL_NAME} (on ${NODE_NAME_1})"
echo "   template name   : ${TEMPLATE_NAME}"
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

# --- Step 4: create storage pool on node-1 + wait for agent reconcile --------

echo ""
echo ">> Step 4 — creating storage pool '${POOL_NAME}' on ${NODE_NAME_1} (agent auto-reconciles)"
"${CLI}" pool create "${POOL_NAME}" \
    --node "${NODE_NAME_1}" \
    --path "${POOL_PATH}" \
    --wait

"${CLI}" cluster set-default-pool "${POOL_NAME}"

# --- Step 5: create template via CLI (idempotent + materialise) --------------

echo ""
echo ">> Step 5 — creating template '${TEMPLATE_NAME}' via CLI (compute-mode SHA)"
"${CLI}" template create "${TEMPLATE_NAME}" \
    --arch "${OTHERIX_NODE_ARCH}" \
    --os-family linux \
    --os-variant ubuntu-24.04 \
    --image-url "${OTHERIX_TEMPLATE_URL}" \
    --cloud-init "${REPO_ROOT}/dev/config/cloud-init-otherix.yaml" \
    --pool "${POOL_NAME}" \
    --wait

# --- Done --------------------------------------------------------------------

echo ""
echo ">> seed-mvp complete"
echo "   node 1   : ${NODE_NAME_1}"
[ "${PLATFORM}" = "lima" ] && echo "   node 2   : ${NODE_NAME_2}"
echo "   pool     : ${POOL_NAME} on ${NODE_NAME_1} (default=yes)"
echo "   template : ${TEMPLATE_NAME}"
echo ""
echo "next steps:"
echo "  ${CLI} node list"
echo "  ${CLI} node get ${NODE_NAME_1}   # WireGuard fabric block + peers"
