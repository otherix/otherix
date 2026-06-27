#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# api-token CLI smoke - drives `otherix api-token create | list | revoke`
# against the real control plane as the admin operator and proves a minted
# token works end to end: it authenticates as the same identity, appears in
# the list, and is rejected after revoke.
#
# API-token management is CP-only - there is no agent interaction - so this
# smoke needs only a reachable control plane, not a booted VM.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# with the admin cluster profile (`dev`) configured and current.
#
# Usage: make smoke-api-token   (or: bash dev/smoke/api-token/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

OTX="${OTX:-./bin/otherix}"
ADMIN_CLUSTER="${ADMIN_CLUSTER:-dev}"
TOKEN_NAME="${TOKEN_NAME:-smoke-api-token}"

RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*" >&2; }
info() { echo "${YEL}..${NC} $*" >&2; }
fail() { echo "${RED}FAIL${NC} $*" >&2; echo "OTHERIX_SMOKE_FAIL"; exit 1; }

otx() { "$OTX" "$@"; }

PREFIX=""
# Best-effort: revoke the smoke token if the run left one behind.
cleanup() {
  echo "--- cleanup ---"
  if [[ -n "$PREFIX" ]]; then
    otx api-token revoke "$PREFIX" --force >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "=== api-token smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
info "CP version: $(cp_version)"
WHO="$(otx user whoami --output json 2>/dev/null | jq -r '.username // ""')"
[[ "$WHO" == "admin" ]] || fail "whoami is not the bootstrap admin (got '${WHO:-none}')"
pass "CP up; whoami = admin"

# =====================================================================
# scenario 1: create a token and capture the one-time plaintext
# =====================================================================
echo
echo "=== scenario 1: create a token (capture the one-time plaintext) ==="
CREATE_JSON="$(otx api-token create "$TOKEN_NAME" --ttl 1h --output json)" \
  || fail "api-token create failed"
TOKEN="$(jq -r '.token // ""' <<<"$CREATE_JSON")"
PREFIX="$(jq -r '.prefix // ""' <<<"$CREATE_JSON")"
[[ "$TOKEN" == otx_* ]] || fail "create did not return a plaintext otx_* token"
[[ -n "$PREFIX" ]] || fail "create did not return a prefix"
pass "created token ${PREFIX} (name=${TOKEN_NAME}, ttl 1h)"

# =====================================================================
# scenario 2: the minted token authenticates as the same identity
# =====================================================================
echo
echo "=== scenario 2: the minted token authenticates ==="
# Pass the token via the env var (not --token in argv) so the secret never
# lands in the process table - the recommended non-argv path. No --endpoint:
# the current `dev` cluster profile supplies the endpoint and CA trust, and
# the env token overrides that profile's stored token.
WHO_VIA_TOKEN="$(OTHERIX_API_TOKEN="$TOKEN" otx user whoami --output json 2>/dev/null | jq -r '.username // ""')" \
  || true
[[ "$WHO_VIA_TOKEN" == "admin" ]] \
  || fail "the minted token did not authenticate as admin (got '${WHO_VIA_TOKEN:-none}')"
pass "minted token authenticates as admin"

# =====================================================================
# scenario 3: the token appears in the list (status active)
# =====================================================================
echo
echo "=== scenario 3: the token is listed (active) ==="
LIST_JSON="$(otx api-token list --output json)" || fail "api-token list failed"
ROW="$(jq -r --arg p "$PREFIX" '.data[] | select(.prefix==$p)' <<<"$LIST_JSON")"
[[ -n "$ROW" ]] || fail "the created token ${PREFIX} is not in the list"
[[ -z "$(jq -r '.revoked_at // ""' <<<"$ROW")" ]] || fail "the fresh token is already revoked in the list"
pass "token ${PREFIX} is listed and active"

# =====================================================================
# scenario 4: revoke the token
# =====================================================================
echo
echo "=== scenario 4: revoke the token ==="
otx api-token revoke "$PREFIX" --force >/dev/null 2>&1 || fail "api-token revoke failed"
pass "revoked token ${PREFIX}"

# =====================================================================
# scenario 5: the revoked token no longer authenticates
# =====================================================================
echo
echo "=== scenario 5: the revoked token is rejected ==="
if OTHERIX_API_TOKEN="$TOKEN" otx user whoami >/dev/null 2>&1; then
  fail "the revoked token still authenticates - revoke did not take effect"
fi
pass "revoked token is rejected"

# =====================================================================
# scenario 6: the revoked token shows up only with --include-revoked
# =====================================================================
echo
echo "=== scenario 6: revoked token is hidden by default, shown with --include-revoked ==="
DEFAULT_HAS="$(otx api-token list --output json | jq -r --arg p "$PREFIX" '[.data[] | select(.prefix==$p)] | length')"
[[ "$DEFAULT_HAS" == "0" ]] || fail "revoked token still shows in the default list"
REVOKED_ROW="$(otx api-token list --include-revoked --output json | jq -r --arg p "$PREFIX" '.data[] | select(.prefix==$p) | .revoked_at // ""')"
[[ -n "$REVOKED_ROW" ]] || fail "revoked token not shown even with --include-revoked"
pass "revoked token hidden by default; visible (revoked) with --include-revoked"

PREFIX=""  # nothing left to clean up
trap - EXIT
echo
echo "${GREEN}=== api-token smoke PASSED ===${NC}"
echo "  create (plaintext once) -> authenticates -> listed active -> revoke -> rejected -> hidden/included"
echo "OTHERIX_SMOKE_PASS"
