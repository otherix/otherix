#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Load-balancer smoke - proves `otherix lb` fronts a label-selected pool of VMs
# end to end: it balances new connections across the eligible backends, it stops
# sending traffic to a backend the moment that backend leaves the running phase,
# and it refuses a connection outright when no backend is eligible.
#
# What it proves, end to end (operator CLI only):
#   - an operator registers a load balancer over a label selector
#     (`otherix lb create lb --port <p> --selector app=<v>`) and forwards a local
#     port to it (`otherix lb connect lb --listen 127.0.0.1:<lp>`); each new
#     connection is brokered to one of the eligible backend VMs through the
#     converged gateway (the control plane stays out of the data path);
#   - BALANCING: over many short connections BOTH backends answer, so selection
#     is per connection across the eligible pool (not pinned to one backend);
#   - ELIGIBILITY: after one backend is powered off (its observed phase leaves
#     running), every subsequent connection is served by the surviving backend
#     ONLY - the control plane excludes the stopped backend from the pool;
#   - NO ELIGIBLE BACKEND: once both backends are powered off, a connection is
#     refused (the broker returns no eligible backend, so the forwarded local
#     connection carries no bytes) - the load balancer fails closed.
#
# HOW BALANCING IS MEASURED:
#   Each backend guest runs a tiny TCP server on $PORT that answers every new
#   connection with its OWN identity string and closes. A client makes many
#   short, sequential connections to the local `otherix lb connect` listener and
#   records the identity returned by each; the smoke asserts BOTH identities
#   appear (balancing), then only the survivor's after a poweroff (eligibility),
#   then none at all after both are off (no-eligible fails closed).
#
# WHERE THE OPERATOR CLI RUNS:
#   The brokered DATA-PLANE connect runs INSIDE the dedicated gateway node, not
#   on the host - in this dev stack the host reaches the control plane but NOT the
#   gateway's data-plane endpoint (no route to the inter-node subnet), so a
#   host-run connect would broker fine and then fail to dial the gateway. The node
#   reaches both. The node CLI is staged with a config that carries the public API
#   URL, the operator token, and the cluster CA (reachability, credential, and TLS
#   trust), and the connect + the client both run there.
#
# GATEWAY PLACEMENT (dev stack): identical to the ingress smokes - the third dev
#   node is repurposed as the gateway (its agent is stopped), leaving the first
#   two nodes as the VM hosts for the two backends.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# plus jq, python3, and go on the host.
#
# Usage: make smoke-lb   (or: bash dev/smoke/lb/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
# Resolved in preconditions to the linux cross-builds matching the gateway node's
# arch (the node runs linux, the host may be macOS), overridable.
GW_BIN="${GW_BIN:-}"
CLI_BIN="${CLI_BIN:-}"

# A unique per-run suffix keeps the LB/VMs/label re-runnable without a clean
# restart: the cluster has no node-delete for the gateway, and a fixed name/label
# would collide with a half-torn prior run.
RUN="${RUN:-$(date +%H%M%S)-${RANDOM}}"

NODE1="node-1"                              # first VM host (backend lbvm-a)
NODE2="node-2"                              # second VM host (backend lbvm-b)
NET="lb-ovl-${RUN}"                         # dhcp-enabled overlay (unique per run)
SUBNET="${SUBNET:-10.97.0.0/24}"            # unlikely to clash with the dev stack

VM_A="lbvm-a-${RUN}"                        # backend on NODE1
VM_B="lbvm-b-${RUN}"                        # backend on NODE2
LABEL_KEY="app"                             # selector key
LABEL_VAL="lbsmoke-${RUN}"                  # selector value (unique per run)
LB_NAME="lbsmoke-${RUN}"                    # the load balancer

PORT="${PORT:-9000}"                        # guest TCP port the backends serve on
LP="${LP:-19020}"                           # local `otherix lb connect` listener port

