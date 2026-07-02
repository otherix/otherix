#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Ingress-grant smoke - proves an EXTERNAL grant reaches a VM over the ingress
# data path, scoped to exactly the granted guest ports, and survives a live
# migration. An operator mints a per-person grant with `otherix ingress-grant
# create`; the grant holder then reaches the guest with `otherix forward`
# presenting ONLY the grant token as its credential (no operator login, no
# ownership) - exactly the external-user model the grant exists for.
#
# What it proves, end to end (operator + grant-holder CLI only):
#   - an operator mints a grant scoped to a single guest TCP port on an OVERLAY
#     VM (`otherix ingress-grant create ext --vm <vm>:<port> --login <user>`),
#     and the grant holder reaches the guest echo server through the converged
#     gateway with `otherix forward <vm> <port>` carrying only the grant token;
#   - PORT SCOPE: a brokered connection to a port NOT in the grant is refused
#     (uniform 404 - the broker leaks neither the VM's existence nor the grant's
#     scope), so the grant is a real per-port capability, not a VM-wide pass;
#   - SOURCE-IP PIN: a grant pinned with `--source-ip` to a CIDR the caller is
#     not in is refused (uniform 404) even for an in-scope port, so the pin is
#     enforced at the broker;
#   - SEAMLESS LIVE MIGRATION: a held grant-holder forward session keeps its byte
#     stream round-tripping while the guest is live-migrated to another node -
#     the gateway data path survives the cutover with no multi-second gap and no
#     dropped session;
#   - BRIDGE RELAY: for a managed-bridge VM (no overlay/gateway path) the grant
#     holder's forward reaches the guest through the control-plane relay, and an
#     out-of-scope port is refused there too - so the relay path enforces the
#     same per-port scope as the gateway path and is not regressed.
#
# HOW SEAMLESSNESS IS MEASURED:
#   A continuous-traffic client holds ONE TCP connection open to the grant
#   holder's local `otherix forward` listener and sends a monotonically
#   increasing line every TICK seconds, reading each line echoed back by the
#   guest and recording its arrival time. Because the forward brokers once per
#   accepted connection and splices it through the gateway, that single
#   connection rides the gateway data path; during the migration the client keeps
#   sending, and the smoke asserts the LARGEST gap between consecutive echoes
#   stayed under GAP_THRESHOLD and that the session never dropped.
#
# WHERE THE GRANT-HOLDER CLI RUNS:
#   The brokered DATA-PLANE forward (overlay + bridge) runs INSIDE the dedicated
#   gateway node, not on the host - in this dev stack the host reaches the
#   control plane but NOT the gateway's data-plane endpoint (no route to the
#   inter-node subnet), so a host-run forward would broker fine and then fail to
#   dial the gateway. The node reaches both. The grant-holder credential is the
#   grant token alone: the node CLI is staged with a config that carries only the
#   public API URL + the cluster CA (reachability + TLS trust, not a secret), and
#   the grant token is presented through OTHERIX_API_TOKEN, which overrides the
#   config's operator token in the CLI credential precedence. So the forward runs
#   with exactly the grant token as its bearer.
#
#   The broker-STATUS assertions (out-of-scope port, source-IP pin) resolve to a
#   404 in the broker BEFORE any data plane is selected, so they run from the
#   host CLI with the grant token passed via `--token`.
#
# GATEWAY PLACEMENT (dev stack): identical to the ingress-gateway smoke - the
#   third dev node is repurposed as the gateway (its agent is stopped), leaving
#   the first two nodes as VM hosts for the create + migrate.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# plus jq, python3, and go on the host.
#
# Usage: make smoke-ingress-grant   (or: bash dev/smoke/ingress-grant/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
# Resolved in preconditions to the linux cross-builds matching the gateway node's
# arch (the node runs linux, the host may be macOS), overridable.
GW_BIN="${GW_BIN:-}"
CLI_BIN="${CLI_BIN:-}"

# A unique per-run suffix keeps grants/VMs re-runnable without a clean restart:
# the cluster has no node-delete for the gateway, and a fixed grant/VM name would
# collide with a half-torn prior run.
RUN="${RUN:-$(date +%H%M%S)-${RANDOM}}"

