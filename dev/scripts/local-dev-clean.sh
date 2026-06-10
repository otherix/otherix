#!/usr/bin/env bash
# local-dev-clean — pristine teardown of the local dev stack.
#
# Runs the destructive `local-dev-stop` (stop api + delete Lima VMs / netns +
# wipe etcd/pki) and then removes the remaining dev residue so the repo looks
# like the stack was never started:
#   1. local-dev-stop.sh  — stop api + clean-dev + etcd-reset
#   2. rm -rf .local       — logs/pid + etcd + pki (superset of etcd-reset)
#   3. otherix config remove cluster — drop the dev cluster from ~/.otherix
#
# Does NOT touch bin/ (that is `make clean`). Steps 2-3 are best-effort so a
# partial stack still cleans up.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CLI="${REPO_ROOT}/bin/otherix"

cd "${REPO_ROOT}"

echo ">> local-dev-clean"
echo ""

echo ">> Step 1/3 — local-dev-stop (stop api + delete VMs/netns + wipe etcd/pki)"
bash "${SCRIPT_DIR}/local-dev-stop.sh"

echo ">> Step 2/3 — remove .local/ (logs, pid, etcd, pki)"
rm -rf "${REPO_ROOT}/.local"
echo "   ✓ .local removed"

echo ">> Step 3/3 — remove the dev cluster from the CLI config (~/.otherix)"
if [ -x "${CLI}" ]; then
    if "${CLI}" config remove cluster >/dev/null 2>&1; then
        echo "   ✓ CLI cluster 'cluster' removed"
    else
        echo "   (no 'cluster' entry in the CLI config — nothing to remove)"
    fi
else
    echo "   (bin/otherix not built — skipping CLI config cleanup)"
fi

echo ""
echo ">> local-dev-clean complete — pristine slate"
echo "   Run 'make local-dev-start' to bring everything back up."