GW_NAME="${GW_NAME:-lb-gw-${RUN}}"          # the gateway node name (fresh join)
GW_LISTEN_PORT="${GW_LISTEN_PORT:-9443}"    # the gateway control listener (mTLS)
GW_INGRESS_PORT="${GW_INGRESS_PORT:-9444}"  # the gateway ingress listener clients dial for /v1/connect
GW_INDEX="${GW_INDEX:-3}"                    # the third dev node becomes the gateway

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"           # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"                 # seconds for a network to reconcile ready
GW_READY_WAIT="${GW_READY_WAIT:-180}"       # seconds for the gateway to report overlay-ready
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"       # seconds for a guest to lease an overlay IP
OP_WAIT="${OP_WAIT:-180}"                    # seconds for an async poweroff task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-120}"             # seconds for the CP-projected status.phase to converge
BALANCE_HITS="${BALANCE_HITS:-20}"          # short connections for the balancing check
EXCLUDE_HITS="${EXCLUDE_HITS:-10}"          # short connections for the eligibility check
NOELIG_HITS="${NOELIG_HITS:-6}"             # short connections for the no-eligible check

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

GW_HANDLE="$(smoke_handle "$GW_INDEX")"
# The operator data-plane CLI runs on the gateway node (see "WHERE THE OPERATOR
# CLI RUNS"); it is the same node that hosts the gateway daemon.
NODE_CLI_HANDLE="$GW_HANDLE"

# net_ready NODE NET -> 0 when the network reconciled "ready" on NODE.
net_ready() {
  [[ "$(otx network get "$2" --output json 2>/dev/null \
      | jq -r --arg n "$1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
      == "ready" ]]
}

# gw_overlay_ready -> 0 when the gateway reports the overlay reconciled ready.
gw_overlay_ready() { net_ready "$GW_NAME" "$NET"; }

# vm_phase NAME -> the CP-observed status.phase ("" if the VM is gone).
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# wait_net_ready NODE -> block (up to NET_WAIT) until NET reconciled ready on NODE.
wait_net_ready() {
  local node="$1" deadline; deadline=$(( SECONDS + NET_WAIT ))
  info "waiting for $NET to reconcile ready on $node (<= ${NET_WAIT}s)"
  while (( SECONDS < deadline )); do net_ready "$node" "$NET" && return 0; sleep 3; done
  return 1
}

# wait_phase_left_running NAME -> block (up to PHASE_WAIT) until NAME's observed
# phase is a non-empty value other than running (i.e. the poweroff converged and
# the control plane will now exclude it from the pool).
wait_phase_left_running() {
  local name="$1" deadline got; deadline=$(( SECONDS + PHASE_WAIT ))
  info "waiting for $name observed phase to leave running (<= ${PHASE_WAIT}s)"
  while (( SECONDS < deadline )); do
    got="$(vm_phase "$name")"
    [[ -n "$got" && "$got" != "running" ]] && { pass "$name phase=$got (left running)"; return 0; }
    sleep 2
  done
  fail "$name still 'running' after ${PHASE_WAIT}s (poweroff did not converge)"
}

# node_path HANDLE SRC -> a path to SRC usable INSIDE the node. On netns the host
# filesystem is shared, so the host path works as-is; on Lima the file is copied.
node_path() {
  local handle="$1" src="$2" base
  base="$(basename "$src")"
  case "$SMOKE_PLATFORM" in
    netns) printf '%s' "$src" ;;
    lima)  limactl cp "$src" "$handle:/tmp/$base" >/dev/null; printf '/tmp/%s' "$base" ;;
  esac
}

# gw_serve -> launch the gateway-only agent data plane on the dedicated node (detached).
gw_serve() {
  run_on "$GW_HANDLE" sudo sh -c 'setsid nohup otherix-agent serve >/var/log/otherix-agent-gateway.log 2>&1 < /dev/null &'
}

# wait_gw_ready -> block (up to GW_READY_WAIT) until the gateway reports NET ready.
wait_gw_ready() {
  local deadline; deadline=$(( SECONDS + GW_READY_WAIT ))
  while (( SECONDS < deadline )); do gw_overlay_ready && return 0; sleep 3; done
  return 1
}

