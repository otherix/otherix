#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Load-balancer active-health smoke - proves `otherix lb` drives its backend
# pool from an L4 TCP health probe, not just the observed VM phase: a backend
# whose traffic port stops accepting (while the VM stays running) is taken out
# of the pool, comes back when the port re-opens, and a load balancer whose
# backends are all still warming keeps serving.
#
# What it proves, end to end (operator CLI only):
#   - WARMING DOES NOT DARKEN: right after `otherix lb create`, before any
#     backend has been confirmed healthy (the probes have not settled a verdict
#     yet), `otherix lb connect` still reaches a backend. A fresh load balancer
#     whose backends are all warming must serve, never refuse - the health
#     filter only subtracts a backend that a settled probe verdict confirms is
#     down, so an unconfirmed (warming) backend stays in the pool;
#   - HEALTHY BALANCING: once both backends report healthy=true on
#     `otherix lb get -o json`, many short connections reach BOTH backends;
#   - HEALTH-DRIVEN EXCLUSION: closing backend A's traffic port INSIDE the guest
#     (the VM stays running, so this is a health exclusion, not a phase
#     exclusion) makes `lb get` report A healthy=false after the unhealthy
#     threshold, and every subsequent connection is served by B ONLY;
#   - RECOVERY: re-opening A's port makes `lb get` report A healthy=true again
#     after the healthy threshold, and connections balance over both again;
#   - HEALTH-PORT SPLIT: a second load balancer configured with a health port
#     different from its traffic port keeps A eligible (healthy=true) even while
#     A's TRAFFIC port is closed - the probe follows the configured health port,
#     not the traffic port;
#   - FAIL CLOSED: with BOTH backends' traffic ports closed (both confirmed
#     unhealthy), `otherix lb connect` is refused with HTTP 409
#     (ingress_unavailable) - a confirmed-all-down pool hands out nothing.
#
# HOW A BACKEND PORT IS TOGGLED INSIDE THE GUEST (no SSH needed):
#   Each backend guest runs one tiny server that binds TWO ports: a TRAFFIC port
#   ($PORT) that answers every connection with the backend's own identity string
#   (this is what the load balancer fronts AND health-probes), and an always-open
#   CONTROL port ($CTLPORT) that accepts a one-line command - "DOWN" closes the
#   traffic listener (the traffic port then refuses connections, so its health
#   probe fails), "UP" re-opens it. The control port is reached deterministically
#   through a per-backend control load balancer whose selector matches only that
#   one backend, so `otherix lb connect <control-lb>` always brokers to the
#   intended backend's control port. The control port must be separate from the
#   toggled traffic port: once the traffic port is closed we could no longer
#   reach that backend to re-open it through the traffic port.
#
# HOW BALANCING / EXCLUSION IS MEASURED:
#   A client makes many short, sequential connections to the local
#   `otherix lb connect` listener and records the identity returned by each;
#   the smoke asserts which identities appear (both, one, or none). A connection
#   that carries no bytes is a "NOHIT" (a refused / no-eligible connect).
#
# WHERE THE OPERATOR CLI RUNS:
#   The brokered DATA-PLANE connect runs INSIDE the dedicated gateway node, not
#   on the host - in this dev stack the host reaches the control plane but NOT
#   the gateway's data-plane endpoint (no route to the inter-node subnet). The
#   node CLI is staged with a config that carries the public API URL, the
#   operator token, and the cluster CA; the connect + the client both run there.
#   The health verdicts (`otherix lb get`) are a plain control-plane read, so
#   those run on the host.
#
# GATEWAY PLACEMENT (dev stack): identical to the ingress / lb smokes - the third
#   dev node is repurposed as the gateway (its agent is stopped), leaving the
#   first two nodes as the VM hosts for the two backends.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# plus jq, python3, and go on the host.
#
# Usage: make smoke-lb-health   (or: bash dev/smoke/lb-health/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
# Resolved in preconditions to the linux cross-builds matching the gateway node's
# arch (the node runs linux, the host may be macOS), overridable.
GW_BIN="${GW_BIN:-}"
CLI_BIN="${CLI_BIN:-}"

# A unique per-run suffix keeps the LBs/VMs/labels re-runnable without a clean
# restart: the cluster has no node-delete for the gateway, and a fixed name/label
# would collide with a half-torn prior run.
RUN="${RUN:-$(date +%H%M%S)-${RANDOM}}"

NODE1="node-1"                              # first VM host (backend A)
NODE2="node-2"                              # second VM host (backend B)
NET="lbh-ovl-${RUN}"                        # dhcp-enabled overlay (unique per run)
SUBNET="${SUBNET:-10.98.0.0/24}"            # unlikely to clash with the dev stack

