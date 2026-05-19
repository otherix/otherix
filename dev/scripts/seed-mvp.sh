#!/usr/bin/env bash
# seed-mvp — operator-driven Step 4 bootstrap orchestration.
#
# The agent runs continuously в State A polling-loop mode
# (systemd-managed) и we drive bootstrap by invoking
# `otherix-agent bootstrap` directly on the agent host, passing
# token + ca-fingerprint + cp-url + node-name + advertised-endpoint
# + migration-host as CLI flags. No bootstrap.env, no EnvironmentFile,
# no manual restart — the polling loop picks up the four files
# written by bootstrap within 5 seconds.
#
# Pool creation goes through `otherix pool create` — the CP returns
# the pool в `declared_pools` on heartbeat и the agent reconciler
# registers it locally. No SQL INSERT, no hardcoded UUID. См. Step 7
# comment block для full context.
#
# Idempotent — re-runs are safe:
#   - `otherix config add cluster --force` revokes prior token, mints fresh;
#   - `otherix-agent bootstrap` is а no-op when cert material is already
#     present (use --force к re-bootstrap explicitly);
#   - storage_pools / cluster_settings INSERT|UPDATE с ON CONFLICT;
#   - `otherix template create` falls through на 409 к materialise step;
#
# Required env:
#   OTHERIX_BOOTSTRAP_ADMIN_EMAIL    — admin email used для CP bootstrap
#   OTHERIX_BOOTSTRAP_ADMIN_PASSWORD — admin password
#
# Optional env (с defaults):
#   OTHERIX_DB_DSN        — psql connection string
#   OTHERIX_LIMA_INSTANCE — Lima VM name (default: otherix-dev) — only used on macOS
#   OTHERIX_CP_URL        — CP base URL для CLI auth (default: http://localhost:8080)
#   OTHERIX_NODE_ARCH     — node architecture (auto от uname)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CLI="${REPO_ROOT}/bin/otherix"

if [ ! -x "${CLI}" ]; then
    echo "otherix CLI not found at ${CLI} — run 'make build-cli' first" >&2
    exit 1
fi

: "${OTHERIX_DB_DSN:=postgres://otherix:otherix@127.0.0.1:5432/otherix?sslmode=disable}"
: "${OTHERIX_LIMA_INSTANCE:=otherix-dev}"
: "${OTHERIX_CP_URL:=http://localhost:8080}"
: "${OTHERIX_NODE_ARCH:=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')}"

if [ -z "${OTHERIX_BOOTSTRAP_ADMIN_EMAIL:-}" ] || [ -z "${OTHERIX_BOOTSTRAP_ADMIN_PASSWORD:-}" ]; then
    echo "OTHERIX_BOOTSTRAP_ADMIN_EMAIL и OTHERIX_BOOTSTRAP_ADMIN_PASSWORD must be set" >&2
    echo "(same env vars the CP boot hook uses к seed the admin row)" >&2
    exit 1
fi

# psql wrapper — prefers а local client, else falls back К docker exec.
if command -v psql >/dev/null 2>&1; then
    psql_run() { psql "${OTHERIX_DB_DSN}" "$@"; }
elif docker ps --format '{{.Names}}' | grep -q '^otherix-dev-postgres$'; then
    psql_run() { docker exec -i otherix-dev-postgres psql -U otherix -d otherix "$@"; }
else
    echo "psql not available locally и otherix-dev-postgres container not running — run 'make dev-up' first" >&2
    exit 1
fi

# Platform dispatch — Lima (macOS host) vs direct filesystem (Linux native).
case "$(uname -s)" in
    Darwin) PLATFORM=lima ;;
    Linux)  PLATFORM=native ;;
    *)      echo "unsupported platform: $(uname -s)" >&2; exit 1 ;;
esac

# Ubuntu Noble minimal cloudimg — ~150 MiB vs ~664 MiB for the server
# variant, ~4× faster smoke iteration. Switch к server-cloudimg only
# if а test needs features minimal strips (GUI, full runtimes).
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

