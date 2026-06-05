#!/usr/bin/env bash
# restart-api-dev — restart the dev api-server in place, preserving state.
#
# Stops the running otherix-api (graceful SIGTERM, SIGKILL fallback) and starts
# a fresh one in the background against dev/config/api.yaml, reusing the same
# PID/log files as `make local-dev-start`. The embedded-etcd data dir, Lima VM,
# agent, and seeded cluster state are left untouched — only the api-server
# process is swapped, so an already-bootstrapped dev stand keeps its nodes and
# VMs. Use after rebuilding the api binary to pick up code changes without a
# destructive `make local-dev-stop` / `local-dev-start` cycle.
#
# The make target rebuilds the binary first (restart-api-dev: build-api); this
# script only stops and starts.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RUN_DIR="${REPO_ROOT}/.local/run"
PID_FILE="${RUN_DIR}/otherix-api.pid"
LOG_FILE="${RUN_DIR}/otherix-api.log"
CONFIG="${REPO_ROOT}/dev/config/api.yaml"

cd "${REPO_ROOT}"
mkdir -p "${RUN_DIR}"

echo ">> restart-api-dev"
echo ""

echo ">> Step 1/3 — Stop otherix-api (graceful, 35s budget)"
if [ -f "${PID_FILE}" ]; then
    pid=$(cat "${PID_FILE}")
    if kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
        stopped=0
        for _ in $(seq 1 35); do
            if ! kill -0 "${pid}" 2>/dev/null; then
                stopped=1
                break
            fi
            sleep 1
        done
        if [ "${stopped}" -eq 1 ]; then
            echo "   ✓ PID ${pid} stopped gracefully"
        else
            echo "   ! PID ${pid} did not exit within 35s; SIGKILL"
            kill -9 "${pid}" 2>/dev/null || true
        fi
    else
        echo "   PID file present but ${pid} not alive"
    fi
    rm -f "${PID_FILE}"
fi

# Defense-in-depth: any orphan otherix-api process not tracked by the PID file.
orphans=$(pgrep -f "otherix-api --config" 2>/dev/null || true)
if [ -n "${orphans}" ]; then
    echo "   found orphan otherix-api PIDs: ${orphans} — killing"
    echo "${orphans}" | xargs kill 2>/dev/null || true
    sleep 2
    still=$(pgrep -f "otherix-api --config" 2>/dev/null || true)
    if [ -n "${still}" ]; then
        echo "${still}" | xargs kill -9 2>/dev/null || true
    fi
fi

if [ ! -x "${REPO_ROOT}/bin/otherix-api" ]; then
    echo "✗ ${REPO_ROOT}/bin/otherix-api not found — run 'make build-api' first" >&2
    exit 1
fi

echo ">> Step 2/3 — Start otherix-api in background"
nohup "${REPO_ROOT}/bin/otherix-api" --config "${CONFIG}" \
    > "${LOG_FILE}" 2>&1 &
api_pid=$!
echo "${api_pid}" > "${PID_FILE}"
echo "   PID ${api_pid} → ${LOG_FILE}"

echo ">> Step 3/3 — Wait for CP /healthz (60s budget)"
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
echo "   ✓ CP reachable at http://localhost:8080 (PID ${api_pid})"