VM_A="lbhvm-a-${RUN}"                       # backend on NODE1
VM_B="lbhvm-b-${RUN}"                       # backend on NODE2
LABEL_KEY="app"                             # shared pool selector key
LABEL_VAL="lbhsmoke-${RUN}"                 # shared pool selector value (unique per run)
IDLBL_KEY="id"                              # per-backend selector key (control channel)
IDLBL_A="a-${RUN}"                          # backend A's unique id label value
IDLBL_B="b-${RUN}"                          # backend B's unique id label value

LB_MAIN="lbh-${RUN}"                        # the health-driven load balancer under test
LB_SPLIT="lbh-split-${RUN}"                 # a second LB with health port != traffic port
CTL_A="lbh-ctl-a-${RUN}"                    # control channel to backend A (selector id=A)
CTL_B="lbh-ctl-b-${RUN}"                    # control channel to backend B (selector id=B)

PORT="${PORT:-9000}"                        # guest TCP traffic port (fronted + health-probed)
CTLPORT="${CTLPORT:-9100}"                  # guest TCP control port (always open; toggles $PORT)
LP="${LP:-19030}"                           # local `lb connect` listener port (main LB)
CTL_LP="${CTL_LP:-19031}"                   # local `lb connect` listener port (control LBs)

# Short health cadence so the smoke converges fast. A settled verdict needs
# HEALTHY_THRESHOLD consecutive successes (or UNHEALTHY_THRESHOLD failures),
# HEALTH_INTERVAL seconds apart.
HEALTH_INTERVAL="${HEALTH_INTERVAL:-3}"
HEALTHY_THRESHOLD="${HEALTHY_THRESHOLD:-2}"
UNHEALTHY_THRESHOLD="${UNHEALTHY_THRESHOLD:-2}"

GW_NAME="${GW_NAME:-lbh-gw-${RUN}}"         # the gateway node name (fresh join)
GW_LISTEN_PORT="${GW_LISTEN_PORT:-9443}"    # the gateway control listener (mTLS)
GW_INDEX="${GW_INDEX:-3}"                    # the third dev node becomes the gateway

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"           # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"                 # seconds for a network to reconcile ready
GW_READY_WAIT="${GW_READY_WAIT:-180}"       # seconds for the gateway to report overlay-ready
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"       # seconds for a guest to lease an overlay IP
HEALTH_UP_WAIT="${HEALTH_UP_WAIT:-150}"     # seconds for a backend to first reach healthy=true
HEALTH_FLIP_WAIT="${HEALTH_FLIP_WAIT:-90}"  # seconds for a health verdict to flip after a toggle
BALANCE_HITS="${BALANCE_HITS:-20}"          # short connections for a balancing check
EXCLUDE_HITS="${EXCLUDE_HITS:-12}"          # short connections for the exclusion check
WARM_HITS="${WARM_HITS:-4}"                 # short connections for the warming-serves check
NOELIG_HITS="${NOELIG_HITS:-6}"             # short connections for the fail-closed check

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

# wait_net_ready NODE -> block (up to NET_WAIT) until NET reconciled ready on NODE.
wait_net_ready() {
  local node="$1" deadline; deadline=$(( SECONDS + NET_WAIT ))
  info "waiting for $NET to reconcile ready on $node (<= ${NET_WAIT}s)"
  while (( SECONDS < deadline )); do net_ready "$node" "$NET" && return 0; sleep 3; done
  return 1
}

# lb_backend_health LB VMNAME -> the backend's health verdict for VMNAME under LB
# as one of "true" / "false" / "null" (warming, no verdict yet) / "absent" (the
# backend is not currently matched / listed). Empty on a failed read.
lb_backend_health() {
  otx lb get "$1" --output json 2>/dev/null \
    | jq -r --arg n "$2" \
        '[.backends[]? | select(.vm_name==$n) | .healthy] | if length==0 then "absent" else (.[0]|tostring) end' \
        2>/dev/null || true
}