# --- node-side operator CLI plumbing (resolved in preconditions) -------
# NODE_OTX / NODE_CFG: the staged operator CLI and its reachability + credential
# + trust config on the gateway node. NODE_WORK is a node-local scratch dir.
NODE_OTX=""
NODE_CFG=""
NODE_WORK=""
NODE_LB_CLIENT_PY=""

# lb_connect_start LOGFILE -> launch `otherix lb connect` on the gateway node,
# binding 127.0.0.1:LP inside the node, with the staged operator credential.
lb_connect_start() {
  local logf="$1"
  run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" \
    lb connect "$LB_NAME" --listen "127.0.0.1:${LP}" >"$logf" 2>&1 &
}

# lb_connect_stop -> stop any `otherix lb connect` listener on the node.
lb_connect_stop() {
  run_on "$NODE_CLI_HANDLE" pkill -f "$NODE_OTX lb connect" >/dev/null 2>&1 || true
}

# lb_hits N -> make N short connections to the node's local lb-connect listener
# and print the identity each backend returned, one per line ("NOHIT" on a
# connection that carried no bytes - i.e. a refused / no-eligible connect).
lb_hits() {
  run_on "$NODE_CLI_HANDLE" python3 "$NODE_LB_CLIENT_PY" 127.0.0.1 "$LP" "$1" 2>/dev/null
}

# --- scratch + the guest cloud-config + the balancing client -----------
WORKDIR="$(mktemp -d)"
NC_FILE="${WORKDIR}/network-config.yaml"

# network-config (netplan v2): DHCP the single guest NIC, marked optional so
# systemd-networkd-wait-online does not block boot waiting for the lease.
cat >"$NC_FILE" <<'EOF'
network:
  version: 2
  ethernets:
    guest-nic:
      match:
        name: "en*"
      dhcp4: true
      dhcp6: false
      optional: true
EOF

# write_cloud_init IDENTITY OUTFILE -> render the backend guest cloud-config. A
# first-boot runcmd writes a tiny python3 server bound to $PORT that answers every
# connection with IDENTITY (the caller reads it back through the load balancer to
# tell which backend served the connection) and an announce loop that prints
# "OTHERIX_GUEST_IP <ip>" to /dev/console once the NIC has an IPv4, which the
# harness reads off the captured serial.log. Writes go to /dev/console - the
# kernel's active console maps to the serial device the host captures (ttyAMA0 on
# arm64, ttyS0 on amd64), which a fixed tty name would get wrong across arches.
write_cloud_init() {
  local identity="$1" outfile="$2"
  cat >"$outfile" <<EOF
#cloud-config
write_files:
  - path: /usr/local/bin/otherix-lb-server.py
    permissions: '0755'
    content: |
      import socket
      IDENTITY = "${identity}"
      s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
      s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
      s.bind(('0.0.0.0', ${PORT}))
      s.listen(16)
      while True:
          c, _ = s.accept()
          try:
              c.sendall((IDENTITY + "\n").encode())
          except OSError:
              pass
          finally:
              c.close()
  - path: /usr/local/bin/otherix-announce-ip.sh
    permissions: '0755'
    content: |
      #!/bin/sh
      SC=/dev/console
      while :; do
        IP=\$(ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1)
        [ -n "\$IP" ] && echo "OTHERIX_GUEST_IP \$IP" > "\$SC"
        sleep 2
      done
runcmd:
  - [ sh, -c, "setsid nohup python3 /usr/local/bin/otherix-lb-server.py >/dev/null 2>&1 < /dev/null &" ]
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-announce-ip.sh >/dev/null 2>&1 < /dev/null &" ]
EOF
}

# The balancing client: make N short, sequential connections to the local
# lb-connect listener, read the identity line each backend returns, and print one
# token per attempt - the identity, or "NOHIT" when the connection carried no
# bytes (a refused / no-eligible connect the broker tore down). Each attempt is
# bounded by a socket timeout so a no-eligible run never hangs. Staged on the node.
LB_CLIENT_PY="${WORKDIR}/lb_client.py"
cat >"$LB_CLIENT_PY" <<'PYEOF'
import socket, sys