NODE1="node-1"                              # VM host + live-migration source
NET="ingressgr-ovl"                         # dhcp-enabled overlay
SUBNET="${SUBNET:-10.95.0.0/24}"            # unlikely to clash with the dev stack
VM="ingress-grant-vm-${RUN}"                # the forwarded overlay guest
GW_NAME="${GW_NAME:-ingress-grant-gw-${RUN}}"
ECHO_PORT="${ECHO_PORT:-9000}"              # the granted guest echo-server port
OFF_PORT="${OFF_PORT:-9001}"                # a guest port NOT in any grant
GW_LISTEN_PORT="${GW_LISTEN_PORT:-9443}"    # the gateway control listener (mTLS)
GW_INGRESS_PORT="${GW_INGRESS_PORT:-9444}"  # the gateway ingress listener clients dial for /v1/connect

# The bridge guest (relay-path proof): a managed bridge so the agent learns the
# guest IP from the CP-IPAM DHCP reservation, exactly as the relay resolves it.
NET_BR="ingressgr-br"                       # managed bridge network
BR_SUBNET="${BR_SUBNET:-10.96.0.0/24}"      # unlikely to clash with the dev stack
BR_NAME="otxgrbr0"                          # Linux bridge device (IFNAMSIZ-safe)
VM_BR="ingress-grant-br-${RUN}"             # the relay-path guest

# Grant names (unique per run).
GRANT="ig-ext-${RUN}"                       # overlay grant, single granted port
GRANT_SRC="ig-src-${RUN}"                   # source-IP-pinned grant (wrong CIDR)
GRANT_BR="ig-br-${RUN}"                     # bridge grant, single granted port
# A CIDR the caller is guaranteed NOT to be in (TEST-NET-3, RFC 5737).
WRONG_CIDR="${WRONG_CIDR:-203.0.113.0/24}"

LOGIN="${LOGIN:-ubuntu}"                     # default user in the Noble minimal cloudimg

# Local listener ports for `otherix forward`. FWD_PORT is the node-side data-plane
# listener; PROBE_FWD_PORT is the host-side broker-status probes (broker only).
FWD_PORT="${FWD_PORT:-19010}"
PROBE_FWD_PORT="${PROBE_FWD_PORT:-19011}"

# The gateway runs on the third dev node (its agent is stopped first).
GW_INDEX="${GW_INDEX:-3}"

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"           # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"                 # seconds for a network to reconcile ready
GW_READY_WAIT="${GW_READY_WAIT:-180}"       # seconds for the gateway to report overlay-ready
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"       # seconds for the guest to lease an overlay IP
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"         # seconds for a live migrate cutover
TICK="${TICK:-0.2}"                         # seconds between continuous-traffic lines
GAP_THRESHOLD="${GAP_THRESHOLD:-10}"        # max tolerated seconds with no echo across a cutover

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

GW_HANDLE="$(smoke_handle "$GW_INDEX")"
# The grant-holder data-plane CLI runs on the gateway node (see "WHERE THE
# GRANT-HOLDER CLI RUNS"); it is the same node that hosts the gateway daemon.
NODE_CLI_HANDLE="$GW_HANDLE"

# net_ready NODE NET -> 0 when the network reconciled "ready" on NODE.
net_ready() {
  [[ "$(otx network get "$2" --output json 2>/dev/null \
      | jq -r --arg n "$1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
      == "ready" ]]
}

# gw_overlay_ready -> 0 when the gateway reports the overlay reconciled ready.
gw_overlay_ready() { net_ready "$GW_NAME" "$NET"; }

# vm_node VM -> the node the VM currently runs on.
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.current_node_id // .status.current_node // empty'; }

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
  local deadline=$(( SECONDS + GW_READY_WAIT ))
  while (( SECONDS < deadline )); do gw_overlay_ready && return 0; sleep 3; done
  return 1
}

# --- node-side grant-holder CLI plumbing (resolved in preconditions) ---
# NODE_OTX / NODE_CFG: the staged operator CLI and its reachability+trust config
# on the node. The grant token is presented at run time through OTHERIX_API_TOKEN,
# which overrides the config's operator token, so the forward runs with exactly
# the grant token as its bearer. NODE_WORK is a node-local scratch dir for the
# per-run stop / arrivals files (kept off the host because the forward listener
# binds the node's loopback, so its clients must run on the node too).
NODE_OTX=""
NODE_CFG=""
NODE_WORK=""
NODE_PROBE_PY=""
NODE_FWD_CLIENT_PY=""