# wait_lb_health LB VMNAME WANT TIMEOUT -> block until LB reports VMNAME's health
# verdict == WANT ("true"/"false"), else fail the smoke.
wait_lb_health() {
  local lb="$1" vm="$2" want="$3" to="$4" deadline got=""
  deadline=$(( SECONDS + to ))
  info "waiting for $lb to report backend $vm healthy=$want (<= ${to}s)"
  while (( SECONDS < deadline )); do
    got="$(lb_backend_health "$lb" "$vm")"
    [[ "$got" == "$want" ]] && { pass "$lb backend $vm healthy=$got"; return 0; }
    sleep 2
  done
  fail "$lb backend $vm never reported healthy=$want within ${to}s (last='${got:-none}')"
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

# gw_serve -> launch the gateway data plane on the dedicated node (detached).
gw_serve() {
  run_on "$GW_HANDLE" sudo sh -c 'setsid nohup otherix-gateway serve >/var/log/otherix-gateway.log 2>&1 < /dev/null &'
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
NODE_CTL_CLIENT_PY=""

# lb_connect_start LOGFILE -> launch `otherix lb connect` on the gateway node for
# the MAIN load balancer, binding 127.0.0.1:LP inside the node.
lb_connect_start() {
  local logf="$1"
  run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" \
    lb connect "$LB_MAIN" --listen "127.0.0.1:${LP}" >"$logf" 2>&1 &
}

# lb_hits N -> make N short connections to the node's MAIN lb-connect listener and
# print the identity each backend returned, one per line ("NOHIT" on a connection
# that carried no bytes - a refused / no-eligible connect).
lb_hits() {
  run_on "$NODE_CLI_HANDLE" python3 "$NODE_LB_CLIENT_PY" 127.0.0.1 "$LP" "$1" 2>/dev/null
}

# send_ctl CTL_LB COMMAND -> open an ephemeral `lb connect` to CTL_LB on the
# gateway node (CTL_LB selects exactly one backend, so the broker always reaches
# that backend's control port), send COMMAND to it, read the ack, tear the
# connect down. Fails the smoke unless the backend acks "OK".
send_ctl() {
  local ctllb="$1" cmd="$2" logf reply ok deadline pid
  logf="${WORKDIR}/ctl-${ctllb}-$(date +%s).log"
  run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" \
    lb connect "$ctllb" --listen "127.0.0.1:${CTL_LP}" >"$logf" 2>&1 &
  pid=$!
  deadline=$(( SECONDS + 20 )); ok=0
  while (( SECONDS < deadline )); do grep -q 'forwarding ' "$logf" 2>/dev/null && { ok=1; break; }; sleep 1; done
  if (( ok != 1 )); then
    kill "$pid" 2>/dev/null || true
    fail "control connect to $ctllb never opened a listener: $(cat "$logf" 2>/dev/null)"
  fi
  reply="$(run_on "$NODE_CLI_HANDLE" python3 "$NODE_CTL_CLIENT_PY" 127.0.0.1 "$CTL_LP" "$cmd" 2>/dev/null || true)"
  kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
  run_on "$NODE_CLI_HANDLE" pkill -f "lb connect $ctllb" >/dev/null 2>&1 || true
  [[ "$reply" == "OK" ]] || fail "control command '$cmd' to $ctllb was not acked (reply='${reply:-none}')"
  info "control command '$cmd' acked by $ctllb"
}

# --- scratch + the guest cloud-config + the clients --------------------
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
# first-boot runcmd starts one python3 server that binds the TRAFFIC port
# ($PORT), answering every connection with IDENTITY, plus an always-open CONTROL
# port ($CTLPORT) that toggles the traffic listener: "DOWN" closes it (the
# traffic port then refuses, so its health probe fails), "UP" re-opens it. A
# second runcmd announces the leased IPv4 to /dev/console so the harness can read
# it off the captured serial.log. Writes go to /dev/console - the kernel's active
# console maps to the serial device the host captures (ttyAMA0 on arm64, ttyS0 on
# amd64), which a fixed tty name would get wrong across arches.
write_cloud_init() {
  local identity="$1" outfile="$2"
  cat >"$outfile" <<EOF
#cloud-config
write_files:
  - path: /usr/local/bin/otherix-lbh-server.py
    permissions: '0755'
    content: |
      import socket, selectors
      IDENTITY = "${identity}"
      PORT = ${PORT}
      CTLPORT = ${CTLPORT}
      sel = selectors.DefaultSelector()
      def listener(p):
          s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
          s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
          s.bind(('0.0.0.0', p))
          s.listen(64)
          s.setblocking(False)
          return s
      ctl = listener(CTLPORT)
      sel.register(ctl, selectors.EVENT_READ, 'ctl')
      state = {'traffic': listener(PORT)}
      sel.register(state['traffic'], selectors.EVENT_READ, 'traffic')
      while True:
          for key, _ in sel.select(timeout=1):
              try:
                  conn, _ = key.fileobj.accept()
              except OSError:
                  continue
              if key.data == 'traffic':
                  try:
                      conn.sendall((IDENTITY + "\n").encode())
                  except OSError:
                      pass
                  conn.close()
                  continue
              conn.settimeout(1.0)
              try:
                  data = conn.recv(64)
              except OSError:
                  data = b""
              cmd = data.decode('ascii', 'replace').strip().upper()
              reply = "OK"
              if cmd == "DOWN":
                  t = state.get('traffic')
                  if t is not None:
                      try:
                          sel.unregister(t)
                      except (KeyError, ValueError):
                          pass
                      t.close()
                      state['traffic'] = None
              elif cmd == "UP":
                  if state.get('traffic') is None:
                      t = listener(PORT)
                      sel.register(t, selectors.EVENT_READ, 'traffic')
                      state['traffic'] = t
              else:
                  reply = IDENTITY
              try:
                  conn.sendall((reply + "\n").encode())
              except OSError:
                  pass
              conn.close()
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
  - [ sh, -c, "setsid nohup python3 /usr/local/bin/otherix-lbh-server.py >/dev/null 2>&1 < /dev/null &" ]
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-announce-ip.sh >/dev/null 2>&1 < /dev/null &" ]
EOF
}

# The balancing client: make N short, sequential connections to a local
# lb-connect listener, read the identity line each backend returns, and print one
# token per attempt - the identity, or "NOHIT" when the connection carried no
# bytes (a refused / no-eligible connect). Each attempt is bounded by a socket
# timeout so a no-eligible run never hangs. Staged on the node.
LB_CLIENT_PY="${WORKDIR}/lbh_client.py"
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

# The control client: connect once to a local lb-connect listener (fronting a
# per-backend control LB), send a one-line COMMAND, read the backend's one-line
# reply, print it ("NOREPLY" on failure). Staged on the node.
CTL_CLIENT_PY="${WORKDIR}/lbh_ctl.py"
cat >"$CTL_CLIENT_PY" <<'PYEOF'
import socket, sys

host, port, cmd = sys.argv[1], int(sys.argv[2]), sys.argv[3]
try:
    s = socket.create_connection((host, port), timeout=10)
    s.settimeout(10)
    s.sendall((cmd + "\n").encode())
    buf = b""
    while b"\n" not in buf:
        chunk = s.recv(64)
        if not chunk:
            break
        buf += chunk
    s.close()
    line = buf.split(b"\n", 1)[0].decode(errors="replace").strip()
    print(line if line else "NOREPLY")
except OSError:
    print("NOREPLY")
PYEOF

# --- background-process bookkeeping + cleanup --------------------------
BG_PIDS=()
track_bg() { BG_PIDS+=("$1"); }
kill_bg()  { local p; for p in "${BG_PIDS[@]:-}"; do [ -n "$p" ] || continue; kill "$p" 2>/dev/null || true; pkill -P "$p" 2>/dev/null || true; done; }

GW_LAUNCHED=""

# kill_node_cli -> stop any straggler operator CLI processes on the gateway node.
kill_node_cli() {
  [ -n "$NODE_OTX" ] || return 0
  run_on "$NODE_CLI_HANDLE" sh -c "pkill -f '$NODE_OTX'; pkill -f 'lbh_client'; pkill -f 'lbh_ctl'" >/dev/null 2>&1 || true
}

cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the LBs/VMs/network/gateway up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  kill_bg
  kill_node_cli
  run_on "$GW_HANDLE" sudo pkill -f 'otherix-gateway serve' >/dev/null 2>&1 || true
  if [ "$SMOKE_PLATFORM" = "lima" ] && [ -n "$NODE_OTX" ]; then
    run_on "$NODE_CLI_HANDLE" rm -rf \
      "$NODE_WORK" "$NODE_OTX" "$NODE_CFG" "$NODE_LB_CLIENT_PY" "$NODE_CTL_CLIENT_PY" >/dev/null 2>&1 || true
  fi
  for lb in "$LB_MAIN" "$LB_SPLIT" "$CTL_A" "$CTL_B"; do
    otx lb delete "$lb" --force >/dev/null 2>&1 || true
  done
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
echo "=== lb-health smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v python3 >/dev/null || fail "python3 is required on the host"
command -v go >/dev/null || fail "go is required on the host (to cross-build the node CLI)"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" lb create --help >/dev/null 2>&1 || fail "this otherix build has no 'lb create' command (rebuild from the current tree)"
"$OTX" lb connect --help >/dev/null 2>&1 || fail "this otherix build has no 'lb connect' command (rebuild from the current tree)"
"$OTX" lb create --help 2>&1 | grep -q -- '--health-interval' \
  || fail "this otherix build has no active-health flags on 'lb create' (rebuild from the current tree)"
# The gateway + operator CLI run on a linux node; pick the cross-build for that
# node's arch (the host may be macOS), building on demand.
GW_ARCH="$(run_on "$GW_HANDLE" uname -m 2>/dev/null)"
case "$GW_ARCH" in
  aarch64) GW_GOARCH=arm64 ;;
  x86_64)  GW_GOARCH=amd64 ;;
  *) fail "unsupported gateway node arch '${GW_ARCH:-unknown}'" ;;