host, port, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
for _ in range(n):
    try:
        s = socket.create_connection((host, port), timeout=8)
        s.settimeout(8)
        buf = b""
        while b"\n" not in buf:
            chunk = s.recv(256)
            if not chunk:
                break
            buf += chunk
        s.close()
        line = buf.split(b"\n", 1)[0].decode(errors="replace").strip()
        print(line if line else "NOHIT")
    except OSError:
        print("NOHIT")
PYEOF

# --- background-process bookkeeping + cleanup --------------------------
BG_PIDS=()
track_bg() { BG_PIDS+=("$1"); }
kill_bg()  { local p; for p in "${BG_PIDS[@]:-}"; do [ -n "$p" ] || continue; kill "$p" 2>/dev/null || true; pkill -P "$p" 2>/dev/null || true; done; }

GW_LAUNCHED=""

# kill_node_cli -> stop any straggler operator CLI processes on the gateway node.
kill_node_cli() {
  [ -n "$NODE_OTX" ] || return 0
  run_on "$NODE_CLI_HANDLE" sh -c "pkill -f '$NODE_OTX'; pkill -f 'lb_client'" >/dev/null 2>&1 || true
}

cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the LB/VMs/network/gateway up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  kill_bg
  kill_node_cli
  run_on "$GW_HANDLE" sudo pkill -f 'otherix-agent serve' >/dev/null 2>&1 || true
  if [ "$SMOKE_PLATFORM" = "lima" ] && [ -n "$NODE_OTX" ]; then
    run_on "$NODE_CLI_HANDLE" rm -rf \
      "$NODE_WORK" "$NODE_OTX" "$NODE_CFG" "$NODE_LB_CLIENT_PY" >/dev/null 2>&1 || true
  fi
  otx lb delete "$LB_NAME" --force >/dev/null 2>&1 || true
  otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  # Bring the dedicated node's agent back so a subsequent run finds three hosts.
  if [ -n "$GW_LAUNCHED" ]; then
    info "restart the agent on the gateway node (best effort) so the stack returns to three hosts"
    run_on "$GW_HANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl start otherix-agent' >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== lb smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v python3 >/dev/null || fail "python3 is required on the host"
command -v go >/dev/null || fail "go is required on the host (to cross-build the node CLI)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" lb create --help >/dev/null 2>&1 || fail "this otherix build has no 'lb create' command (rebuild from the current tree)"
"$OTX" lb connect --help >/dev/null 2>&1 || fail "this otherix build has no 'lb connect' command (rebuild from the current tree)"
# The gateway + operator CLI run on a linux node; pick the cross-build for that
# node's arch (the host may be macOS), building on demand.
GW_ARCH="$(run_on "$GW_HANDLE" uname -m 2>/dev/null)"
case "$GW_ARCH" in
  aarch64) GW_GOARCH=arm64 ;;
  x86_64)  GW_GOARCH=amd64 ;;
  *) fail "unsupported gateway node arch '${GW_ARCH:-unknown}'" ;;
esac
if [ -z "$GW_BIN" ]; then
  GW_BIN="bin/linux-${GW_GOARCH}/otherix-agent"
  [ -x "$GW_BIN" ] || make "build-linux-${GW_GOARCH}" >/dev/null 2>&1 || true
fi
[ -x "$GW_BIN" ] || fail "otherix-agent not found at '$GW_BIN' (run make build-linux-arm64 / build-linux-amd64, or set GW_BIN=...)"
if [ -z "$CLI_BIN" ]; then
  CLI_BIN="bin/linux-${GW_GOARCH}/otherix-cli"
  info "cross-building the operator CLI for linux/${GW_GOARCH}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$GW_GOARCH" go build -trimpath -o "$CLI_BIN" ./cmd/cli \
    || fail "failed to cross-build the operator CLI for linux/${GW_GOARCH}"
fi
[ -x "$CLI_BIN" ] || fail "operator CLI cross-build not found at '$CLI_BIN' (set CLI_BIN=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
for n in "$NODE1" "$NODE2"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done
info "vm hosts: $NODE1 + $NODE2; gateway node handle=$GW_HANDLE"
pass "CP up (${CP_VERSION}); $NODE1 + $NODE2 ready"