# grant_forward_start VM PORT LPORT LOGFILE TOKEN -> launch `otherix forward` on
# the node AS THE GRANT HOLDER: the only credential is TOKEN (the grant token),
# injected through OTHERIX_API_TOKEN so it overrides the config's operator token.
# The listener binds 127.0.0.1:LPORT inside the node.
grant_forward_start() {
  local vm="$1" port="$2" lport="$3" logf="$4" token="$5"
  run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" OTHERIX_API_TOKEN="$token" "$NODE_OTX" \
    forward "$vm" "$port" --listen "127.0.0.1:${lport}" >"$logf" 2>&1 &
}

# node_forward_stop -> stop any `otherix forward` listener on the node.
node_forward_stop() {
  run_on "$NODE_CLI_HANDLE" pkill -f "$NODE_OTX forward" >/dev/null 2>&1 || true
}

# node_probe LPORT -> single-shot reachability probe against the node's forward
# listener; prints PROBE_OK / PROBE_NOECHO / PROBE_FAIL.
node_probe() {
  run_on "$NODE_CLI_HANDLE" python3 "$NODE_PROBE_PY" 127.0.0.1 "$1" 2>&1
}

# --- scratch + the host-side cloud-config/clients ----------------------
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

# The guest echo-server cloud-config. A first-boot runcmd writes a tiny python3
# line-echo server bound to the granted port and launches it detached; once the
# NIC has an IPv4 the guest announces it on the serial console as
# "OTHERIX_GUEST_IP <ip>" (repeatedly), which the harness reads off the captured
# serial.log to confirm the guest is up. Writes go to /dev/console - the kernel's
# active console maps to the serial device the host captures (ttyAMA0 on arm64,
# ttyS0 on amd64), which a fixed tty name would get wrong across architectures.
read -r -d '' CLOUD_INIT <<EOF || true
#cloud-config
write_files:
  - path: /usr/local/bin/otherix-echo.py
    permissions: '0755'
    content: |
      import socket
      s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
      s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
      s.bind(('0.0.0.0', ${ECHO_PORT}))
      s.listen(8)
      while True:
          c, _ = s.accept()
          try:
              while True:
                  d = c.recv(4096)
                  if not d:
                      break
                  c.sendall(d)
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
  - [ sh, -c, "setsid nohup python3 /usr/local/bin/otherix-echo.py >/dev/null 2>&1 < /dev/null &" ]
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-announce-ip.sh >/dev/null 2>&1 < /dev/null &" ]
EOF
[ -n "$CLOUD_INIT" ] || fail "internal: cloud-config came out empty"

# Continuous-traffic client (the seamlessness metric): holds ONE TCP connection
# open to the local `otherix forward` listener, sends a monotonic line every TICK
# seconds, and records the arrival time of each echoed line. Writer/reader run on
# separate threads so a brief cutover stall never deadlocks the loop. It exits
# when the stop file appears (or the hard deadline passes) and prints the largest
# gap between consecutive echoes. Staged on the node.
FWD_CLIENT_PY="${WORKDIR}/forward_client.py"
cat >"$FWD_CLIENT_PY" <<'PYEOF'
import os, socket, sys, threading, time

host, port = sys.argv[1], int(sys.argv[2])
arrivals_path, stop_path, max_secs, tick = sys.argv[3], sys.argv[4], float(sys.argv[5]), float(sys.argv[6])

s = socket.create_connection((host, port), timeout=30)
arrivals = open(arrivals_path, "w")
stop = threading.Event()
# Set when a thread sees the connection close before we asked it to stop - a
# clean cutover-time RST would otherwise pass the gap check (the surviving
# pre-drop echoes are evenly spaced) while having actually dropped the session.
dropped = threading.Event()

def writer():
    n = 0
    while not stop.is_set():
        try:
            s.sendall(("%d\n" % n).encode())
        except OSError:
            dropped.set()
            return
        n += 1
        time.sleep(tick)

def reader():
    rbuf = b""
    while not stop.is_set():
        try:
            d = s.recv(4096)
        except OSError:
            dropped.set()
            return
        if not d:
            dropped.set()
            return
        rbuf += d
        while b"\n" in rbuf:
            line, rbuf = rbuf.split(b"\n", 1)
            arrivals.write("%.3f %s\n" % (time.time(), line.decode(errors="replace")))
            arrivals.flush()