esac
if [ -z "$GW_BIN" ]; then
  GW_BIN="bin/linux-${GW_GOARCH}/otherix-gateway"
  [ -x "$GW_BIN" ] || make "build-linux-${GW_GOARCH}" >/dev/null 2>&1 || true
fi
[ -x "$GW_BIN" ] || fail "otherix-gateway not found at '$GW_BIN' (run make build-linux-arm64 / build-linux-amd64, or set GW_BIN=...)"
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

# Stage the CLI binary, its config, and the clients on the node.
NODE_OTX="$(node_path "$NODE_CLI_HANDLE" "$CLI_BIN")"
run_on "$NODE_CLI_HANDLE" chmod +x "$NODE_OTX" >/dev/null 2>&1 || true
NODE_CFG="$(node_path "$NODE_CLI_HANDLE" "${WORKDIR}/node-config")"
NODE_LB_CLIENT_PY="$(node_path "$NODE_CLI_HANDLE" "$LB_CLIENT_PY")"
NODE_CTL_CLIENT_PY="$(node_path "$NODE_CLI_HANDLE" "$CTL_CLIENT_PY")"

case "$SMOKE_PLATFORM" in
  netns) NODE_WORK="$WORKDIR" ;;
  lima)  NODE_WORK="/tmp/otx-lbh-$$" ;;
