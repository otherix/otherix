#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# CLI-driven HA grow smoke (operator path). Same local-process harness as
# dev/scripts/smoke-ha.sh - three real otherix-api processes on loopback grown
# from one node to a 3-voter etcd cluster - but the two manual membership steps
# are replaced with the Phase 2 operator wrappers, so the smoke exercises the
# exact commands a human runs:
#
#   1. Mint the cluster join token with `otherix cluster join-token create`
#      (the otherix CLI, a remote API client) instead of a raw curl POST to
#      /v1/nodes/join-tokens. Parsed from --output json (.token,
#      .ca_fingerprint_sha256).
#   2. Write each joiner's cluster_join config with `otherix-api join
#      --no-restart` (the daemon subcommand that rewrites api.yaml in place)
#      instead of hand-generating the cluster_join YAML block. --no-restart
#      writes config only; the smoke then starts the process itself, mirroring
#      what `systemctl restart` would do under systemd.
#
# Everything else (self-driving join boot path, always-on promote loop,
# assertions) is identical to smoke-ha.sh. The CP is a native binary so no Lima
# is involved.
#
# Usage: bash dev/scripts/smoke-ha-cli.sh   (run from repo root)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
WORK="$ROOT/.local/smoke/run-cli"
API="$ROOT/.local/smoke/otherix-api"
CLI="$ROOT/.local/smoke/otherix"

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

gen_config() { # node, mode, initial_cluster
  # A joiner is seeded here in single mode (a self-contained, valid api.yaml);
  # `otherix-api join --no-restart` then rewrites etcd.mode=join, etcd.name, and
  # the cluster_join block in place, leaving every other key intact. A join node
  # gets an EMPTY initial_cluster: it computes its real initial-cluster from the
  # membership in the /v1/cluster/join response, never from config. node0
  # (single/bootstrap) keeps its self-only initial_cluster. agent_server is on
  # for everyone because /v1/cluster/join is served on the agent TLS listener.
  # workers is off (the promote loop is always-on and independent of it). All
  # nodes share jwt_secret so a node0-issued JWT is accepted everywhere.
  local n="$1" mode="$2" ic="$3"
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

# voter_count returns the number of voting members (is_learner==false) as seen
# by GET /v1/cluster/members on the given node, using the admin JWT.
voter_count() { # node_index, jwt
  curl -fsS "http://127.0.0.1:${API_PORT[$1]}/v1/cluster/members" -H "Authorization: Bearer $2" \
    | jq '[.data[] | select(.is_learner==false)] | length'
}

# wait_voters polls GET /v1/cluster/members on node0 until the voter count
# reaches $want or the timeout elapses. The self-driving join registers the new
# learner and the always-on promote loop (15s cadence) converts it to a voter
# once it has caught up, so this can take a couple of promote ticks.
wait_voters() { # want, timeout-secs, jwt
  local want="$1" t="${2:-90}" jwt="$3" i=0 v=""
  while (( i < t )); do
    v="$(voter_count 0 "$jwt" 2>/dev/null || echo "?")"
    if [ "$v" = "$want" ]; then return 0; fi
    if (( i % 5 == 0 )); then log "waiting for $want voters (have ${v}); ${i}s/${t}s"; fi
    sleep 1; ((i++))
  done
  fail "voters reached ${v}, want ${want} within ${t}s; node2 log tail:\n$(tail -15 "$WORK/n2/log" 2>/dev/null)"
}

# net_create POSTs a network on the given node and prints the created id.
net_create() { # node_index, jwt, name, bridge
  curl -fsS -X POST "http://127.0.0.1:${API_PORT[$1]}/v1/networks" \
    -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$3\",\"type\":\"bridge\",\"bridge_name\":\"$4\"}" | jq -r .id
}

# net_name reads a network by id on the given node and prints its name.
net_name() { # node_index, jwt, id
  curl -fsS "http://127.0.0.1:${API_PORT[$1]}/v1/networks/$3" -H "Authorization: Bearer $2" \
    | jq -r '.name // empty'
}

# grow drives a joiner entirely through the operator wrappers: seed a single-mode
# api.yaml, run `otherix-api join --no-restart` to rewrite it into a join config
# (cluster_join block, NO initial_cluster) and drop the token file, start the
# process, wait ready, then wait for the auto-promote loop to make it a voter.
grow() { # node, want_voters
  local n="$1" want="$2"
  local dir="$WORK/n$n"
  gen_config "$n" single ""
  log "node$n: writing join config via 'otherix-api join --no-restart'"
  "$API" join \
    --cp-url "https://127.0.0.1:${AGENT_PORT[0]}" \
    --token "$TOKEN" \
    --ca-fingerprint "sha256:$FP" \
    --name "otherix-$n" \
    --config "$dir/api.yaml" \
    --token-dest "$dir/token" \
    --no-restart \
    >"$dir/join.log" 2>&1 \
    || fail "otherix-api join failed for node$n; log:\n$(cat "$dir/join.log")"
  start_node "$n"
  wait_ready "$n" 60
  log "node$n booted via self-driving join; awaiting promotion to voter"
  wait_voters "$want" 90 "$JWT"
  ok "node$n joined (self-driving) and auto-promoted to voter ($want voters)"
}

# ---------------------------------------------------------------------------

log "build api + cli"
go build -o "$API" ./cmd/api
go build -o "$CLI" ./cmd/cli
rm -rf "$WORK"; mkdir -p "$WORK"

# node0: single-node bootstrap.
gen_config 0 single "$(initial_cluster 0)"
start_node 0
wait_ready 0 30
ok "node0 up (single)"

# Admin login (raw curl - the CLI has no `login` verb; it authenticates with a
# bearer token, and a JWT is a valid bearer per cpclient.Options.Token).
log "login admin"
JWT="$(curl -fsS -X POST "http://127.0.0.1:${API_PORT[0]}/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PW\"}" | jq -r .access_token)"
[ -n "$JWT" ] && [ "$JWT" != "null" ] || fail "admin login failed"
[ "$(voter_count 0 "$JWT")" = "1" ] || fail "node0 voters != 1"
ok "node0 reports 1 voter via GET /v1/cluster/members"

# Mint the cluster join token via the OPERATOR CLI (the otherix binary, a remote
# API client). --endpoint points it at node0 over plain HTTP; --token passes the
# admin JWT as the bearer credential (cliauth.FlagEndpoint / cliauth.FlagToken).
# JSON output is trivially parseable: .token and .ca_fingerprint_sha256.
# max-uses=2 so node1 + node2 each redeem the one token once.
log "mint cluster join token via 'otherix cluster join-token create'"
MINT="$("$CLI" cluster join-token create \
  --endpoint "http://127.0.0.1:${API_PORT[0]}" \
  --token "$JWT" \
  --max-uses 2 \
  --output json)" || fail "CLI mint failed:\n$MINT"
