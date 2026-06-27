#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# User-CLI smoke - drives the whole `otherix user` operator surface against the
# real control plane as an admin operator, and proves the username-first identity
# model end to end: identity is a username (not an email), passwords are read
# from stdin, RBAC still gates admin-only operations, and a re-roled / re-
# passworded user behaves as expected on the next login.
#
# User management is CP-only - there is no agent interaction - so this smoke
# needs only a reachable control plane, not a booted VM. It runs entirely through
# the `otherix` CLI as a real operator would:
#
#   1. preconditions: CP up; `user whoami` shows the bootstrap admin (username
#      `admin`, role `admin`).
#   2. create a developer by username with a stdin password; `user get` shows
#      role developer and no email (username-first: email is optional/absent).
#   3. an invalid username (`Bad_Name` - uppercase + underscore) is rejected at
#      the validation edge (non-zero exit).
#   4. a duplicate username is rejected (conflict, non-zero exit).
#   5. RBAC gate: log in as the developer (a second, non-current cluster profile
#      pinned to the same dev CA), then an admin-only op (`user list`) is refused
#      with permission_denied, while a developer-allowed op (`vm list`) succeeds.
#   6. `user set-role smoke-dev operator` -> `user get` shows operator.
#   7. `user set-password smoke-dev` (stdin) -> a fresh login with the NEW
#      password succeeds and a login with the OLD password fails.
#   8. `user delete smoke-dev` -> `user get smoke-dev` is not found.
#
# The developer session is a SEPARATE named cluster profile (`smoke-dev-cluster`)
# added with --set-current=false, so the admin's current cluster is never
# disturbed; every developer-scoped call passes --cluster explicitly. Cleanup
# deletes the smoke user, removes the smoke profile, and re-asserts that the
# admin `dev` cluster is current, so the host CLI is left exactly as found for
# any later smoke.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# with the admin cluster profile (`dev`) configured and current.
#
# Usage: make smoke-user   (or: bash dev/smoke/user/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
ADMIN_CLUSTER="${ADMIN_CLUSTER:-dev}"            # the seeded admin profile (kept current)
DEV_USER="${DEV_USER:-smoke-dev}"                # the developer created/re-roled/deleted here
DEV_CLUSTER="${DEV_CLUSTER:-smoke-dev-cluster}"  # a non-current profile for the developer session
DEVPASS="${DEVPASS:-smoke-dev-pw-12345}"         # the developer's initial password (>= 12 chars)
NEWPASS="${NEWPASS:-smoke-dev-pw-rotated-67890}" # the rotated password (>= 12 chars)
BAD_USER="${BAD_USER:-Bad_Name}"                 # an invalid username (uppercase + underscore)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
# Progress lines go to stderr so they never leak into a $(...) capture.
pass() { echo "${GREEN}PASS${NC} $*" >&2; }
info() { echo "${YEL}..${NC} $*" >&2; }
fail() { echo "${RED}FAIL${NC} $*" >&2; echo "OTHERIX_SMOKE_FAIL"; exit 1; }

otx() { "$OTX" "$@"; }

# admin_field FIELD <username> -> a single JSON field of `user get` as admin
# ("" when the field is absent or the user is missing).
admin_field() {
  otx user get "$2" --output json 2>/dev/null | jq -r ".${1} // \"\"" 2>/dev/null || true
}

# current_cluster -> the name of the current cluster in the config ("" on error).
current_cluster() {
  otx config list 2>/dev/null | awk '$3=="*"{print $1}' || true
}

# add_dev_profile -> (re)create the developer cluster profile, pinned to the same
# dev cluster CA via --ca-fingerprint (mirrors seed-dev.sh), logging in as the
# developer. --set-current=false leaves the admin profile current; --force
# replaces a stale entry from a prior run. Reads the password from $1 on stdin.
add_dev_profile() {
  local pw="$1" fp
  fp="$(curl -fsSk "${CP_BASE}/v1/ca" | jq -r '.signer_fingerprint_sha256')"
  [[ -n "$fp" && "$fp" != "null" ]] || fail "could not read cluster CA fingerprint from ${CP_BASE}/v1/ca"
  OTHERIX_PASSWORD="$pw" otx config add cluster \
    --name "$DEV_CLUSTER" \
    --server "$CP_BASE" \
    --login "$DEV_USER" \
    --ca-fingerprint "$fp" \
    --set-current=false \
    --force
}

