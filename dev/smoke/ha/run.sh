#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Slice-9 HA multi-process smoke. Runs THREE real otherix-api processes on
# loopback, grows a single node to a 3-voter etcd cluster using the real
# slice-9 mechanics (on-disk cluster CA, /v1/cluster/join CA fetch over TLS,
# always-on peer mTLS), then checks replication and a 1-of-3 partition.
#
# Membership steps (AddLearner / PromoteMember) are driven by dev/smoke/ha/driver
# because the product does not yet wire that orchestration (see ROADMAP). The CP
# is a native binary, so no Lima is involved (Lima is only for the Linux agent).
#
# Usage: bash dev/smoke/ha/run.sh   (run from repo root)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$ROOT"
WORK="$ROOT/.local/smoke/run"
API="$ROOT/.local/smoke/otherix-api"
DRIVER="$ROOT/.local/smoke/driver"

ADMIN_EMAIL="smoke-admin@otherix.test"
ADMIN_PW="smoke-admin-password-123"
JWT_SECRET="smoke-only-jwt-secret-32-bytes!!!"

# node index 0/1/2 -> ports: api(http) agentTLS peer(https) client(http).
# Indexed arrays (not associative) to stay bash-3.2 compatible (macOS default).
API_PORT=( 18080 18081 18082 )
AGENT_PORT=( 18443 18444 18445 )
PEER_PORT=( 12380 12382 12384 )
CLIENT_PORT=( 12379 12381 12383 )
PID=()

peer_url()   { echo "https://127.0.0.1:${PEER_PORT[$1]}"; }
client_url() { echo "http://127.0.0.1:${CLIENT_PORT[$1]}"; }

log()  { echo -e "\033[1;36m[smoke]\033[0m $*"; }
ok()   { echo -e "\033[1;32m[ ok ]\033[0m $*"; }
fail() { echo -e "\033[1;31m[fail]\033[0m $*"; cleanup; exit 1; }

cleanup() {
  log "cleanup"
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  sleep 1
  for n in "${!PID[@]}"; do kill -9 "${PID[$n]}" 2>/dev/null || true; done
}
trap cleanup EXIT

initial_cluster() { # args: node indices to include
  local parts=() n
  for n in "$@"; do parts+=("otherix-$n=$(peer_url "$n")"); done
  local IFS=,; echo "${parts[*]}"
}

gen_config() { # node, mode, initial_cluster, [cluster_join_block]
  local n="$1" mode="$2" ic="$3" cjoin="${4:-}"
  local dir="$WORK/n$n"
  mkdir -p "$dir/pki"
  cat > "$dir/api.yaml" <<YAML
server:        { listen: "127.0.0.1:${API_PORT[$n]}", read_timeout: 30s, write_timeout: 30s, shutdown_grace: 10s }
agent_server:  { enabled: true, listen: "127.0.0.1:${AGENT_PORT[$n]}" }
agent_client:  { enabled: false }
workers:       { enabled: false }
logger:        { level: "info", format: "json" }
auth:          { jwt_secret: "$JWT_SECRET", jwt_access_ttl: 15m, jwt_refresh_ttl: 720h }
console:       { access_mode: "proxy" }
cluster_ca:    { cert_file: "$dir/pki/cluster-ca.crt", key_file: "$dir/pki/cluster-ca.key" }
etcd:
  mode:          "$mode"
  name:          "otherix-$n"
  data_dir:      "$dir/etcd"
  peer_url:      "$(peer_url "$n")"
  peer_auto_dir: "$dir/pki/peer"
  client_url:    "$(client_url "$n")"
  cluster_token: "otherix-smoke"
  initial_cluster: "$ic"
cp_cert:       { local_cache: { enabled: false } }
$cjoin
YAML
}

start_node() { # node
  local n="$1"
  local dir="$WORK/n$n"
  if [ "$n" = "0" ]; then
    OTHERIX_BOOTSTRAP_ADMIN_EMAIL="$ADMIN_EMAIL" OTHERIX_BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PW" \
      "$API" --config "$dir/api.yaml" >"$dir/log" 2>&1 &
  else
    "$API" --config "$dir/api.yaml" >"$dir/log" 2>&1 &
  fi
  PID[$n]=$!
  log "started node$n pid=${PID[$n]}"
}

wait_ready() { # node, timeout-secs
  local n="$1" t="${2:-30}" i=0
  while (( i < t*2 )); do
    if curl -fsS "http://127.0.0.1:${API_PORT[$n]}/readyz" >/dev/null 2>&1; then return 0; fi
    if ! kill -0 "${PID[$n]:-0}" 2>/dev/null; then fail "node$n exited early; last log:\n$(tail -8 "$WORK/n$n/log")"; fi
    sleep 0.5; ((i++))
  done
  fail "node$n not ready in ${t}s; last log:\n$(tail -12 "$WORK/n$n/log")"
}