# --- provision the operator CLI on the gateway node --------------------
echo "=== preconditions: stage the operator CLI on the gateway node ==="
# The node reaches the control plane on its public API host (the agent dials the
# same host for mTLS; the public API shares it on the public port). Derive that
# URL from a running agent's config rather than hard-coding it.
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
_agent_url="$(run_on "$SMOKE_HANDLE_1" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)"
_agent_host="${_agent_url#*://}"; _agent_host="${_agent_host%%:*}"
_pub_port="${CP_BASE##*:}"; { [ -n "$_pub_port" ] && [ "$_pub_port" != "$CP_BASE" ]; } || _pub_port=8080
[ -n "$_agent_host" ] || fail "could not resolve the node-facing CP host (set NODE_CP_URL=https://host:port)"
NODE_CP_URL="${NODE_CP_URL:-https://${_agent_host}:${_pub_port}}"
info "node CLI -> control plane at ${NODE_CP_URL}"

# The node trusts the dev cluster CA (the CA that signs both the public CP cert
# and the gateway cert), so a verified (no insecure-skip) dial works for both the
# broker call and the gateway data-plane leg.
CLUSTER_CA_PEM="${CLUSTER_CA_PEM:-.local/pki/cluster-ca.crt}"
[ -f "$CLUSTER_CA_PEM" ] || fail "cluster CA PEM not found at '$CLUSTER_CA_PEM' (set CLUSTER_CA_PEM=...)"
NODE_TOKEN="$("$OTX" config show --show-token 2>/dev/null | awk -F': ' '/^token:/{print $2}')"
[ -n "$NODE_TOKEN" ] || fail "could not read the operator token from the host CLI config (run otherix config add cluster)"
NODE_CA_B64="$(base64 < "$CLUSTER_CA_PEM" | tr -d '\n')"
cat >"${WORKDIR}/node-config" <<EOF
apiVersion: v1
kind: Config
current-cluster: smoke
clusters:
    - name: smoke
      server: ${NODE_CP_URL}
      token: ${NODE_TOKEN}
      certificate-authority-data: ${NODE_CA_B64}
EOF

# Stage the CLI binary, its config, and the balancing client on the node.
NODE_OTX="$(node_path "$NODE_CLI_HANDLE" "$CLI_BIN")"
run_on "$NODE_CLI_HANDLE" chmod +x "$NODE_OTX" >/dev/null 2>&1 || true
NODE_CFG="$(node_path "$NODE_CLI_HANDLE" "${WORKDIR}/node-config")"
NODE_LB_CLIENT_PY="$(node_path "$NODE_CLI_HANDLE" "$LB_CLIENT_PY")"

case "$SMOKE_PLATFORM" in
  netns) NODE_WORK="$WORKDIR" ;;
  lima)  NODE_WORK="/tmp/otx-lb-$$" ;;
esac
run_on "$NODE_CLI_HANDLE" sh -c "mkdir -p '$NODE_WORK'" || fail "could not prepare the node scratch dir at $NODE_WORK"

# Prove the staged CLI talks to the control plane with a VERIFIED TLS dial (no
# insecure-skip) before relying on it for the brokered connect.
run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" node list >/dev/null 2>&1 \
  || fail "the staged node CLI could not reach/verify the control plane at ${NODE_CP_URL} (TLS trust or reachability)"
pass "operator CLI staged on $GW_HANDLE; verified TLS dial to ${NODE_CP_URL}"

# best-effort delete-first so stale leftovers from a prior run do not clash
otx lb delete "$LB_NAME" --force >/dev/null 2>&1 || true
otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true

# --- step 1: overlay network + two labelled backends -------------------
echo "=== step 1: dhcp overlay $NET + two labelled backends ($LABEL_KEY=$LABEL_VAL) ==="
otx network create "$NET" --type overlay --subnet "$SUBNET" --dhcp \
  || fail "network create $NET failed"
