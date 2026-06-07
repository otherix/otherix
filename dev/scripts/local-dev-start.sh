#!/usr/bin/env bash
# local-dev-start — one-shot orchestration of the full local dev stack.
#
# Brings up api-server (embedded etcd) + Lima VM + agent + CLI
# cluster config so that `./bin/otherix` works against a fresh local cluster
# without any further setup. No Postgres, no migrations - the api-server runs
# its own embedded etcd member (dev data dir .local/etcd).
#
# Sequence:
#    1. port pre-flight — fail if 8080 / 8443 / 9443 are in use
#    2. build           — api + agent + cli
#    3. bootstrap-dev   — Lima VM staging (idempotent — Lima detects existing VM)
#    4. lima readiness  — macOS only: verify VM shell responsive + agent binary staged
#    5. start api-server — background, PID + log in .local/run/ (boots embedded etcd)
#    6. wait /healthz   — 60s budget
#    7. seed-dev        - bootstrap protocol + CLI cluster (no template; VMs are created from an image URL)
#                         (the cluster default pool is CP-auto-provisioned)
#    8. node list       — final sanity check
#
# Fail-fast on existing state per locked decision (2a): if otherix-api is
# already running, exit with a clear "run local-dev-stop first" message.
# The admin row now lives in embedded etcd (wiped by local-dev-stop's
# etcd-reset), so the old Postgres admin-email pre-check is gone - a fresh
# data dir has no admin and the api-server seeds on boot; a reused data dir
# keeps the matching admin (BootstrapAdmin is idempotent).
#
# Default credentials (overridable):
#   OTHERIX_BOOTSTRAP_ADMIN_EMAIL    — admin@otherix.local
#   OTHERIX_BOOTSTRAP_ADMIN_PASSWORD — correct-horse-battery-staple

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RUN_DIR="${REPO_ROOT}/.local/run"
PID_FILE="${RUN_DIR}/otherix-api.pid"
LOG_FILE="${RUN_DIR}/otherix-api.log"

# Strip CLI auth env vars inherited from parent shell — local-dev-start mints
# a fresh cluster from scratch and stores credentials in ~/.otherix/config; any
# stale value (e.g. expired JWT in $OTHERIX_API_TOKEN from a previous session)
# would override the freshly stored token per CLI auth precedence chain
# (--token > $OTHERIX_API_TOKEN > stored cluster). Pre-flight wipe avoids
# the "Step 3 — token expired" footgun.
unset OTHERIX_API_TOKEN OTHERIX_SERVER OTHERIX_CONFIG OTHERIX_LOGIN OTHERIX_PASSWORD

: "${OTHERIX_BOOTSTRAP_ADMIN_EMAIL:=admin@otherix.local}"
: "${OTHERIX_BOOTSTRAP_ADMIN_PASSWORD:=correct-horse-battery-staple}"
export OTHERIX_BOOTSTRAP_ADMIN_EMAIL OTHERIX_BOOTSTRAP_ADMIN_PASSWORD

cd "${REPO_ROOT}"

echo ">> local-dev-start"
echo "   admin email      : ${OTHERIX_BOOTSTRAP_ADMIN_EMAIL}"
echo "   pid/log dir      : ${RUN_DIR}"
echo ""

# --- Fail-fast: check for existing api-server -------------------------------
if pgrep -f "otherix-api --config" >/dev/null 2>&1; then
    pids=$(pgrep -f "otherix-api --config" | tr '\n' ' ')
    echo "✗ otherix-api already running (PID ${pids})" >&2
    echo "  Run 'make local-dev-stop' first." >&2
    exit 1
fi
if [ -f "${PID_FILE}" ]; then
    stale_pid=$(cat "${PID_FILE}")
    if kill -0 "${stale_pid}" 2>/dev/null; then
        echo "✗ PID file ${PID_FILE} points to live process ${stale_pid}" >&2
        echo "  Run 'make local-dev-stop' first." >&2
        exit 1
    fi
    rm -f "${PID_FILE}"
fi

mkdir -p "${RUN_DIR}"

# check_port_free returns 0 if nothing listens on 127.0.0.1:$1, else 1
# with a human-readable failure on stderr. Uses bash's /dev/tcp pseudo-device
# to avoid a hard dependency on lsof / nc / ss across macOS + Linux —
# a successful TCP connect proves something is listening.
check_port_free() {
    local port="$1"; local label="$2"
    if (exec 3<>/dev/tcp/127.0.0.1/"${port}") 2>/dev/null; then
        exec 3<&- 3>&-
        echo "✗ port ${port} (${label}) already in use" >&2
        echo "  free it (lsof -i :${port}) or 'make local-dev-stop'." >&2
        return 1
    fi
    return 0
}

echo ">> Step 1/8 — Pre-flight port availability"
# Only check ports we bind ourselves later in the flow: the two CP listeners
# and the Lima agent forward. etcd's 2379/2380 are bound by the embedded
# member we start in Step 5, not pre-flighted here (a stale dev member on
# those ports surfaces as an "etcd start" failure in the api log).
port_fails=0
check_port_free 8080 "CP main listener"    || port_fails=$((port_fails+1))
check_port_free 8443 "CP agent listener"   || port_fails=$((port_fails+1))
check_port_free 9443 "agent-1 (Lima fwd)"  || port_fails=$((port_fails+1))
check_port_free 9444 "agent-2 (Lima fwd)"  || port_fails=$((port_fails+1))
if [ "${port_fails}" -gt 0 ]; then
    exit 1