tw = threading.Thread(target=writer, daemon=True)
tr = threading.Thread(target=reader, daemon=True)
tw.start(); tr.start()

deadline = time.time() + max_secs
while time.time() < deadline:
    if os.path.exists(stop_path):
        break
    time.sleep(0.2)
stop.set()
try:
    s.close()
except OSError:
    pass

times = []
arrivals.flush()
with open(arrivals_path) as f:
    for ln in f:
        try:
            times.append(float(ln.split()[0]))
        except (ValueError, IndexError):
            pass
maxgap = 0.0
for i in range(1, len(times)):
    maxgap = max(maxgap, times[i] - times[i - 1])
print("FORWARD_CLIENT echoes=%d maxgap=%.3f dropped=%d" % (len(times), maxgap, 1 if dropped.is_set() else 0))
PYEOF

# Single-shot reachability probe: one connection to the local listener, one line
# echoed back. Prints PROBE_OK on a clean round-trip, PROBE_NOECHO / PROBE_FAIL
# otherwise. Staged on the node.
PROBE_PY="${WORKDIR}/forward_probe.py"
cat >"$PROBE_PY" <<'PYEOF'
import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
try:
    s = socket.create_connection((host, port), timeout=8)
    s.settimeout(8)
    s.sendall(b"otherix-probe\n")
    d = s.recv(64)
    s.close()
    print("PROBE_OK" if b"otherix-probe" in d else "PROBE_NOECHO")
except OSError as e:
    print("PROBE_FAIL %s" % e)
PYEOF

# forward_status_probe WANT LPORT OTXARGS... -> start `otherix <OTXARGS> --listen
# 127.0.0.1:LPORT` on the HOST, make ONE local connection (which forces a broker
# attempt), and assert the forward's stderr carries WANT (e.g. "HTTP 404"). This
# asserts the broker's status without any data plane, so it runs on the host: the
# broker rejects an out-of-scope port or a source-IP-pin miss BEFORE it selects a
# transport, and the forward prints the broker failure to stderr per connection.
forward_status_probe() {
  local want="$1" lport="$2"; shift 2
  local errf; errf="${WORKDIR}/forward-status-$$.err"
  "$OTX" "$@" --listen "127.0.0.1:${lport}" >/dev/null 2>"$errf" &
  local fpid=$!
  sleep 1
  python3 "$PROBE_PY" 127.0.0.1 "$lport" >/dev/null 2>&1 || true
  sleep 1
  kill "$fpid" 2>/dev/null || true; wait "$fpid" 2>/dev/null || true
  if grep -q "$want" "$errf"; then rm -f "$errf"; return 0; fi
  echo "--- forward stderr (wanted '${want}') ---" >&2; cat "$errf" >&2; rm -f "$errf"; return 1
}

# grant_token GRANT_NAME [CREATE-ARGS...] -> create GRANT_NAME as JSON and print
# its one-time token. CREATE-ARGS are the scope flags (--vm, --login, --source-ip).
grant_token() {
  local name="$1"; shift
  otx ingress-grant create "$name" "$@" -o json 2>/dev/null | jq -r '.token'
}

# --- background-process bookkeeping + cleanup --------------------------
BG_PIDS=()
track_bg() { BG_PIDS+=("$1"); }
kill_bg()  { local p; for p in "${BG_PIDS[@]:-}"; do [ -n "$p" ] || continue; kill "$p" 2>/dev/null || true; pkill -P "$p" 2>/dev/null || true; done; }

GW_LAUNCHED=""

# kill_node_cli -> stop any straggler grant-holder CLI processes on the node.
kill_node_cli() {
  [ -n "$NODE_OTX" ] || return 0
  run_on "$NODE_CLI_HANDLE" sh -c "pkill -f '$NODE_OTX'; pkill -f 'forward_client'" >/dev/null 2>&1 || true
}

cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the VMs/networks/gateway/grants up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  kill_bg
  kill_node_cli
  run_on "$GW_HANDLE" sudo pkill -f 'otherix-agent serve' >/dev/null 2>&1 || true
  if [ "$SMOKE_PLATFORM" = "lima" ] && [ -n "$NODE_OTX" ]; then
    run_on "$NODE_CLI_HANDLE" rm -rf \
      "$NODE_WORK" "$NODE_OTX" "$NODE_CFG" "$NODE_PROBE_PY" "$NODE_FWD_CLIENT_PY" >/dev/null 2>&1 || true
  fi
  otx ingress-grant delete "$GRANT" >/dev/null 2>&1 || true
  otx ingress-grant delete "$GRANT_SRC" >/dev/null 2>&1 || true
  otx ingress-grant delete "$GRANT_BR" >/dev/null 2>&1 || true
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM_BR" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  otx network delete "$NET_BR" --force >/dev/null 2>&1 || true
  # Bring the dedicated node's agent back so a subsequent run finds three hosts.
  if [ -n "$GW_LAUNCHED" ]; then
    info "restart the agent on the gateway node (best effort) so the stack returns to three hosts"
    run_on "$GW_HANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl start otherix-agent' >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== ingress-grant smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v python3 >/dev/null || fail "python3 is required on the host"
command -v go >/dev/null || fail "go is required on the host (to cross-build the node CLI)"
command -v curl >/dev/null || fail "curl is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" forward --help >/dev/null 2>&1 || fail "this otherix build has no 'forward' command (rebuild from the current tree)"
"$OTX" ingress-grant create --help >/dev/null 2>&1 || fail "this otherix build has no 'ingress-grant create' command (rebuild from the current tree)"
# The gateway + grant-holder CLI run on a linux node; pick the cross-build for
# that node's arch (the host may be macOS), building on demand.
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
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"

# Discover the migration TARGET: a ready node that is NOT the source and NOT the
# node we are about to repurpose as the gateway.
TARGET="$(otx node list --status ready --output json 2>/dev/null \
  | jq -r --arg src "$NODE1" '[.data[]? | select(.name != $src) | .name] | first // ""')"
[[ -n "$TARGET" ]] || fail "no second ready node found besides $NODE1 (run make local-dev-start)"
info "vm host source=$NODE1 target=$TARGET; gateway node handle=$GW_HANDLE"
pass "CP up (${CP_VERSION}); $NODE1 + $TARGET ready"

# --- provision the grant-holder CLI on the gateway node ----------------
echo "=== preconditions: stage the grant-holder CLI on the gateway node ==="
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

# The node trusts the dev cluster CA (the same CA that signs both the public CP
# cert and the gateway cert), so a verified (no insecure-skip) dial works for both
# the broker call and the gateway data-plane leg. This is reachability + trust,
# not a credential - the grant-holder credential is the grant token, injected via
# OTHERIX_API_TOKEN at run time (it overrides the config token below).
CLUSTER_CA_PEM="${CLUSTER_CA_PEM:-.local/pki/cluster-ca.crt}"
[ -f "$CLUSTER_CA_PEM" ] || fail "cluster CA PEM not found at '$CLUSTER_CA_PEM' (set CLUSTER_CA_PEM=...)"
# The staged config carries the operator token only so a plain (non-grant) call
# like `node list` verifies reachability below; every grant-holder forward
# overrides it with the grant token through OTHERIX_API_TOKEN.
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

# Stage the CLI binary, its config, and the data-plane client scripts on the node.
NODE_OTX="$(node_path "$NODE_CLI_HANDLE" "$CLI_BIN")"
run_on "$NODE_CLI_HANDLE" chmod +x "$NODE_OTX" >/dev/null 2>&1 || true
NODE_CFG="$(node_path "$NODE_CLI_HANDLE" "${WORKDIR}/node-config")"
NODE_PROBE_PY="$(node_path "$NODE_CLI_HANDLE" "$PROBE_PY")"
NODE_FWD_CLIENT_PY="$(node_path "$NODE_CLI_HANDLE" "$FWD_CLIENT_PY")"

case "$SMOKE_PLATFORM" in
  netns) NODE_WORK="$WORKDIR" ;;
  lima)  NODE_WORK="/tmp/otx-ingress-grant-$$" ;;
esac
run_on "$NODE_CLI_HANDLE" sh -c "mkdir -p '$NODE_WORK'" || fail "could not prepare the node scratch dir at $NODE_WORK"

# Prove the staged CLI talks to the control plane with a VERIFIED TLS dial (no
# insecure-skip) before relying on it for the brokered flows.
run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" node list >/dev/null 2>&1 \
  || fail "the staged node CLI could not reach/verify the control plane at ${NODE_CP_URL} (TLS trust or reachability)"
