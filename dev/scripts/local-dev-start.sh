#!/usr/bin/env bash
# local-dev-start — one-shot orchestration of the full local dev stack.
#
# Brings up Postgres + api-server + Lima VM + agent + CLI cluster config
# so that `./bin/otherix` works against а fresh local cluster без any
# further setup.
#
# Sequence:
#    1. port pre-flight — fail если 8080 / 8443 / 9443 are in use
#    2. dev-up          — Postgres bind mount up
#    3. migrate-up      — schema
#    4. pre-flight      — fail если а stale admin row blocks bootstrap
#    5. build           — api + agent + cli
#    6. bootstrap-dev   — Lima VM staging (idempotent — Lima detects existing VM)
#    7. lima readiness  — macOS only: verify VM shell responsive + agent binary staged
#    8. start api-server — background, PID + log в .local/run/
#    9. wait /healthz   — 60s budget
#   10. seed-mvp        — bootstrap protocol + seed pool + template + CLI cluster
#   11. node list       — final sanity check
#
# Fail-fast on existing state per locked decision (2a): if otherix-api is
# already running OR the admin row email doesn't match the configured
# bootstrap email, exit с а clear "run local-dev-stop first" message.
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

# Strip CLI auth env vars inherited от parent shell — local-dev-start mints
# а fresh cluster from scratch и stores credentials в ~/.otherix/config; any
# stale value (e.g. expired JWT в $OTHERIX_API_TOKEN от а previous session)
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
        echo "✗ PID file ${PID_FILE} points к live process ${stale_pid}" >&2
        echo "  Run 'make local-dev-stop' first." >&2
        exit 1
    fi
    rm -f "${PID_FILE}"
fi

mkdir -p "${RUN_DIR}"

# check_port_free returns 0 если nothing listens on 127.0.0.1:$1, else 1
# с а human-readable failure on stderr. Uses bash's /dev/tcp pseudo-device
# к avoid а hard dependency on lsof / nc / ss across macOS + Linux —
# а successful TCP connect proves something is listening.
check_port_free() {
    local port="$1"; local label="$2"
    if (exec 3<>/dev/tcp/127.0.0.1/"${port}") 2>/dev/null; then
        exec 3<&- 3>&-
        echo "✗ port ${port} (${label}) already в use" >&2
        echo "  free it (lsof -i :${port}) или 'make local-dev-stop'." >&2
        return 1
    fi
    return 0
}

echo ">> Step 1/11 — Pre-flight port availability"
# Only check ports we bind ourselves later в the flow. 5432 is owned by
# Docker и `dev-up` is idempotent против already-running postgres
# (including the one local-dev-stop re-creates через db-reset), so it
# would always flag а false positive immediately after а stop.
port_fails=0
check_port_free 8080 "CP main listener"   || port_fails=$((port_fails+1))
check_port_free 8443 "CP agent listener"  || port_fails=$((port_fails+1))
check_port_free 9443 "agent (Lima fwd)"   || port_fails=$((port_fails+1))
if [ "${port_fails}" -gt 0 ]; then
    exit 1
fi
echo "   ✓ 8080 / 8443 / 9443 all free"

echo ">> Step 2/11 — Postgres dev dependencies"
make --no-print-directory dev-up >/dev/null
# Give Postgres а moment to accept connections (testcontainers-style wait).
for _ in $(seq 1 15); do
    if docker exec otherix-dev-postgres pg_isready -U otherix >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

echo ">> Step 3/11 — Apply migrations"
make --no-print-directory migrate-up >/dev/null

echo ">> Step 4/11 — Pre-flight check (admin row matches expected email)"
existing_email=$(docker exec otherix-dev-postgres \
    psql -U otherix -d otherix -tA -c \
    "select email from users where role='admin' and deleted_at is null limit 1" \
    2>/dev/null | tr -d '[:space:]' || true)
if [ -n "${existing_email}" ] && [ "${existing_email}" != "${OTHERIX_BOOTSTRAP_ADMIN_EMAIL}" ]; then
    echo "✗ admin row exists с email '${existing_email}'" >&2
    echo "  but local-dev-start is configured for '${OTHERIX_BOOTSTRAP_ADMIN_EMAIL}'." >&2
    echo "" >&2
    echo "  Run 'make local-dev-stop' to wipe state, then 'make local-dev-start'." >&2
    exit 1
