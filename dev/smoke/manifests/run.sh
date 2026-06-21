#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Declarative-manifests operator smoke - drives the `otherix` CLI as a
# real operator against a real agent on the Lima VM, exercising the
# YAML-manifest surface end to end:
#   - `otherix create -f` (single multi-doc apply: kind Network + kind VM, --wait)
#   - `otherix vm get  -o yaml` / `otherix network get -o yaml` round-trip
#   - `otherix delete -f` (single multi-doc, reverse order: VM -> Network)
#
# The VM is intentionally NOT attached to the Network: that keeps the
# single-shot create -f / delete -f free of the VM<->Network reconcile/teardown
# dependency (a VM on a brand-new network races the network-aware scheduler on
# create, and the network delete is blocked by the VM's NIC on delete because VM
# delete is async). The attached-VM-on-a-manifest-network flow is covered by the
# networking smoke (which sequences network-reconcile before the VM); the
# single-apply dependency gap is a known limitation tracked separately.
#
# This validates the manifest feature end to end: the whole flow goes through
# the real `otherix` CLI, never curl.
#
# PREREQUISITES: a seeded dev environment, i.e. run AFTER
#   make local-dev-start
# so that: the CP is up on https://localhost:8080, the `dev` CLI cluster
# is configured, the node is `ready`, and a default pool exists. The CP
# and CLI binaries MUST be built from the current tree (the manifest
# create/delete/projection surface is this feature). This script creates
# its own network + VM and cleans them up; it does NOT tear down the dev
# env.
#
# Usage: make smoke-manifests   (or: bash dev/smoke/manifests/run.sh)

set -euo pipefail

# --- configuration -----------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"
OTX="${OTX:-./bin/otherix}"
NODE="${NODE:-node-1}"
IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
VM_NAME="smoke-mf"
SMOKE_NET="smoke-mf-net"
SMOKE_BRIDGE="otmf0"
VM_TIMEOUT="${VM_TIMEOUT:-300}"        # seconds to wait for vm create task

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

MANIFEST=""
cleanup() {
  echo "--- cleanup ---"
  otx vm delete "$VM_NAME" --wait --force >/dev/null 2>&1 || true
  otx network delete "$SMOKE_NET" --force >/dev/null 2>&1 || true
  [[ -n "$MANIFEST" ]] && rm -f "$MANIFEST"
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== manifests smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
node_status="$(otx node get "$NODE" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$node_status" == "ready" ]] || fail "$NODE not ready (got '${node_status:-none}'); run make seed-dev"
pass "CP up, $NODE ready"

# best-effort delete-first so a stale leftover from a prior failed run does not
# 409 the create below (the VM is unattached, so the network delete is not
# blocked).
otx vm delete "$VM_NAME" --wait --force >/dev/null 2>&1 || true
otx network delete "$SMOKE_NET" --force >/dev/null 2>&1 || true

# --- step 1: render the multi-doc manifest from the committed template --
# The manifest lives in stack.yaml.tmpl (a real file, not an inline heredoc);
# the script only substitutes its @@...@@ tokens.
echo "=== step 1: render manifest ==="
MANIFEST="$(mktemp -t otherix-manifest-smoke.XXXXXX.yaml)"
sed \
  -e "s|@@IMAGE_URL@@|${IMAGE_URL}|g" \
  -e "s|@@ARCH@@|${ARCH}|g" \
  "${SCRIPT_DIR}/stack.yaml.tmpl" >"$MANIFEST"
pass "rendered manifest to $MANIFEST"

# --- step 2: create -f (wait for VM ready) -----------------------------
echo "=== step 2: create -f ==="
info "create -f (wait for VM ready, <= ${VM_TIMEOUT}s; cold pool fetches the image on first use)"
otx create -f "$MANIFEST" --wait --wait-timeout "${VM_TIMEOUT}s" \
  || fail "create -f did not reach success"
pass "create -f applied Network + VM and reached ready"

# --- step 3: get -o yaml round-trip ------------------------------------
echo "=== step 3: get -o yaml round-trip ==="
otx vm get "$VM_NAME" -o yaml | grep -q "kind: VM" \
  || fail "vm get -o yaml did not emit 'kind: VM'"
pass "vm get -o yaml round-trips (kind: VM)"
otx network get "$SMOKE_NET" -o yaml | grep -q "bridgeName: ${SMOKE_BRIDGE}" \
  || fail "network get -o yaml did not emit 'bridgeName: ${SMOKE_BRIDGE}'"
pass "network get -o yaml round-trips (bridgeName: ${SMOKE_BRIDGE})"

# --- step 4: delete -f (reverse order) ---------------------------------
echo "=== step 4: delete -f ==="
info "delete -f (reverse order: VM -> Network)"
otx delete -f "$MANIFEST" --force || fail "delete -f failed"
pass "delete -f removed VM + Network"

trap - EXIT
rm -f "$MANIFEST"
echo
echo "${GREEN}=== manifests smoke OK ===${NC}"