pass "grant-holder CLI staged on $GW_HANDLE; verified TLS dial to ${NODE_CP_URL}"

# best-effort delete-first so stale leftovers from a prior run do not clash
otx ingress-grant delete "$GRANT" >/dev/null 2>&1 || true
otx ingress-grant delete "$GRANT_SRC" >/dev/null 2>&1 || true
otx ingress-grant delete "$GRANT_BR" >/dev/null 2>&1 || true
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_BR" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
otx network delete "$NET_BR" --force >/dev/null 2>&1 || true

# --- step 1: overlay + the forwarded guest -----------------------------
echo "=== step 1: dhcp overlay $NET + guest $VM (echo server on :${ECHO_PORT}) ==="
otx network create "$NET" --type overlay --subnet "$SUBNET" --dhcp \
  || fail "network create $NET failed"
deadline=$(( SECONDS + NET_WAIT )); ok=0
info "waiting for $NET to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do net_ready "$NODE1" "$NET" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NET did not reconcile ready on $NODE1 within ${NET_WAIT}s"

info "creating $VM on $NODE1 (echo server on :${ECHO_PORT})"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --vcpus 2 --memory-mb 2048 \
  --user-data - --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
pass "$VM running on $NODE1 (id=$VMID)"

# Confirm the guest is up by its serial announce sentinel (also proves the echo
# server's boot path ran).
SRC_STATE="$(smoke_state 1)"
deadline=$(( SECONDS + GUEST_IP_WAIT )); GUEST_IP=""
info "waiting for the guest to announce its overlay IP (<= ${GUEST_IP_WAIT}s)"
while (( SECONDS < deadline )); do
  GUEST_IP="$(run_on "$SMOKE_HANDLE_1" sudo grep -oE 'OTHERIX_GUEST_IP [0-9.]+' \
      "${SRC_STATE}/vms/${VMID}/serial.log" 2>/dev/null | awk '{print $2}' | tail -n1)" || true
  [ -n "$GUEST_IP" ] && break
  sleep 5
done
[ -n "$GUEST_IP" ] || fail "guest never announced an overlay IP within ${GUEST_IP_WAIT}s"
pass "guest overlay IP is $GUEST_IP"

# --- step 2: join an ingress gateway -----------------------------------
echo "=== step 2: join ingress gateway $GW_NAME ==="
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
GW_CP_URL="${GW_CP_URL:-$(run_on "$GW_HANDLE" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)}"
[ -n "$GW_CP_URL" ] || fail "could not resolve the gateway control-plane URL (set GW_CP_URL=https://...:PORT)"
GW_NODE_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1")"
[ -n "$GW_NODE_IP" ] || fail "could not resolve the gateway node IP"

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
  --advertised-endpoint "https://${GW_NODE_IP}:${GW_LISTEN_PORT}" \
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

# --- step 3: mint the overlay grant + grant-holder forward -------------
echo "=== step 3: mint grant $GRANT (--vm $VM:$ECHO_PORT) + grant-holder forward ==="
GRANT_TOK="$(grant_token "$GRANT" --vm "${VM}:${ECHO_PORT}" --login "$LOGIN")"
[[ "$GRANT_TOK" == otx_ingressgrant_* ]] || fail "ingress-grant create did not return a grant token (got '${GRANT_TOK:-none}')"
pass "operator minted grant $GRANT scoped to $VM:$ECHO_PORT"

FWD_LOG="${WORKDIR}/forward.log"
grant_forward_start "$VM" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG" "$GRANT_TOK"
FWD_PID=$!; track_bg "$FWD_PID"
deadline=$(( SECONDS + 20 )); ok=0
while (( SECONDS < deadline )); do grep -q 'forwarding ' "$FWD_LOG" 2>/dev/null && { ok=1; break; }; sleep 1; done
(( ok == 1 )) || fail "grant-holder forward did not open a local listener on the node: $(cat "$FWD_LOG" 2>/dev/null)"

PROBE_OUT="$(node_probe "$FWD_PORT")"
grep -q PROBE_OK <<<"$PROBE_OUT" \
  || fail "grant-holder forward reachability probe failed: ${PROBE_OUT}; forward log: $(cat "$FWD_LOG" 2>/dev/null)"
pass "grant holder reached $VM:$ECHO_PORT through the gateway with only the grant token ($PROBE_OUT)"