NODE_NAME="${OTHERIX_NODE_NAME:-node-mvp}"
POOL_NAME="${OTHERIX_POOL_NAME:-pool-mvp}"
TEMPLATE_NAME="${OTHERIX_TEMPLATE_NAME:-ubuntu-noble-${OTHERIX_NODE_ARCH}-mvp}"

echo ">> seed-mvp"
echo "   platform        : ${PLATFORM}"
echo "   architecture    : ${OTHERIX_NODE_ARCH}"
echo "   node name       : ${NODE_NAME}"
echo "   pool name       : ${POOL_NAME}"
echo "   template name   : ${TEMPLATE_NAME}"
echo "   CP url          : ${OTHERIX_CP_URL}"
[ "${PLATFORM}" = "lima" ] && echo "   Lima instance   : ${OTHERIX_LIMA_INSTANCE}"

# --- Step 1: wait для CP к become reachable ----------------------------------

echo ""
echo ">> Step 1/8 — waiting for CP at ${OTHERIX_CP_URL}/healthz"
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
echo ">> Step 2/8 — configuring CLI cluster (mints long-lived API token)"
"${CLI}" config add cluster \
    --name dev \
    --server "${OTHERIX_CP_URL}" \
    --login "${OTHERIX_BOOTSTRAP_ADMIN_EMAIL}" \
    --password "${OTHERIX_BOOTSTRAP_ADMIN_PASSWORD}" \
    --force

# --- Step 3: mint join token + capture fingerprint ---------------------------

echo ""
echo ">> Step 3/8 — minting join token (pre-bound к ${NODE_NAME}, single-use)"
TOKEN_BUNDLE="$("${CLI}" node join-token create \
    --node-name "${NODE_NAME}" \
    --ttl 10m \
    --output json)"

JOIN_TOKEN="$(echo "${TOKEN_BUNDLE}" | jq -r '.token')"
CA_FINGERPRINT="$(echo "${TOKEN_BUNDLE}" | jq -r '.ca_fingerprint_sha256')"

if [ -z "${JOIN_TOKEN}" ] || [ "${JOIN_TOKEN}" = "null" ]; then
    echo "   ✗ token mint failed — JSON: ${TOKEN_BUNDLE}" >&2
    exit 1
fi

# Per-platform CP URL the agent sees + per-platform listen / migration host.
case "${PLATFORM}" in
    lima)
        CP_URL_FROM_AGENT="https://host.lima.internal:8443"
        ADVERTISED_ENDPOINT="https://127.0.0.1:9443"
        MIGRATION_HOST="0.0.0.0"
        ;;
    native)
        CP_URL_FROM_AGENT="https://localhost:8443"
        ADVERTISED_ENDPOINT="https://127.0.0.1:9443"
        MIGRATION_HOST="127.0.0.1"
        ;;
esac

# --- Step 4: run bootstrap subcommand на the agent host ----------------------

echo ""
echo ">> Step 4/8 — invoking otherix-agent bootstrap on agent host"

run_bootstrap() {
    case "${PLATFORM}" in
        lima)
            # No sudo — /opt/otherix/certs/ is chown'd к the Lima user
            # in the provision script (dev/lima/otherix-dev.yaml), и
            # /etc/otherix is also owned by the user. Running bootstrap
            # as the regular user writes the cert material с the
            # ownership the systemd unit expects (User=$LIMA_USER).
            limactl shell "${OTHERIX_LIMA_INSTANCE}" -- /usr/local/bin/otherix-agent bootstrap \
                --token "${JOIN_TOKEN}" \
                --ca-fingerprint "sha256:${CA_FINGERPRINT}" \
                --cp-url "${CP_URL_FROM_AGENT}" \
                --node-name "${NODE_NAME}" \
                --advertised-endpoint "${ADVERTISED_ENDPOINT}" \
                --migration-host "${MIGRATION_HOST}" \
                --migration-port-range-start 49152 \
                --migration-port-range-end 49251 \
                --listen "0.0.0.0:9443" \
                --force
            ;;
        native)
            # Linux user-mode dev: writes cert material under $XDG_CONFIG_HOME
            # so we override cert-dir + config-path к match the user-mode unit.
            mkdir -p "${HOME}/.config/otherix/certs"
            "${REPO_ROOT}/bin/otherix-agent" bootstrap \
                --token "${JOIN_TOKEN}" \
                --ca-fingerprint "sha256:${CA_FINGERPRINT}" \
                --cp-url "${CP_URL_FROM_AGENT}" \
                --node-name "${NODE_NAME}" \
                --advertised-endpoint "${ADVERTISED_ENDPOINT}" \
                --migration-host "${MIGRATION_HOST}" \
                --migration-port-range-start 49152 \
                --migration-port-range-end 49251 \
                --listen "127.0.0.1:9443" \
                --cert-dir "${HOME}/.config/otherix/certs" \
                --config-path "${HOME}/.config/otherix/agent.yaml" \
                --force
            ;;
    esac
}
run_bootstrap