fi
echo "   ✓ 8080 / 8443 / 9443 / 9444 all free"

echo ">> Step 2/8 — Build api + agent + cli"
make --no-print-directory build >/dev/null

echo ">> Step 3/8 — Stage Lima VM + agent (idempotent)"
make --no-print-directory bootstrap-dev

# Step 4 closes the "Lima says Started, but not usable" gap. `limactl start`
# returns once the VM boots, but cloud-init + provisioning (apt install,
# agent binary stage) run async and can take 30-60s longer on first start.
# Without this gate, seed-dev Step 4 (`limactl shell ... otherix-agent
# bootstrap`) fails with a cryptic "command not found" if the binary hasn't
# landed yet. Linux native skips entirely — bootstrap-dev-linux is
# synchronous (build + systemd unit install).
echo ">> Step 4/8 — Lima VM readiness (macOS only, both VMs)"
if [ "$(uname -s)" = "Darwin" ]; then
    for vm in otherix-dev-1 otherix-dev-2; do
        # Shell responsive — bounds Lima 'Started' to actual usability.
        # 60s budget (30 iterations × 2s) — first-start cloud-init occasionally
        # delays SSH availability past Lima's own readiness signal.
        lima_ready=0
        for _ in $(seq 1 30); do
            if limactl shell "${vm}" -- true >/dev/null 2>&1; then
                lima_ready=1
                break
            fi
            sleep 2
        done
        if [ "${lima_ready}" -ne 1 ]; then
            echo "✗ Lima VM ${vm} not responsive after 60s" >&2
            limactl list "${vm}" >&2 || true
            exit 1
        fi

        # Agent binary present — bounds cloud-init / `copy-agent-lima`
        # completion. 60s budget separate from shell readiness because the
        # binary copy can land after SSH becomes usable.
        agent_ready=0
        for _ in $(seq 1 30); do
            if limactl shell "${vm}" -- test -x /usr/local/bin/otherix-agent 2>/dev/null; then
                agent_ready=1
                break
            fi
            sleep 2
        done
        if [ "${agent_ready}" -ne 1 ]; then
            echo "✗ otherix-agent binary not staged in ${vm} after 60s" >&2
            echo "  inspect: limactl shell ${vm} sudo journalctl --no-pager | tail -50" >&2
            exit 1
        fi
        echo "   ✓ ${vm} responsive, agent binary staged"
    done
else
    echo "   (Linux native — bootstrap-dev is synchronous, no readiness gate needed)"
fi

echo ">> Step 5/8 — Start otherix-api in background"
nohup "${REPO_ROOT}/bin/otherix-api" --config "${REPO_ROOT}/dev/config/api.yaml" \
    > "${LOG_FILE}" 2>&1 &
api_pid=$!
echo "${api_pid}" > "${PID_FILE}"
echo "   PID ${api_pid} → ${LOG_FILE}"

echo ">> Step 6/8 — Wait for CP /healthz (60s budget)"
ready=0
for _ in $(seq 1 30); do
    if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
        ready=1
        break
    fi
    if ! kill -0 "${api_pid}" 2>/dev/null; then
        echo "✗ otherix-api died during startup" >&2
        echo "  Last 30 lines from ${LOG_FILE}:" >&2
        tail -30 "${LOG_FILE}" >&2
        rm -f "${PID_FILE}"
        exit 1
    fi
    sleep 2
done
if [ "${ready}" -ne 1 ]; then
    echo "✗ /healthz unreachable after 60s; api-server may be hung" >&2
    tail -30 "${LOG_FILE}" >&2
    exit 1
fi
echo "   ✓ CP reachable at http://localhost:8080"

echo ">> Step 7/8 — Bootstrap agent + seed cluster (seed-dev)"
make --no-print-directory seed-dev

# No pool-ready wait: VM create is admission-only (returns 201 pending) and the
# CP scheduler binds the VM once the default pool reconciles to ready, so
# `create -f demo-vm.yaml` no longer needs the pool to be ready first - the VM
# sits in `pending` (reason pool_not_ready) and converges on its own.
echo ">> Step 8/8 — Final sanity (otherix node list)"
"${REPO_ROOT}/bin/otherix" node list

cat <<EOF

>> local-dev-start complete

   etcd data  : ${REPO_ROOT}/.local/etcd (embedded member)
   api-server : http://localhost:8080 (PID $(cat "${PID_FILE}"))
   api log    : ${LOG_FILE}
   Lima VMs   : otherix-dev-1 (node-1) / otherix-dev-2 (node-2)
   CLI        : ${REPO_ROOT}/bin/otherix (cluster: dev)

Try:
   ./bin/otherix node list
   ./bin/otherix node get node-1            # WireGuard fabric block + peers
   make smoke-wireguard-mesh                # cross-host WG handshake
   ./bin/otherix create -f dev/manifests/demo-vm.yaml --wait   # demo VM (starts pending, converges to running)
   ./bin/otherix vm get demo                # status: pending (pool_not_ready) -> running as the pool reconciles
   ./bin/otherix vm console demo            # serial console; login ubuntu / demo, detach Ctrl+]

Stop + wipe:
   make local-dev-stop
EOF