# --- step 4: port scope - an out-of-scope port is refused --------------
echo "=== step 4: a port NOT in the grant is refused (404) ==="
# The broker rejects an out-of-scope port before selecting any transport, so this
# runs from the host with the grant token passed via --token (no data plane).
forward_status_probe "HTTP 404" "$PROBE_FWD_PORT" --token "$GRANT_TOK" forward "$VM" "$OFF_PORT" \
  || fail "grant broker did not refuse out-of-scope port $OFF_PORT with 404 (the grant must be per-port)"
pass "grant broker refused out-of-scope port $VM:$OFF_PORT with 404 (per-port scope enforced)"

# --- step 5: source-IP pin - a wrong-CIDR grant is refused -------------
echo "=== step 5: a source-IP-pinned grant (wrong CIDR) is refused (404) ==="
GRANT_SRC_TOK="$(grant_token "$GRANT_SRC" --vm "${VM}:${ECHO_PORT}" --login "$LOGIN" --source-ip "$WRONG_CIDR")"
[[ "$GRANT_SRC_TOK" == otx_ingressgrant_* ]] || fail "source-IP grant create did not return a token (got '${GRANT_SRC_TOK:-none}')"
# Same VM + in-scope port; the ONLY reason to refuse is the source-IP pin miss.
forward_status_probe "HTTP 404" "$PROBE_FWD_PORT" --token "$GRANT_SRC_TOK" forward "$VM" "$ECHO_PORT" \
  || fail "source-IP-pinned grant (pin $WRONG_CIDR) was not refused with 404 (the pin must be enforced)"
pass "grant pinned to $WRONG_CIDR refused an in-scope port with 404 (source-IP pin enforced)"

# --- step 6: seamless live migration -----------------------------------
echo "=== step 6: grant-holder forward survives live migration $NODE1 -> $TARGET ==="
NODE_ARR="${NODE_WORK}/arrivals.log"; NODE_STOP="${NODE_WORK}/stop"; OUT="${WORKDIR}/client.out"
run_on "$NODE_CLI_HANDLE" rm -f "$NODE_STOP" "$NODE_ARR" >/dev/null 2>&1 || true
run_on "$NODE_CLI_HANDLE" python3 "$NODE_FWD_CLIENT_PY" 127.0.0.1 "$FWD_PORT" \
  "$NODE_ARR" "$NODE_STOP" "$(( MIGRATE_WAIT + 60 ))" "$TICK" >"$OUT" 2>&1 &
CLIENT_PID=$!; track_bg "$CLIENT_PID"
sleep 3   # let the session establish and a few echoes flow before the cutover
otx vm migrate "$VM" --node "$TARGET" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "live migrate $NODE1 -> $TARGET did not complete within ${MIGRATE_WAIT}s"
NOW_NODE="$(vm_node "$VM")"
[[ "$NOW_NODE" == *"$TARGET"* ]] || info "post-migrate current node reported as '$NOW_NODE'"
sleep 3
run_on "$NODE_CLI_HANDLE" touch "$NODE_STOP" >/dev/null 2>&1 || true
wait "$CLIENT_PID" 2>/dev/null || true
OUT_TXT="$(cat "$OUT" 2>/dev/null)"; echo "$OUT_TXT"
MAXGAP="$(grep -oE 'maxgap=[0-9.]+' <<<"$OUT_TXT" | head -n1 | cut -d= -f2)"
ECHOES="$(grep -oE 'echoes=[0-9]+' <<<"$OUT_TXT" | head -n1 | cut -d= -f2)"
DROPPED="$(grep -oE 'dropped=[0-9]+' <<<"$OUT_TXT" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP" ] && [ -n "$ECHOES" ] || fail "continuous-traffic client did not report a metric: ${OUT_TXT}"
(( ECHOES > 0 )) || fail "no echoes recorded across the migration - the grant-holder session dropped"
[ "$DROPPED" = "0" ] || fail "the grant-holder session closed during the migration - not seamless (a cutover-time reset)"
awk -v g="$MAXGAP" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "session stalled ${MAXGAP}s across the cutover (>= ${GAP_THRESHOLD}s) - not seamless"
pass "grant-holder forward survived live migration (echoes=$ECHOES, max gap ${MAXGAP}s < ${GAP_THRESHOLD}s)"

