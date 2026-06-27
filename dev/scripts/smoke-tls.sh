#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# CLI-over-HTTPS smoke (operator path). A single native otherix-api process
# serves the user-facing /v1 API over TLS on loopback with an auto-generated
# cluster-CA leaf cert. The smoke then runs `otherix config add cluster` against
# the https:// endpoint, which performs the trust-on-first-use cluster-CA fetch
# plus fingerprint pin, stores the CA inline in the CLI config, logs in, and
# issues an API token - then drives a normal `otherix vm list` over HTTPS using
# only the stored inline CA. This is the CP<->CLI boundary; no agents, no Lima.
#
# Usage: bash dev/scripts/smoke-tls.sh   (run from repo root)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
WORK="$ROOT/.local/smoke/run-tls"
API="$ROOT/.local/smoke/otherix-api"
CLI="$ROOT/.local/smoke/otherix"
CFG="$WORK/cli-config.yaml"

ADMIN_USER="smoke-admin"
ADMIN_PW="smoke-admin-password-123"
JWT_SECRET="smoke-only-jwt-secret-32-bytes!!!"

API_PORT=18080
PEER_PORT=12380
CLIENT_PORT=12379
BASE="https://127.0.0.1:${API_PORT}"

PID=""

log()  { echo -e "\033[1;36m[smoke]\033[0m $*"; }
ok()   { echo -e "\033[1;32m[ ok ]\033[0m $*"; }
fail() { echo -e "\033[1;31m[fail]\033[0m $*"; [ -n "$PID" ] && tail -15 "$WORK/log" 2>/dev/null || true; cleanup; exit 1; }

cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
  sleep 1
  [ -n "$PID" ] && kill -9 "$PID" 2>/dev/null || true
}
trap cleanup EXIT

log "build api + cli"
go build -o "$API" ./cmd/api
go build -o "$CLI" ./cmd/cli
rm -rf "$WORK"; mkdir -p "$WORK/pki"

# Single-node api with the USER listener on TLS. agent_server is off (no agents
# in this smoke); server.tls.enabled is the only TLS knob exercised here. With
# no operator cert configured, LoadOrGenerateCPCert auto-generates a leaf signed
# by the cluster CA whose SAN covers localhost + 127.0.0.1, so the CLI can reach
# the listener at https://127.0.0.1.
cat > "$WORK/api.yaml" <<YAML
server:        { listen: "127.0.0.1:${API_PORT}", read_timeout: 30s, write_timeout: 30s, shutdown_grace: 10s, tls: { enabled: true } }
agent_server:  { enabled: false }
agent_client:  { enabled: false }
workers:       { enabled: false }
logger:        { level: "info", format: "json" }
auth:          { jwt_secret: "$JWT_SECRET", jwt_access_ttl: 15m, jwt_refresh_ttl: 720h }
console:       { access_mode: "proxy" }
cluster_ca:    { cert_file: "$WORK/pki/cluster-ca.crt", key_file: "$WORK/pki/cluster-ca.key" }
etcd:
  mode:          "single"
  name:          "otherix-0"
  data_dir:      "$WORK/etcd"
  peer_url:      "https://127.0.0.1:${PEER_PORT}"
  peer_auto_dir: "$WORK/pki/peer"
  client_url:    "http://127.0.0.1:${CLIENT_PORT}"
  cluster_token: "otherix-smoke-tls"
  initial_cluster: "otherix-0=https://127.0.0.1:${PEER_PORT}"
cp_cert:       { local_cache: { enabled: false } }
YAML

log "start api (user listener over TLS)"
OTHERIX_BOOTSTRAP_ADMIN_USERNAME="$ADMIN_USER" OTHERIX_BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PW" \
  "$API" --config "$WORK/api.yaml" >"$WORK/log" 2>&1 &
PID=$!
log "api pid=$PID"

# Readiness over HTTPS. We have not pinned the CA yet, so -k here.
i=0
while (( i < 60 )); do
  if curl -fsSk "$BASE/readyz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$PID" 2>/dev/null; then fail "api exited early"; fi
  sleep 0.5; ((i++))
done
(( i < 60 )) || fail "api not ready over HTTPS in 30s"
ok "api ready over HTTPS at $BASE"

# Sanity: plaintext HTTP must be refused on the TLS listener (proves it is
# really serving TLS, not silently plaintext).
if curl -fsS "http://127.0.0.1:${API_PORT}/readyz" >/dev/null 2>&1; then
  fail "plaintext HTTP succeeded on a TLS listener - TLS not enforced"
fi
ok "plaintext HTTP rejected on the TLS listener"

# Read the cluster CA fingerprint out of band (curl -k) to pin non-interactively.
FP="$(curl -fsSk "$BASE/v1/ca" | jq -r '.signer_fingerprint_sha256')"
[ -n "$FP" ] && [ "$FP" != "null" ] || fail "could not read CA fingerprint from /v1/ca"
ok "cluster CA fingerprint ${FP:0:16}..."

# Operator path: config add cluster over HTTPS. The system-trust probe fails
# (self-signed cluster CA), so the CLI falls back to the TOFU fetch + pin against
# the supplied fingerprint, stores the CA inline, logs in, and issues a token.
log "otherix config add cluster over HTTPS (TOFU + --ca-fingerprint)"
"$CLI" config add cluster \
  --config "$CFG" \
  --name tls-smoke \
  --server "$BASE" \
  --login "$ADMIN_USER" \
  --password "$ADMIN_PW" \
  --ca-fingerprint "$FP" \
  >"$WORK/add.log" 2>&1 || fail "config add cluster failed:\n$(cat "$WORK/add.log")"
ok "cluster added over HTTPS"

# The stored config must carry the inline CA so it is self-contained.
grep -q "certificate-authority-data:" "$CFG" || fail "stored config has no inline CA"
ok "config is self-contained (certificate-authority-data present)"

# A normal command over HTTPS using ONLY the stored inline CA (no -k anywhere).
log "otherix vm list over HTTPS using the stored inline CA"
"$CLI" --config "$CFG" vm list >"$WORK/list.log" 2>&1 || fail "vm list over HTTPS failed:\n$(cat "$WORK/list.log")"
ok "vm list succeeded over HTTPS with the inline CA"

echo
ok "CLI-over-HTTPS (user API TLS) smoke PASSED"