TOKEN="$(echo "$MINT" | jq -r .token)"
FP="$(echo "$MINT" | jq -r .ca_fingerprint_sha256)"
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || fail "mint token failed: $MINT"
[ -n "$FP" ] && [ "$FP" != "null" ] || fail "mint fingerprint missing: $MINT"
ok "minted cluster token via CLI, CA fingerprint ${FP:0:16}..."

# Grow to 3 voters, one at a time, entirely through the operator wrappers +
# the product's self-driving join + always-on auto-promote.
grow 1 2
grow 2 3

[ "$(voter_count 0 "$JWT")" = "3" ] || fail "voters != 3 after growth"
ok "3-voter cluster formed over peer mTLS via CLI-driven operator join path"

# Replication: create a network on node0, read it back on node2 with the shared
# admin JWT. The row replicates through etcd.
log "replication: create network on node0, read on node2"
NET1="$(net_create 0 "$JWT" smoke-net br-smoke)"
[ -n "$NET1" ] && [ "$NET1" != "null" ] || fail "create network on node0 failed"
[ "$(net_name 2 "$JWT" "$NET1")" = "smoke-net" ] || fail "network $NET1 not replicated to node2"
ok "network created on node0 replicated to node2"

# Partition: kill node2; 2 of 3 remain -> quorum holds, cluster stays writable.
log "partition: killing node2"
kill "${PID[2]}"; wait "${PID[2]}" 2>/dev/null || true; unset 'PID[2]'
sleep 2
NET2="$(net_create 0 "$JWT" smoke-net2 br-smoke2)"
[ -n "$NET2" ] && [ "$NET2" != "null" ] || fail "cluster not writable with 2/3 (quorum lost?)"
[ "$(net_name 1 "$JWT" "$NET2")" = "smoke-net2" ] || fail "network $NET2 not on node1"
ok "quorum survived 1-of-3 partition; cluster still writable"

echo
ok "HA CLI-driven multi-process smoke PASSED"