kill "$FWD_PID" 2>/dev/null || true; wait "$FWD_PID" 2>/dev/null || true
node_forward_stop

# --- step 7: bridge relay path + scope ---------------------------------
echo "=== step 7: bridge VM $VM_BR - grant-holder forward over the relay + scope ==="
otx network create "$NET_BR" --type bridge --managed --bridge-name "$BR_NAME" \
  --egress nat --subnet "$BR_SUBNET" --dhcp \
  || fail "network create $NET_BR failed"
deadline=$(( SECONDS + NET_WAIT )); ok=0
info "waiting for $NET_BR to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do net_ready "$NODE1" "$NET_BR" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NET_BR did not reconcile ready on $NODE1 within ${NET_WAIT}s"

info "creating $VM_BR on $NODE1 (managed bridge $NET_BR, echo server on :${ECHO_PORT})"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM_BR" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET_BR" \
  --vcpus 2 --memory-mb 2048 \
  --user-data - --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_BR did not reach running within ${CREATE_WAIT}s"
pass "$VM_BR running on $NODE1 (bridge $NET_BR)"

GRANT_BR_TOK="$(grant_token "$GRANT_BR" --vm "${VM_BR}:${ECHO_PORT}" --login "$LOGIN")"
[[ "$GRANT_BR_TOK" == otx_ingressgrant_* ]] || fail "bridge grant create did not return a token (got '${GRANT_BR_TOK:-none}')"
pass "operator minted grant $GRANT_BR scoped to $VM_BR:$ECHO_PORT"

# A bridge guest has no overlay/gateway path, so the broker selects the
# control-plane relay transport; a clean echo round-trip proves the relay reaches
# the guest echo server for the grant holder. Retry while the guest boots.
BFWD_LOG="${WORKDIR}/forward-bridge.log"
deadline=$(( SECONDS + CREATE_WAIT )); ok=0
info "waiting for the grant-holder forward to reach $VM_BR over the relay (<= ${CREATE_WAIT}s)"
while (( SECONDS < deadline )); do
  node_forward_stop
  grant_forward_start "$VM_BR" "$ECHO_PORT" "$FWD_PORT" "$BFWD_LOG" "$GRANT_BR_TOK"
  BFWD_PID=$!; track_bg "$BFWD_PID"
  ldl=$(( SECONDS + 15 )); lok=0
  while (( SECONDS < ldl )); do grep -q 'forwarding ' "$BFWD_LOG" 2>/dev/null && { lok=1; break; }; sleep 1; done
  if (( lok == 1 )); then
    PROBE_BR="$(node_probe "$FWD_PORT")"
    grep -q PROBE_OK <<<"$PROBE_BR" && { ok=1; break; }
  fi
  kill "$BFWD_PID" 2>/dev/null || true; wait "$BFWD_PID" 2>/dev/null || true
  sleep 6
done
(( ok == 1 )) || fail "grant-holder forward never reached $VM_BR over the relay within ${CREATE_WAIT}s (last: ${PROBE_BR:-none}; log: $(cat "$BFWD_LOG" 2>/dev/null))"
pass "grant holder reached $VM_BR:$ECHO_PORT over the control-plane relay with only the grant token ($PROBE_BR)"
kill "$BFWD_PID" 2>/dev/null || true; wait "$BFWD_PID" 2>/dev/null || true
node_forward_stop

echo "=== step 8: an out-of-scope port on the bridge guest is refused (404) ==="
forward_status_probe "HTTP 404" "$PROBE_FWD_PORT" --token "$GRANT_BR_TOK" forward "$VM_BR" "$OFF_PORT" \
  || fail "bridge grant broker did not refuse out-of-scope port $OFF_PORT with 404 (the relay path must enforce per-port scope)"
pass "bridge grant broker refused out-of-scope port $VM_BR:$OFF_PORT with 404 (relay per-port scope not regressed)"

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the grants + VMs + networks, stops the node-side CLI and
# gateway, and restarts the gateway node's agent so a re-run finds three hosts.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== ingress-grant smoke PASSED ===${NC}"
echo "  external grant reaches an overlay guest through the gateway with only the grant token"
echo "  out-of-scope port refused (404); source-IP-pinned grant refused (404)"
echo "  grant-holder forward survives live migration; bridge relay path reaches the guest + refuses an out-of-scope port"