wait_net_ready "$NODE1" || fail "$NET did not reconcile ready on $NODE1 within ${NET_WAIT}s"
wait_net_ready "$NODE2" || fail "$NET did not reconcile ready on $NODE2 within ${NET_WAIT}s"
pass "$NET reconciled ready on $NODE1 and $NODE2"

# wait_guest_ip NAME NODE_INDEX VMID -> block until the guest announces an IP on
# its serial console (also proves the backend server's boot path ran).
wait_guest_ip() {
  local name="$1" idx="$2" id="$3" handle state deadline ip=""
  handle="$(smoke_handle "$idx")"; state="$(smoke_state "$idx")"
  deadline=$(( SECONDS + GUEST_IP_WAIT ))
  info "waiting for $name to announce its overlay IP (<= ${GUEST_IP_WAIT}s)"
  while (( SECONDS < deadline )); do
    ip="$(run_on "$handle" sudo grep -oE 'OTHERIX_GUEST_IP [0-9.]+' \
        "${state}/vms/${id}/serial.log" 2>/dev/null | awk '{print $2}' | tail -n1)" || true
    [ -n "$ip" ] && { pass "$name overlay IP is $ip"; return 0; }
    sleep 5
  done
  fail "$name never announced an overlay IP within ${GUEST_IP_WAIT}s"
}