grow() { # node, max_uses-token-info via globals TOKEN; adds+promotes node n
  local n="$1"
  log "add-learner for node$n ($(peer_url "$n"))"
  local id; id="$("$DRIVER" add-learner "$(client_url 0)" "$(peer_url "$n")")" || fail "add-learner node$n"
  echo "otherix-$n token: using cluster join token (fetches CA via /v1/cluster/join)"
  printf '%s' "$TOKEN" > "$WORK/n$n/token"
  start_node "$n"
  wait_ready "$n" 40
  log "promote node$n (member id $id)"
  "$DRIVER" promote "$(client_url 0)" "$id" || fail "promote node$n"
  "$DRIVER" wait-serving "$(client_url "$n")" || fail "wait-serving node$n"
  ok "node$n joined and promoted to voter"
}

# ---------------------------------------------------------------------------

log "build api + driver"
go build -o "$API" ./cmd/api
go build -o "$DRIVER" ./dev/smoke/ha/driver
rm -rf "$WORK"; mkdir -p "$WORK"

# node0: single-node bootstrap.
gen_config 0 single "$(initial_cluster 0)"
start_node 0
wait_ready 0 30
ok "node0 up (single)"
[ "$("$DRIVER" voters "$(client_url 0)")" = "1" ] || fail "node0 voters != 1"

# Mint a cluster join token (max_uses=2: node1 + node2 each fetch the CA once).
log "login admin + mint cluster join token"
JWT="$(curl -fsS -X POST "http://127.0.0.1:${API_PORT[0]}/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PW\"}" | jq -r .access_token)"
[ -n "$JWT" ] && [ "$JWT" != "null" ] || fail "admin login failed"
MINT="$(curl -fsS -X POST "http://127.0.0.1:${API_PORT[0]}/v1/nodes/join-tokens" \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"kind":"cluster","max_uses":2}')"
TOKEN="$(echo "$MINT" | jq -r .token)"
FP="$(echo "$MINT" | jq -r .ca_fingerprint_sha256)"
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || fail "mint token failed: $MINT"
ok "minted cluster token, CA fingerprint ${FP:0:16}..."

cjoin_block() { # node
  cat <<YAML
cluster_join:
  cp_url:         "https://127.0.0.1:${AGENT_PORT[0]}"
  token_path:     "$WORK/n$1/token"
  ca_fingerprint: "$FP"
  timeout:        "20s"
YAML
}

# Grow to 3 voters, one at a time (settle-gated).
gen_config 1 join "$(initial_cluster 0 1)" "$(cjoin_block 1)"
grow 1
gen_config 2 join "$(initial_cluster 0 1 2)" "$(cjoin_block 2)"
grow 2

V="$("$DRIVER" voters "$(client_url 0)")"
[ "$V" = "3" ] || fail "voters = $V, want 3"
ok "3-voter cluster formed over peer mTLS"

# Replication: write on node0, read on node2.
"$DRIVER" put "$(client_url 0)" /otherix/smoke/k1 hello || fail "put k1"
[ "$("$DRIVER" get "$(client_url 2)" /otherix/smoke/k1)" = "hello" ] || fail "k1 not replicated to node2"
ok "write on node0 replicated to node2"

# Partition: kill node2; 2 of 3 remain -> quorum holds, cluster stays writable.
log "partition: killing node2"
kill "${PID[2]}"; wait "${PID[2]}" 2>/dev/null || true; unset 'PID[2]'
sleep 2
"$DRIVER" put "$(client_url 0)" /otherix/smoke/k2 world || fail "cluster not writable with 2/3 (quorum lost?)"
[ "$("$DRIVER" get "$(client_url 1)" /otherix/smoke/k2)" = "world" ] || fail "k2 not on node1"
ok "quorum survived 1-of-3 partition; cluster still writable"

# Heal: restart node2 (data dir intact), it rejoins and catches up.
log "heal: restarting node2"
start_node 2
wait_ready 2 40
[ "$("$DRIVER" get "$(client_url 2)" /otherix/smoke/k2)" = "world" ] || fail "node2 did not catch up after rejoin"
[ "$("$DRIVER" voters "$(client_url 0)")" = "3" ] || fail "voters != 3 after heal"
ok "node2 rejoined and caught up; 3 voters"

echo
ok "HA multi-process smoke PASSED"
