#!/usr/bin/env bash
# local-dev-stop — destructive teardown of the local dev stack.
#
# Stops everything brought up by `make local-dev-start` AND wipes the
# embedded-etcd state via `make etcd-reset`. Per locked decision (3a) — no
# confirmation prompt; user runs this when they want clean slate.
#
# Sequence:
#   1. Stop otherix-api  — SIGTERM, wait for graceful shutdown (35s budget),
#                          SIGKILL fallback
#   2. clean-dev         — stop + delete Lima VM
#   3. etcd-reset        — wipe the embedded-etcd data dir (.local/etcd)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RUN_DIR="${REPO_ROOT}/.local/run"
PID_FILE="${RUN_DIR}/otherix-api.pid"

cd "${REPO_ROOT}"

echo ">> local-dev-stop"
echo ""

echo ">> Step 1/3 — Stop otherix-api (graceful, 35s budget)"
stopped=0
if [ -f "${PID_FILE}" ]; then
    pid=$(cat "${PID_FILE}")
    if kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
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

# Defense-in-depth: any orphan otherix-api process not tracked by PID file.
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
if [ "${stopped}" -eq 0 ] && [ -z "${orphans}" ]; then
    echo "   no api-server processes to stop"
fi

echo ">> Step 2/3 — Stop + delete Lima VM"
make --no-print-directory clean-dev

echo ">> Step 3/3 — Reset embedded etcd (wipe data dir)"
make --no-print-directory etcd-reset >/dev/null

echo ""
echo ">> local-dev-stop complete — system reset"
echo "   etcd data dir wiped (.local/etcd)"
echo "   Lima VMs destroyed (otherix-dev-1, otherix-dev-2)"
echo "   api-server stopped"
echo ""
echo "   Run 'make local-dev-start' to bring everything back up."