# create_backend NAME NODE NODE_INDEX -> render the guest cloud-config (answering
# with NAME as its identity), create the VM pinned to NODE with the pool label,
# wait it running, and wait its guest IP announce.
create_backend() {
  local name="$1" node="$2" idx="$3" ci="${WORKDIR}/cloud-init-${1}.yaml" id
  write_cloud_init "$name" "$ci"
  info "creating $name on $node (label $LABEL_KEY=$LABEL_VAL, server on :${PORT})"
  otx vm create "$name" \
    --image-url "$IMAGE_URL" --arch "$ARCH" --node "$node" --network "$NET" \
    --vcpus 2 --memory-mib 2048 \
    --label "${LABEL_KEY}=${LABEL_VAL}" \
    --user-data "$ci" --network-config "$NC_FILE" \
    --wait --wait-timeout "${CREATE_WAIT}s" \
    || fail "vm create $name did not reach running within ${CREATE_WAIT}s"
  id="$(otx vm get "$name" --output json | jq -r '.id')"
  [[ "$id" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $name VM id (got '$id')"
  pass "$name running on $node (id=$id)"
  wait_guest_ip "$name" "$idx" "$id"
}

create_backend "$VM_A" "$NODE1" 1
create_backend "$VM_B" "$NODE2" 2

# --- step 2: join an ingress gateway -----------------------------------
echo "=== step 2: join ingress gateway $GW_NAME (converged data path for the overlay backends) ==="
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
GW_CP_URL="${GW_CP_URL:-$(run_on "$GW_HANDLE" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)}"
[ -n "$GW_CP_URL" ] || fail "could not resolve the gateway control-plane URL (set GW_CP_URL=https://...:PORT)"
GW_NODE_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1")"
[ -n "$GW_NODE_IP" ] || fail "could not resolve the gateway node IP"

# The control advertised endpoint is what the control plane dials for the
# post-cutover heartbeat nudge (and health). In the Lima dev stack the CP runs on
# the host and reaches each node only through its per-VM host port-forward
# (127.0.0.1:9442+index -> guest:9443), exactly as seed-dev advertises the VM-host
# nodes; the node's inter-VM address is not routable from the host CP. So the
# control endpoint uses the forwarded host port, while the ingress endpoint (dialed
# by the in-node client) keeps the node IP. Distinct reachability, distinct
# endpoints.
GW_CONTROL_ADVERTISED="${GW_CONTROL_ADVERTISED:-https://127.0.0.1:$((9442 + GW_INDEX))}"

# The gateway-only agent joins the same overlay substrate as any node, but its
# regenerated agent.yaml carries no wireguard block, so the gateway runs with an
# empty WG advertised endpoint. Ingress still works because the gateway is always
# the WireGuard initiator: it dials the handshake out to each host, and roaming
# carries the return path, so no configured endpoint is needed.

# Mint a gateway join token through the operator CLI (the --kind gateway flow).
TOK_JSON="$(otx node join-token create --kind gateway --node-name "$GW_NAME" --ttl 1h --output json)" \
  || fail "join-token create --kind gateway failed"
GW_TOKEN="$(jq -r '.token' <<<"$TOK_JSON")"
GW_FP="$(jq -r '.ca_fingerprint_sha256' <<<"$TOK_JSON")"
[ -n "$GW_TOKEN" ] && [ -n "$GW_FP" ] || fail "join-token bundle missing token or fingerprint"

# Free the gateway node: stop its agent so the gateway owns the overlay substrate.
info "stop the agent on the gateway node so the gateway owns the overlay datapath"
run_on "$GW_HANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl stop otherix-agent' >/dev/null 2>&1 || true
run_on "$GW_HANDLE" sudo pkill -f 'otherix-agent serve' >/dev/null 2>&1 || true

# Stage the gateway-only agent binary and bootstrap + serve on the node.
GW_BIN_NODE="$(node_path "$GW_HANDLE" "$GW_BIN")"
run_on "$GW_HANDLE" sudo install -m 0755 "$GW_BIN_NODE" /usr/local/bin/otherix-agent >/dev/null 2>&1 \
  || run_on "$GW_HANDLE" sudo cp "$GW_BIN_NODE" /usr/local/bin/otherix-agent
# Remove the node's prior agent config so bootstrap writes a fresh gateway-enabled
# agent.yaml (bootstrap never overwrites an existing config, even with --force).
# The gateway block is what makes `otherix-agent serve` boot the gateway-only
# runtime instead of the VM-host runtime.
run_on "$GW_HANDLE" sudo rm -f /etc/otherix/agent.yaml >/dev/null 2>&1 || true
# Also drop any gateway WireGuard key left by an earlier run: the gateway is a
# fresh node identity each run, so it must mint a fresh keypair. Reusing the prior
# run's key would present a public key the control plane still has registered to
# the earlier (now gone) gateway node and reject the heartbeat as a duplicate.
run_on "$GW_HANDLE" sudo rm -f /var/lib/otherix/wg-gateway/private.key >/dev/null 2>&1 || true
# --force re-issues the cert material: the repurposed node carries the VM-host
# agent's cert, and --force makes the re-run idempotent over a prior identity.
# --gateway bakes the gateway block; --ingress-* set the ingress splicer clients
# dial. The control --advertised-endpoint/--migration-host/--listen stay as a
# node's would (migration inputs are inert for a gateway but the validator wants
# them).
run_on "$GW_HANDLE" sudo otherix-agent bootstrap --force --gateway \
  --ingress-advertised-endpoint "https://${GW_NODE_IP}:${GW_INGRESS_PORT}" \
  --ingress-listen "0.0.0.0:${GW_INGRESS_PORT}" \
  --token "$GW_TOKEN" --ca-fingerprint "$GW_FP" \
  --cp-url "$GW_CP_URL" --node-name "$GW_NAME" \
  --advertised-endpoint "${GW_CONTROL_ADVERTISED}" \
  --migration-host "${GW_NODE_IP}" \
  --heartbeat-interval "${GW_HEARTBEAT_INTERVAL:-5s}" \
  --listen "0.0.0.0:${GW_LISTEN_PORT}" \
  || fail "gateway-only agent bootstrap failed"
gw_serve
GW_LAUNCHED=1
pass "ingress gateway $GW_NAME bootstrapped and serving on $GW_HANDLE"

info "waiting for gateway $GW_NAME to report $NET ready (<= ${GW_READY_WAIT}s)"
wait_gw_ready || fail "gateway $GW_NAME did not report $NET ready within ${GW_READY_WAIT}s"
pass "gateway $GW_NAME reports $NET converged (forwarding database programmed)"

# --- step 3: register the load balancer + forward a local port ---------
echo "=== step 3: register load balancer $LB_NAME over $LABEL_KEY=$LABEL_VAL (port $PORT) ==="
otx lb create "$LB_NAME" --port "$PORT" --selector "${LABEL_KEY}=${LABEL_VAL}" \
  || fail "lb create $LB_NAME failed"
pass "load balancer $LB_NAME created (selector $LABEL_KEY=$LABEL_VAL, port $PORT)"

FWD_LOG="${WORKDIR}/lb-connect.log"
lb_connect_start "$FWD_LOG"
LB_PID=$!; track_bg "$LB_PID"
deadline=$(( SECONDS + 20 )); ok=0
while (( SECONDS < deadline )); do grep -q 'forwarding ' "$FWD_LOG" 2>/dev/null && { ok=1; break; }; sleep 1; done
(( ok == 1 )) || fail "lb connect did not open a local listener on the node: $(cat "$FWD_LOG" 2>/dev/null)"
pass "lb connect forwarding 127.0.0.1:$LP -> load balancer $LB_NAME on $GW_HANDLE"

# --- step 4: balancing - both backends answer --------------------------
echo "=== step 4: $BALANCE_HITS connections balance across both backends ==="
HITS="$(lb_hits "$BALANCE_HITS")"
info "backend hit distribution:"; sort <<<"$HITS" | uniq -c | sed 's/^/    /' || true
grep -Fq "$VM_A" <<<"$HITS" || fail "backend $VM_A never answered across $BALANCE_HITS connections (not balancing): $(tr '\n' ' ' <<<"$HITS")"
grep -Fq "$VM_B" <<<"$HITS" || fail "backend $VM_B never answered across $BALANCE_HITS connections (not balancing): $(tr '\n' ' ' <<<"$HITS")"
pass "both backends served connections ($VM_A and $VM_B) - the load balancer balances across the pool"

# --- step 5: eligibility - a stopped backend is excluded ---------------
echo "=== step 5: poweroff $VM_B; the pool excludes it ==="
otx vm poweroff "$VM_B" --wait --wait-timeout "${OP_WAIT}s" \
  || fail "poweroff $VM_B did not complete within ${OP_WAIT}s"
wait_phase_left_running "$VM_B"
HITS2="$(lb_hits "$EXCLUDE_HITS")"
info "backend hit distribution after $VM_B poweroff:"; sort <<<"$HITS2" | uniq -c | sed 's/^/    /' || true
grep -Fq "$VM_A" <<<"$HITS2" || fail "surviving backend $VM_A stopped answering after $VM_B was powered off: $(tr '\n' ' ' <<<"$HITS2")"
if grep -Fq "$VM_B" <<<"$HITS2"; then
  fail "stopped backend $VM_B still received traffic - eligibility did not exclude it: $(tr '\n' ' ' <<<"$HITS2")"
fi
pass "stopped backend $VM_B excluded; all traffic served by $VM_A"

# --- step 6: no eligible backend - the load balancer fails closed ------
echo "=== step 6: poweroff $VM_A too; a connect finds no eligible backend ==="
otx vm poweroff "$VM_A" --wait --wait-timeout "${OP_WAIT}s" \
  || fail "poweroff $VM_A did not complete within ${OP_WAIT}s"
wait_phase_left_running "$VM_A"
HITS3="$(lb_hits "$NOELIG_HITS")"
info "connection results with no eligible backend:"; sort <<<"$HITS3" | uniq -c | sed 's/^/    /' || true
if grep -Fq "$VM_A" <<<"$HITS3" || grep -Fq "$VM_B" <<<"$HITS3"; then
  fail "a backend still answered after both were powered off - the no-eligible path did not fail closed: $(tr '\n' ' ' <<<"$HITS3")"
fi
pass "no eligible backend: every connect was refused (no backend answered) - the load balancer fails closed"

kill "$LB_PID" 2>/dev/null || true; wait "$LB_PID" 2>/dev/null || true
lb_connect_stop

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the LB + VMs + network, stops the node-side CLI and
# gateway, and restarts the gateway node's agent so a re-run finds three hosts.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== lb smoke PASSED ===${NC}"
echo "  the load balancer balances new connections across both labelled backends"
echo "  a powered-off backend is excluded from the pool (eligibility)"
echo "  with no eligible backend the load balancer fails closed (no bytes served)"