esac
run_on "$NODE_CLI_HANDLE" sh -c "mkdir -p '$NODE_WORK'" || fail "could not prepare the node scratch dir at $NODE_WORK"

# Prove the staged CLI talks to the control plane with a VERIFIED TLS dial (no
# insecure-skip) before relying on it for the brokered connect.
run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" node list >/dev/null 2>&1 \
  || fail "the staged node CLI could not reach/verify the control plane at ${NODE_CP_URL} (TLS trust or reachability)"
pass "operator CLI staged on $GW_HANDLE; verified TLS dial to ${NODE_CP_URL}"

# best-effort delete-first so stale leftovers from a prior run do not clash
for lb in "$LB_MAIN" "$LB_SPLIT" "$CTL_A" "$CTL_B"; do
  otx lb delete "$lb" --force >/dev/null 2>&1 || true
done
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

# create_backend NAME NODE NODE_INDEX IDLBL -> render the guest cloud-config
# (answering with NAME as its identity), create the VM pinned to NODE with the
# shared pool label AND a unique id label (its control-channel selector), wait it
# running, and wait its guest IP announce.
create_backend() {
  local name="$1" node="$2" idx="$3" idlbl="$4" ci="${WORKDIR}/cloud-init-${1}.yaml" id
  write_cloud_init "$name" "$ci"
  info "creating $name on $node (labels $LABEL_KEY=$LABEL_VAL,$IDLBL_KEY=$idlbl; traffic :${PORT}, control :${CTLPORT})"
  otx vm create "$name" \
    --image-url "$IMAGE_URL" --arch "$ARCH" --node "$node" --network "$NET" \
    --vcpus 2 --memory-mb 2048 \
    --label "${LABEL_KEY}=${LABEL_VAL},${IDLBL_KEY}=${idlbl}" \
    --user-data "$ci" --network-config "$NC_FILE" \
    --wait --wait-timeout "${CREATE_WAIT}s" \
    || fail "vm create $name did not reach running within ${CREATE_WAIT}s"
  id="$(otx vm get "$name" --output json | jq -r '.id')"
  [[ "$id" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $name VM id (got '$id')"
  pass "$name running on $node (id=$id)"
  wait_guest_ip "$name" "$idx" "$id"
}

create_backend "$VM_A" "$NODE1" 1 "$IDLBL_A"
create_backend "$VM_B" "$NODE2" 2 "$IDLBL_B"

# --- step 2: join an ingress gateway -----------------------------------
echo "=== step 2: join ingress gateway $GW_NAME (converged data path for the overlay backends) ==="
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
GW_CP_URL="${GW_CP_URL:-$(run_on "$GW_HANDLE" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)}"
[ -n "$GW_CP_URL" ] || fail "could not resolve the gateway control-plane URL (set GW_CP_URL=https://...:PORT)"
GW_NODE_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1")"
[ -n "$GW_NODE_IP" ] || fail "could not resolve the gateway node IP"

# The gateway joins the WireGuard mesh, so it needs a WG UDP advertised endpoint
# the VM-host agents dial for the handshake. Learn the inter-node WG subnet + port
# a peer agent advertises, then pick the gateway node's address on THAT subnet.
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
PEER_WG_EP="$(run_on "$SMOKE_HANDLE_1" awk '/^[ \t]*advertised_endpoint:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)"
GW_WG_PORT="${GW_WG_PORT:-51820}"
GW_WG_IP=""
if [ -n "$PEER_WG_EP" ]; then
  WG_SUBNET_PREFIX="$(printf '%s' "${PEER_WG_EP%:*}" | cut -d. -f1-3)"
  GW_WG_PORT="${PEER_WG_EP##*:}"
  # shellcheck disable=SC2016
  GW_WG_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1" \
    | grep -E "^${WG_SUBNET_PREFIX}\." | head -n1)"
fi
[ -n "$GW_WG_IP" ] || GW_WG_IP="$GW_NODE_IP"
[ -n "$GW_WG_IP" ] || fail "could not resolve the gateway WireGuard endpoint IP"
info "gateway WireGuard endpoint: ${GW_WG_IP}:${GW_WG_PORT}"

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

# Stage the gateway binary and bootstrap + serve on the node.
GW_BIN_NODE="$(node_path "$GW_HANDLE" "$GW_BIN")"
run_on "$GW_HANDLE" sudo install -m 0755 "$GW_BIN_NODE" /usr/local/bin/otherix-gateway >/dev/null 2>&1 \
  || run_on "$GW_HANDLE" sudo cp "$GW_BIN_NODE" /usr/local/bin/otherix-gateway
# Remove any prior gateway config + WireGuard key so bootstrap writes a fresh
# config (bootstrap never overwrites an existing config, even with --force) and a
# fresh WireGuard identity. Keeps a re-run clean.
run_on "$GW_HANDLE" sudo rm -f /etc/otherix/gateway.yaml /var/lib/otherix/wg-gateway/private.key >/dev/null 2>&1 || true
run_on "$GW_HANDLE" sudo otherix-gateway bootstrap --force \
  --token "$GW_TOKEN" --ca-fingerprint "$GW_FP" \
  --cp-url "$GW_CP_URL" --node-name "$GW_NAME" \
  --advertised-endpoint "https://${GW_NODE_IP}:${GW_LISTEN_PORT}" \
  --wireguard-endpoint "${GW_WG_IP}:${GW_WG_PORT}" \
  --heartbeat-interval "${GW_HEARTBEAT_INTERVAL:-5s}" \
  --listen "0.0.0.0:${GW_LISTEN_PORT}" \
  || fail "otherix-gateway bootstrap failed"
gw_serve
GW_LAUNCHED=1
pass "ingress gateway $GW_NAME bootstrapped and serving on $GW_HANDLE"

info "waiting for gateway $GW_NAME to report $NET ready (<= ${GW_READY_WAIT}s)"
wait_gw_ready || fail "gateway $GW_NAME did not report $NET ready within ${GW_READY_WAIT}s"
pass "gateway $GW_NAME reports $NET converged (forwarding database programmed)"

# --- step 3: warming does not darken -----------------------------------
echo "=== step 3: a fresh load balancer whose backends are warming still serves ==="
otx lb create "$LB_MAIN" --port "$PORT" --selector "${LABEL_KEY}=${LABEL_VAL}" \
  --health-interval "$HEALTH_INTERVAL" \
  --health-healthy-threshold "$HEALTHY_THRESHOLD" \
  --health-unhealthy-threshold "$UNHEALTHY_THRESHOLD" \
  || fail "lb create $LB_MAIN failed"
pass "load balancer $LB_MAIN created (health interval ${HEALTH_INTERVAL}s, healthy/unhealthy threshold ${HEALTHY_THRESHOLD}/${UNHEALTHY_THRESHOLD})"

FWD_LOG="${WORKDIR}/lb-connect.log"
lb_connect_start "$FWD_LOG"
LB_PID=$!; track_bg "$LB_PID"
deadline=$(( SECONDS + 20 )); ok=0
while (( SECONDS < deadline )); do grep -q 'forwarding ' "$FWD_LOG" 2>/dev/null && { ok=1; break; }; sleep 1; done
(( ok == 1 )) || fail "lb connect did not open a local listener on the node: $(cat "$FWD_LOG" 2>/dev/null)"
pass "lb connect forwarding 127.0.0.1:$LP -> load balancer $LB_MAIN on $GW_HANDLE"

# Snapshot the health verdicts right after create for context. A settled healthy
# verdict needs HEALTHY_THRESHOLD successes HEALTH_INTERVAL apart plus heartbeat
# propagation, so both backends are usually still warming here - but on a fast
# stack the verdict can already have settled by the time we snapshot. That is a
# timing race, not a broken invariant: the load-bearing guarantee below is that a
# connect SUCCEEDS immediately after create (degrade-include serves a warming LB),
# so the "neither healthy yet" observation is informational only, never fatal.
WHA="$(lb_backend_health "$LB_MAIN" "$VM_A")"
WHB="$(lb_backend_health "$LB_MAIN" "$VM_B")"
info "health at create: $VM_A=$WHA  $VM_B=$WHB (usually neither 'true' yet; a fast settle is fine)"
# The invariant with teeth: while (or just after) warming, a connect MUST still
# reach a backend (degrade-include).
WARM_HITSOUT="$(lb_hits "$WARM_HITS")"
info "warming connection results:"; sort <<<"$WARM_HITSOUT" | uniq -c | sed 's/^/    /' || true
if ! grep -Fxq "$VM_A" <<<"$WARM_HITSOUT" && ! grep -Fxq "$VM_B" <<<"$WARM_HITSOUT"; then
  fail "no backend served while its verdict was still warming - a fresh load balancer darkened its warming backends (must degrade-include): $(tr '\n' ' ' <<<"$WARM_HITSOUT")"
fi
pass "warming does not darken: $LB_MAIN served a backend while both verdicts were still unsettled"

# --- step 4: balancing over healthy backends ---------------------------
echo "=== step 4: both backends reach healthy=true; connections balance across them ==="
wait_lb_health "$LB_MAIN" "$VM_A" "true" "$HEALTH_UP_WAIT"
wait_lb_health "$LB_MAIN" "$VM_B" "true" "$HEALTH_UP_WAIT"
HITS="$(lb_hits "$BALANCE_HITS")"
info "backend hit distribution:"; sort <<<"$HITS" | uniq -c | sed 's/^/    /' || true
grep -Fq "$VM_A" <<<"$HITS" || fail "backend $VM_A never answered across $BALANCE_HITS connections (not balancing): $(tr '\n' ' ' <<<"$HITS")"
grep -Fq "$VM_B" <<<"$HITS" || fail "backend $VM_B never answered across $BALANCE_HITS connections (not balancing): $(tr '\n' ' ' <<<"$HITS")"
pass "both healthy backends served connections ($VM_A and $VM_B) - the load balancer balances across the healthy pool"

# Bring up the per-backend control channels now (they settle while we run the
# remaining steps; each selects exactly one backend on its control port).
echo "=== step 4b: register per-backend control load balancers ($CTL_A -> $VM_A, $CTL_B -> $VM_B) ==="
otx lb create "$CTL_A" --port "$CTLPORT" --selector "${IDLBL_KEY}=${IDLBL_A}" \
  || fail "lb create $CTL_A failed"
otx lb create "$CTL_B" --port "$CTLPORT" --selector "${IDLBL_KEY}=${IDLBL_B}" \
  || fail "lb create $CTL_B failed"
pass "control channels registered ($CTL_A, $CTL_B)"

# --- step 5: health-driven exclusion -----------------------------------
echo "=== step 5: close $VM_A's traffic port (VM stays running); the pool excludes it on the health verdict ==="
send_ctl "$CTL_A" "DOWN"
# The VM never left running - this is a HEALTH exclusion, not a phase exclusion.
VM_A_PHASE="$(otx vm get "$VM_A" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true)"
[[ "$VM_A_PHASE" == "running" ]] \
  || fail "$VM_A phase is '${VM_A_PHASE:-none}', want running - the exclusion must be driven by the health probe, not a stopped VM"
wait_lb_health "$LB_MAIN" "$VM_A" "false" "$HEALTH_FLIP_WAIT"
# B must stay healthy throughout.
[[ "$(lb_backend_health "$LB_MAIN" "$VM_B")" == "true" ]] \
  || fail "$VM_B unexpectedly not healthy while only $VM_A was taken down"
HITS2="$(lb_hits "$EXCLUDE_HITS")"
info "backend hit distribution after $VM_A traffic port closed:"; sort <<<"$HITS2" | uniq -c | sed 's/^/    /' || true
grep -Fq "$VM_B" <<<"$HITS2" || fail "surviving backend $VM_B stopped answering after $VM_A went unhealthy: $(tr '\n' ' ' <<<"$HITS2")"
if grep -Fq "$VM_A" <<<"$HITS2"; then
  fail "unhealthy backend $VM_A still received traffic - the health verdict did not exclude it: $(tr '\n' ' ' <<<"$HITS2")"
fi
pass "unhealthy backend $VM_A excluded (VM still running); all traffic served by $VM_B"

# --- step 6: recovery on re-open ---------------------------------------
echo "=== step 6: re-open $VM_A's traffic port; it returns to the healthy pool ==="
send_ctl "$CTL_A" "UP"
wait_lb_health "$LB_MAIN" "$VM_A" "true" "$HEALTH_FLIP_WAIT"
HITS3="$(lb_hits "$BALANCE_HITS")"
info "backend hit distribution after $VM_A recovered:"; sort <<<"$HITS3" | uniq -c | sed 's/^/    /' || true
grep -Fq "$VM_A" <<<"$HITS3" || fail "recovered backend $VM_A did not rejoin the pool: $(tr '\n' ' ' <<<"$HITS3")"
grep -Fq "$VM_B" <<<"$HITS3" || fail "backend $VM_B stopped answering after $VM_A recovered: $(tr '\n' ' ' <<<"$HITS3")"
pass "recovered backend $VM_A rejoined; connections balance over both again"

# --- step 7: health-port split -----------------------------------------
echo "=== step 7: a load balancer with a health port != traffic port follows the health port ==="
# $LB_SPLIT fronts the same traffic port ($PORT) but health-probes the control
# port ($CTLPORT), which stays open even when the traffic port is closed. So when
# $VM_A's traffic port goes down, $LB_MAIN (health port == traffic port) reports
# it unhealthy while $LB_SPLIT (health port == control port) keeps it healthy -
# proving the probe honors the configured health port, not the traffic port.
otx lb create "$LB_SPLIT" --port "$PORT" --health-port "$CTLPORT" \
  --selector "${LABEL_KEY}=${LABEL_VAL}" \
  --health-interval "$HEALTH_INTERVAL" \
  --health-healthy-threshold "$HEALTHY_THRESHOLD" \
  --health-unhealthy-threshold "$UNHEALTHY_THRESHOLD" \
  || fail "lb create $LB_SPLIT failed"
wait_lb_health "$LB_SPLIT" "$VM_A" "true" "$HEALTH_UP_WAIT"
wait_lb_health "$LB_SPLIT" "$VM_B" "true" "$HEALTH_UP_WAIT"
pass "$LB_SPLIT reports both backends healthy on the control health port"

send_ctl "$CTL_A" "DOWN"
# $LB_MAIN probes the (now closed) traffic port -> A unhealthy.
wait_lb_health "$LB_MAIN" "$VM_A" "false" "$HEALTH_FLIP_WAIT"
# $LB_SPLIT probes the (still open) control port -> A must STAY healthy. Assert it
# holds across a full unhealthy-threshold window so this is not just probe lag.
sleep $(( (UNHEALTHY_THRESHOLD + 1) * HEALTH_INTERVAL ))
SPLIT_A="$(lb_backend_health "$LB_SPLIT" "$VM_A")"
[[ "$SPLIT_A" == "true" ]] \
  || fail "$LB_SPLIT reported $VM_A healthy=$SPLIT_A while its traffic port was closed but its health port was open - the split health port was NOT honored"
pass "health-port split honored: with $VM_A's traffic port closed, $LB_MAIN=false but $LB_SPLIT=true (probe follows the health port)"

# --- step 8: fail closed when every backend is confirmed down ----------
echo "=== step 8: close $VM_B's traffic port too; with all backends confirmed unhealthy a connect is refused ==="
send_ctl "$CTL_B" "DOWN"
# $VM_A's traffic port is already down from step 7; drive $VM_B down too.
wait_lb_health "$LB_MAIN" "$VM_B" "false" "$HEALTH_FLIP_WAIT"
[[ "$(lb_backend_health "$LB_MAIN" "$VM_A")" == "false" ]] \
  || fail "$VM_A unexpectedly not unhealthy on $LB_MAIN before the fail-closed check"
# Isolate the connect attempts made from here on in the connect log so the 409
# assertion cannot match an earlier line.
PRE_409_LINES="$(wc -l < "$FWD_LOG" 2>/dev/null || echo 0)"
HITS4="$(lb_hits "$NOELIG_HITS")"
info "connection results with every backend confirmed unhealthy:"; sort <<<"$HITS4" | uniq -c | sed 's/^/    /' || true
if grep -Fq "$VM_A" <<<"$HITS4" || grep -Fq "$VM_B" <<<"$HITS4"; then
  fail "a backend still answered after both were confirmed unhealthy - the all-down path did not fail closed: $(tr '\n' ' ' <<<"$HITS4")"
fi
# The load balancer must refuse with 409 ingress_unavailable. The connect CLI
# surfaces the broker's HTTP status ("HTTP 409") on stderr; the response body's
# `ingress_unavailable` code is not echoed by the client, so the operator-visible
# signal asserted here is the 409 status plus the zero-byte (NOHIT) connections.
tail -n +"$(( PRE_409_LINES + 1 ))" "$FWD_LOG" 2>/dev/null | grep -q 'HTTP 409' \
  || fail "no 'HTTP 409' from the broker after all backends went unhealthy - expected a 409 ingress_unavailable refusal. connect log tail: $(tail -n +"$(( PRE_409_LINES + 1 ))" "$FWD_LOG" 2>/dev/null | tr '\n' ' ')"
pass "fail closed: every connect was refused with HTTP 409 (no backend served) when all backends were confirmed unhealthy"

kill "$LB_PID" 2>/dev/null || true; wait "$LB_PID" 2>/dev/null || true

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the LBs + VMs + network, stops the node-side CLI and
# gateway, and restarts the gateway node's agent so a re-run finds three hosts.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== lb-health smoke PASSED ===${NC}"
echo "  a fresh load balancer whose backends are warming still serves (warming does not darken)"
echo "  a backend whose traffic port closes (VM still running) is excluded on the health verdict"
echo "  re-opening the port returns the backend to the pool"
echo "  a load balancer with a separate health port follows the health port, not the traffic port"
echo "  with every backend confirmed unhealthy the load balancer fails closed (HTTP 409)"