# --- Step 5: ensure agent is running ----------------------------------------

echo ""
echo ">> Step 5/8 — ensuring agent service is running"
case "${PLATFORM}" in
    lima)
        # The unit auto-starts on Lima boot (enabled в otherix-dev.yaml provision);
        # `restart` here is а safety net против stuck State A loops at first deploy.
        limactl shell "${OTHERIX_LIMA_INSTANCE}" -- \
            sudo systemctl restart otherix-agent
        ;;
    native)
        systemctl --user start otherix-agent || \
            systemctl --user restart otherix-agent
        ;;
esac

# --- Step 6: wait для first heartbeat (node row populated) -------------------

echo ""
echo ">> Step 6/8 — waiting для bootstrap + first heartbeat (poll nodes.id where name='${NODE_NAME}')"
NODE_ID=""
for i in $(seq 1 60); do
    NODE_ID="$(psql_run -tA -c \
        "SELECT id FROM nodes WHERE name='${NODE_NAME}' AND deleted_at IS NULL;" \
        | tr -d '[:space:]' || true)"
    if [ -n "${NODE_ID}" ]; then
        echo "   ✓ node row present after ${i}s — id=${NODE_ID}"
        break
    fi
    sleep 1
done
if [ -z "${NODE_ID}" ]; then
    echo "   ✗ bootstrap did not complete after 60s" >&2
    case "${PLATFORM}" in
        lima)   echo "   inspect: limactl shell ${OTHERIX_LIMA_INSTANCE} sudo journalctl -u otherix-agent --no-pager | tail -50" >&2 ;;
        native) echo "   inspect: journalctl --user -u otherix-agent --no-pager | tail -50" >&2 ;;
    esac
    exit 1
fi

# --- Step 7: create storage pool via CLI + wait for agent reconcile -----------
#
# The agent's pool registry is name-keyed and reconciled autonomously
# via heartbeat. The CLI `otherix pool create` lands the
# row in storage_pools; the heartbeat handler returns it in
# `declared_pools`; the agent reconciler mkdir's the path и registers
# it locally; `reconciliation_status` flips к `ready` on the next
# heartbeat. No agent restart, no SQL INSERT, no hardcoded UUID.
#
# The wait-loop polls `otherix pool get` until reconciliation_status
# reaches `ready` (60 s budget — comfortably bigger than the typical
# CP heartbeat interval of 30 s).

POOL_PATH="${OTHERIX_POOL_PATH:-/opt/otherix/pools/default}"

echo ""
echo ">> Step 7/8 — creating storage pool '${POOL_NAME}' via CLI (agent auto-reconciles)"
"${CLI}" pool create "${POOL_NAME}" \
    --node "${NODE_NAME}" \
    --path "${POOL_PATH}" \
    --wait

"${CLI}" cluster set-default-pool "${POOL_NAME}"

# --- Step 8: create template via CLI (idempotent + materialise) ---------------

echo ""
echo ">> Step 8/8 — creating template '${TEMPLATE_NAME}' via CLI (compute-mode SHA)"
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
echo "   node     : ${NODE_NAME} (id=${NODE_ID})"
echo "   pool     : ${POOL_NAME} on ${NODE_NAME} (default=yes)"
echo "   template : ${TEMPLATE_NAME}"
echo ""
echo "next steps:"
echo "  ${CLI} node list"
echo "  ${CLI} vm create demo-vm --template ${TEMPLATE_NAME} --vcpus 2 --memory-mb 2048 --wait"