fi
if [ -n "${existing_email}" ]; then
    echo "   admin row already present с matching email — will reuse"
else
    echo "   no admin row — api-server will seed on boot"
fi

echo ">> Step 5/11 — Build api + agent + cli"
make --no-print-directory build >/dev/null

echo ">> Step 6/11 — Stage Lima VM + agent (idempotent)"
make --no-print-directory bootstrap-dev

# Step 7 closes the "Lima says Started, but не usable" gap. `limactl start`
# returns once the VM boots, but cloud-init + provisioning (apt install,
# agent binary stage) run async и can take 30-60s longer на first start.
# Without this gate, seed-mvp Step 4 (`limactl shell ... otherix-agent
# bootstrap`) fails с а cryptic "command not found" if the binary hasn't
# landed yet. Linux native skips entirely — bootstrap-dev-linux is
# synchronous (build + systemd unit install).
echo ">> Step 7/11 — Lima VM readiness (macOS only)"
if [ "$(uname -s)" = "Darwin" ]; then
    # Shell responsive — bounds Lima 'Started' к actual usability.
    # 60s budget (30 iterations × 2s) — first-start cloud-init occasionally
    # delays SSH availability past Lima's own readiness signal.
    lima_ready=0
    for _ in $(seq 1 30); do
        if limactl shell otherix-dev -- true >/dev/null 2>&1; then
            lima_ready=1
            break
        fi
        sleep 2
    done
    if [ "${lima_ready}" -ne 1 ]; then
        echo "✗ Lima VM otherix-dev not responsive after 60s" >&2
        echo "  limactl list otherix-dev:" >&2
        limactl list otherix-dev >&2 || true
        exit 1
    fi

    # Agent binary present — bounds cloud-init / `copy-agent-lima`
    # completion. 60s budget separate от shell readiness because the
    # binary copy can land after SSH becomes usable.
    agent_ready=0
    for _ in $(seq 1 30); do
        if limactl shell otherix-dev -- test -x /usr/local/bin/otherix-agent 2>/dev/null; then
            agent_ready=1
            break
        fi
        sleep 2
    done
    if [ "${agent_ready}" -ne 1 ]; then
        echo "✗ otherix-agent binary not staged в Lima after 60s" >&2
        echo "  inspect: limactl shell otherix-dev sudo journalctl --no-pager | tail -50" >&2
        exit 1
    fi
    echo "   ✓ Lima VM responsive, agent binary staged"
else
    echo "   (Linux native — bootstrap-dev is synchronous, no readiness gate needed)"
fi

echo ">> Step 8/11 — Start otherix-api в background"
nohup "${REPO_ROOT}/bin/otherix-api" --config "${REPO_ROOT}/dev/config/api.yaml" \
    > "${LOG_FILE}" 2>&1 &
api_pid=$!
echo "${api_pid}" > "${PID_FILE}"
echo "   PID ${api_pid} → ${LOG_FILE}"

echo ">> Step 9/11 — Wait for CP /healthz (60s budget)"
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

echo ">> Step 10/11 — Bootstrap agent + seed cluster (seed-mvp)"
make --no-print-directory seed-mvp

echo ">> Step 11/11 — Final sanity (otherix node list)"
"${REPO_ROOT}/bin/otherix" node list

cat <<EOF

>> local-dev-start complete

   Postgres   : 127.0.0.1:5432 (otherix/otherix)
   api-server : http://localhost:8080 (PID $(cat "${PID_FILE}"))
   api log    : ${LOG_FILE}
   Lima VM    : otherix-dev (limactl shell otherix-dev)
   CLI        : ${REPO_ROOT}/bin/otherix (cluster: dev)

Try:
   ./bin/otherix node list
   ./bin/otherix pool list
   ./bin/otherix template list
   ./bin/otherix vm create --name demo --template ubuntu-noble-arm64-mvp --vcpus 2 --memory-mb 2048 --wait

Stop + wipe:
   make local-dev-stop
EOF