# --- cleanup -----------------------------------------------------------
# Best-effort: delete the smoke user, drop the developer profile, and ensure the
# admin cluster is current again so the host CLI is untouched for later smokes.
cleanup() {
  echo "--- cleanup ---"
  otx user delete "$DEV_USER" --force >/dev/null 2>&1 || true
  otx config remove "$DEV_CLUSTER" --force >/dev/null 2>&1 || true
  if [[ "$(current_cluster)" != "$ADMIN_CLUSTER" ]]; then
    otx config use "$ADMIN_CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== user smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v curl >/dev/null || fail "curl is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"

[[ "$(current_cluster)" == "$ADMIN_CLUSTER" ]] \
  || fail "admin cluster '${ADMIN_CLUSTER}' is not current (got '$(current_cluster)'); run make local-dev-start"

WHO_NAME="$(otx user whoami --output json 2>/dev/null | jq -r '.username // ""')"
WHO_ROLE="$(otx user whoami --output json 2>/dev/null | jq -r '.role // ""')"
[[ "$WHO_NAME" == "admin" && "$WHO_ROLE" == "admin" ]] \
  || fail "whoami is not the bootstrap admin (username '${WHO_NAME:-none}', role '${WHO_ROLE:-none}')"
pass "CP up (${CP_VERSION}); whoami = bootstrap admin (username=admin, role=admin)"

# best-effort delete-first of a stale smoke user / profile from a prior run.
cleanup >/dev/null 2>&1 || true

# =======================================================================
# scenario 1: create a developer by username (stdin password, no email)
# =======================================================================
echo
echo "=== scenario 1: create a developer by username with a stdin password ==="
otx user create "$DEV_USER" --role developer --password-stdin <<<"$DEVPASS" \
  || fail "user create ${DEV_USER} was rejected"
[[ "$(admin_field role "$DEV_USER")" == "developer" ]] \
  || fail "${DEV_USER} role is not developer (got '$(admin_field role "$DEV_USER")')"
[[ -z "$(admin_field email "$DEV_USER")" ]] \
  || fail "${DEV_USER} unexpectedly has an email ('$(admin_field email "$DEV_USER")') - username-first identity should leave it absent"
[[ "$(admin_field username "$DEV_USER")" == "$DEV_USER" ]] \
  || fail "${DEV_USER} username round-trip mismatch"
pass "${DEV_USER} created: username=${DEV_USER}, role=developer, no email"

# =======================================================================
# scenario 2: invalid username is rejected at the validation edge
# =======================================================================
echo
echo "=== scenario 2: an invalid username is rejected ==="
if otx user create "$BAD_USER" --role viewer --password-stdin <<<"x-invalid-but-long-enough" >/dev/null 2>&1; then
  otx user delete "$BAD_USER" --force >/dev/null 2>&1 || true
  fail "user create accepted the invalid username '${BAD_USER}' - validation did not reject it"
fi
[[ -z "$(admin_field username "$BAD_USER")" ]] \
  || fail "the invalid username '${BAD_USER}' was persisted despite a non-zero exit"
pass "invalid username '${BAD_USER}' rejected (non-zero exit, not persisted)"

# =======================================================================
# scenario 3: a duplicate username is rejected (conflict)
# =======================================================================
echo
echo "=== scenario 3: a duplicate username is rejected ==="
if otx user create "$DEV_USER" --role viewer --password-stdin <<<"another-password-here" >/dev/null 2>&1; then
  fail "user create accepted a duplicate username '${DEV_USER}' - the uniqueness guard did not fire"
fi
# the original row is untouched (still a developer).
[[ "$(admin_field role "$DEV_USER")" == "developer" ]] \
  || fail "duplicate-create mutated the existing ${DEV_USER} row (role now '$(admin_field role "$DEV_USER")')"
pass "duplicate username '${DEV_USER}' rejected (conflict); existing row untouched"

# =======================================================================
# scenario 4: RBAC still gates - developer is refused admin-only ops
# =======================================================================
echo
echo "=== scenario 4: RBAC gate - a developer session is refused admin ops, allowed VM ops ==="
add_dev_profile "$DEVPASS" >/dev/null 2>&1 \
  || fail "could not log in as ${DEV_USER} / create the ${DEV_CLUSTER} profile"
# the admin profile must remain current after adding the developer profile.
[[ "$(current_cluster)" == "$ADMIN_CLUSTER" ]] \
  || fail "adding the developer profile moved current-cluster off '${ADMIN_CLUSTER}' (now '$(current_cluster)')"
pass "logged in as ${DEV_USER}; admin profile '${ADMIN_CLUSTER}' still current"

# admin-only op via the developer profile MUST be refused (permission_denied/403).
if dev_list_out="$(otx --cluster "$DEV_CLUSTER" user list 2>&1)"; then
  fail "developer ${DEV_USER} was allowed 'user list' (admin-only) - RBAC did not gate it:\n${dev_list_out}"
fi
grep -Eqi 'permission_denied|permission denied|403|forbidden' <<<"$dev_list_out" \
  || fail "developer 'user list' failed but not with a permission error:\n${dev_list_out}"
pass "developer ${DEV_USER} refused 'user list' with a permission error"

# a developer-allowed op MUST succeed.
otx --cluster "$DEV_CLUSTER" vm list >/dev/null 2>&1 \
  || fail "developer ${DEV_USER} was refused 'vm list' (a developer-allowed op)"
pass "developer ${DEV_USER} allowed 'vm list'"

# =======================================================================
# scenario 5: set-role promotes the developer to operator
# =======================================================================
echo
echo "=== scenario 5: set-role smoke-dev operator ==="
otx user set-role "$DEV_USER" operator >/dev/null 2>&1 \
  || fail "user set-role ${DEV_USER} operator failed"
[[ "$(admin_field role "$DEV_USER")" == "operator" ]] \
  || fail "${DEV_USER} role did not become operator (got '$(admin_field role "$DEV_USER")')"
pass "${DEV_USER} re-roled to operator"

# =======================================================================
# scenario 6: set-password rotates the credential
# =======================================================================
echo
echo "=== scenario 6: set-password rotates the login credential ==="
otx user set-password "$DEV_USER" --password-stdin <<<"$NEWPASS" >/dev/null 2>&1 \
  || fail "user set-password ${DEV_USER} failed"

# a fresh login with the NEW password must succeed (re-add the profile).
add_dev_profile "$NEWPASS" >/dev/null 2>&1 \
  || fail "login as ${DEV_USER} with the rotated password failed - set-password did not take"
pass "login with the rotated password succeeds"

# a login with the OLD password must now fail.
if add_dev_profile "$DEVPASS" >/dev/null 2>&1; then
  fail "login as ${DEV_USER} with the OLD password still succeeds - the old credential was not invalidated"
fi
pass "login with the old password fails (credential rotated)"

# the admin profile is still current after all the re-logins.
[[ "$(current_cluster)" == "$ADMIN_CLUSTER" ]] \
  || fail "re-login churn moved current-cluster off '${ADMIN_CLUSTER}' (now '$(current_cluster)')"

# =======================================================================
# scenario 7: delete removes the user
# =======================================================================
echo
echo "=== scenario 7: delete removes the user ==="
otx user delete "$DEV_USER" --force >/dev/null 2>&1 \
  || fail "user delete ${DEV_USER} failed"
if otx user get "$DEV_USER" >/dev/null 2>&1; then
  fail "user get ${DEV_USER} still resolves after delete - the user was not removed"
fi
pass "${DEV_USER} deleted; user get is not found"

# =======================================================================
# done
# =======================================================================
echo "=== final cleanup ==="
cleanup
[[ "$(current_cluster)" == "$ADMIN_CLUSTER" ]] \
  || fail "after cleanup the admin cluster '${ADMIN_CLUSTER}' is not current (got '$(current_cluster)')"
pass "cleanup done; admin cluster '${ADMIN_CLUSTER}' left current for later smokes"

trap - EXIT
echo
echo "${GREEN}=== user smoke PASSED ===${NC}"
echo "  create by username (no email) + stdin password; invalid + duplicate rejected"
echo "  RBAC gate: developer refused 'user list', allowed 'vm list'"
echo "  set-role -> operator; set-password rotates login; delete -> not found"
echo "OTHERIX_SMOKE_PASS"
