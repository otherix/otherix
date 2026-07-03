#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Co-located hypervisor + ingress-gateway smoke - proves that a node running the
# FULL hypervisor agent can ALSO serve ingress on the same host, and that the
# operator can move the gateway role between nodes without cutting a live
# session. Everything runs through the real operator CLI (`otherix`), exactly as
# an operator would.
#
# What it proves, end to end (operator CLI only):
#   - co-location config: a normal hypervisor node gains a gateway ingress block
#     in its agent.yaml (a separate ingress listener + advertised endpoint), with
#     enabled=false and the SHARED WireGuard key - so `otherix-agent serve` keeps
#     the full hypervisor runtime and grafts the ingress plane onto it. The node
#     self-reports its ingress endpoint through the heartbeat even before the role
#     is turned on (the plane is a hypervisor capability, decoupled from the role);
#   - role toggle: `otherix node gateway enable <node>` assigns the ingress role.
#     `otherix node get` then reports the gateway role AND the ingress endpoint,
#     while the node still reports its hypervisor hardware inventory (qemu_version,
#     cpu) - the same agent process is both a hypervisor and a gateway;
#   - still schedulable: with the gateway role on and every other hypervisor
#     cordoned, the scheduler still places a NEW VM on the co-located node - owning
#     a storage pool keeps it VM-eligible even while it carries the gateway role;
#   - reach-through: a guest is created on a DIFFERENT node on an overlay, and
#     `otherix forward` brokers a connection that reaches the guest echo server
#     THROUGH the co-located node's ingress veth data path (the co-located node
#     fronts ingress for a guest it does not host);
#   - multi-overlay: a second overlay with its own guest is reached through the
#     SAME co-located node independently, proving the co-located ingress plane
#     serves more than one overlay at once;
#   - session-safe role move: with a live brokered session held open through the
#     co-located node, the operator enables the role on a second node and disables
#     it on the first. The held session is NOT cut (the drain keeps the membership
#     while a session is live), while a fresh brokerage moves to the second node
#     (the disabled node is no longer selectable). Capacity moved without dropping
#     a live connection;
#   - migration-safe fronting: with a live brokered session held through the
#     co-located gateway, the fronted guest is live-migrated between the two
#     hypervisor hosts TWICE (including onto the co-located host) and the session
#     keeps emitting - ingress follows the guest across repeated cutovers.
#
# GATEWAY / VM-HOST PLACEMENT (dev stack):
#   The three-node dev stack runs a full hypervisor agent on every node. This
#   smoke keeps node-1 as the VM host, and turns node-3 (then node-2) into a
#   CO-LOCATED gateway by hand-editing its agent.yaml - the agent stays a
#   hypervisor; it just also serves ingress. No agent is stopped or swapped for a
#   gateway-only binary (that is the standalone-gateway smoke). CO_INDEX /
#   CO_B_INDEX / HV_INDEX are overrideable for a larger stack.
#
# WHERE THE BROKERED CLI RUNS:
#   The brokered data-plane flow (`otherix forward`) runs INSIDE the co-located
#   node, not on the host. In this dev stack the host can reach the control plane
#   but NOT a gateway's inter-node ingress endpoint (no route to the inter-VM
#   subnet), so a host-run forward would broker fine and then fail to dial the
#   gateway. The co-located node reaches both: the control plane on its public API
#   host and every gateway's ingress endpoint (all nodes share the inter-VM
#   subnet). The smoke cross-builds the operator CLI for the node's arch, stages
#   it on the node with a config that pins the public API URL + the cluster CA,
#   and runs every brokered flow there. Override NODE_CP_URL only for a custom
#   topology.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make local-dev-deploy && make local-dev-cleanrestart
# plus jq, python3, go, and curl on the host, and python3 on the co-located node
# (provisioned in the Lima VM image / present in the netns).
#
# Usage: make smoke-ingress-gateway-colocated
#        (or: bash dev/smoke/ingress-gateway-colocated/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
# Cross-build of the operator CLI for the co-located node's arch, staged on the
# node to run the brokered data-plane flow there. Resolved in preconditions,
# overridable via CLI_BIN.
CLI_BIN="${CLI_BIN:-}"

HV_INDEX="${HV_INDEX:-1}"              # the VM host (a DIFFERENT node from the gateways)
CO_INDEX="${CO_INDEX:-3}"              # co-located gateway A
CO_B_INDEX="${CO_B_INDEX:-2}"         # co-located gateway B (role-move target)
HV_NODE="${HV_NODE:-node-${HV_INDEX}}"
CO_NODE="${CO_NODE:-node-${CO_INDEX}}"
CO_B_NODE="${CO_B_NODE:-node-${CO_B_INDEX}}"

NET_A="colo-ovl-a"                     # first dhcp overlay
SUBNET_A="10.95.0.0/24"                # unlikely to clash with the dev stack
VM_A="colo-vm-a"                       # guest on NET_A (reached through the gateway)
NET_B="colo-ovl-b"                     # second dhcp overlay (multi-overlay proof)
SUBNET_B="10.96.0.0/24"
VM_B="colo-vm-b"                       # guest on NET_B

ECHO_PORT="${ECHO_PORT:-9000}"         # the guest echo server port the gateway dials
GW_INGRESS_PORT="${GW_INGRESS_PORT:-9444}" # ingress listener clients dial for /v1/connect (distinct from server.listen 9443)

FWD_PORT="${FWD_PORT:-19010}"          # node-side local listener for `otherix forward`

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"      # seconds for vm create -> running (incl. cold image fetch)
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"    # seconds for a live migrate cutover (disk copy on TCG is slow)
NET_WAIT="${NET_WAIT:-180}"            # seconds for a network to reconcile ready
GW_READY_WAIT="${GW_READY_WAIT:-180}"  # seconds for a gateway to report an overlay ready
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"  # seconds for the guest to lease an overlay IP and announce it
NODE_READY_WAIT="${NODE_READY_WAIT:-120}" # seconds for a node to return to ready after an agent restart
MOVE_HOLD="${MOVE_HOLD:-40}"           # seconds a held session runs across the role move
TICK="${TICK:-0.2}"                    # seconds between continuous-traffic lines
GAP_THRESHOLD="${GAP_THRESHOLD:-10}"   # max tolerated seconds with no echo during the move (< heartbeat)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

HV_HANDLE="$(smoke_handle "$HV_INDEX")"
CO_HANDLE="$(smoke_handle "$CO_INDEX")"
# The brokered data-plane CLI runs on the co-located node (see "WHERE THE BROKERED
# CLI RUNS"). It reaches the control plane and every gateway's ingress endpoint.
NODE_CLI_HANDLE="$CO_HANDLE"

# net_ready NODE NET -> 0 when the network reconciled "ready" on NODE.
net_ready() {
  [[ "$(otx network get "$2" --output json 2>/dev/null \
      | jq -r --arg n "$1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
      == "ready" ]]
}

# wait_net_ready NODE NET SECONDS -> block until NET reconciled ready on NODE.
wait_net_ready() {
  local node="$1" net="$2" deadline=$(( SECONDS + $3 ))
  while (( SECONDS < deadline )); do net_ready "$node" "$net" && return 0; sleep 3; done
  return 1
}

# node_json NODE -> the node projection JSON (empty on failure).
node_json() { otx node get "$1" --output json 2>/dev/null || true; }
node_status()   { node_json "$1" | jq -r '.status // empty'; }
node_ingress()  { node_json "$1" | jq -r '.ingress_advertised_endpoint // empty'; }
node_qemu()     { node_json "$1" | jq -r '.qemu_version // empty'; }
node_roles()    { node_json "$1" | jq -r '.roles // [] | join(",")'; }
node_has_role() { node_json "$1" | jq -e --arg r "$2" '.roles // [] | index($r)' >/dev/null 2>&1; }

# vm_node VM -> the name of the node the VM currently runs on (empty if unbound).
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.node // empty'; }

# node_ip IDX -> the node's first global IPv4 (the inter-VM ingress address).
node_ip() {
  run_on "$(smoke_handle "$1")" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1"
}

# agent_yaml_path IDX -> the agent config file path on node IDX (per platform).
agent_yaml_path() {
  case "$SMOKE_PLATFORM" in
    lima)  printf '/etc/otherix/agent.yaml' ;;
    netns) printf '%s/agent.yaml' "$(smoke_state "$1")" ;;
  esac
}

# restart_agent IDX -> restart the full hypervisor agent on node IDX so it
# re-reads its (edited) agent.yaml. systemd-managed on Lima; pid-managed on netns.
restart_agent() {
  local idx="$1" handle; handle="$(smoke_handle "$idx")"
  case "$SMOKE_PLATFORM" in
    lima) run_on "$handle" sudo systemctl restart otherix-agent ;;
    netns) netns_restart_agent "$idx" ;;
  esac
}

# netns_restart_agent IDX -> stop the node's agent by its pid file and respawn it
# exactly as dev/scripts/linux-multinode.sh do_start does (netns + private mount).
netns_restart_agent() {
  local idx="$1" dir ns pidf agent_bin
  ns="$(smoke_handle "$idx")"; dir="$(smoke_state "$idx")"; pidf="${dir}/agent.pid"
  agent_bin="$(pwd)/bin/otherix-agent"
  sudo sh -c "[ -f '${pidf}' ] && kill \$(cat '${pidf}') 2>/dev/null; true"
  sleep 2
  # Respawn exactly as do_start does; the redirect + pid capture run under root
  # (sudo does not affect a redirect performed by the calling user's shell).
  sudo sh -c "nohup ip netns exec '${ns}' unshare --mount --propagation private \
    sh -c \"mount --bind '${dir}/pools' /var/lib/otherix/pools && exec '${agent_bin}' serve --config '${dir}/agent.yaml'\" \
    > '${dir}/agent.log' 2>&1 & echo \$! > '${pidf}'"
}

# colo_endpoint IDX -> the ingress endpoint clients dial for node IDX
# (https://<node-ip>:<ingress-port>). Deterministic; printed cleanly for capture.
colo_endpoint() {
  local ip; ip="$(node_ip "$1")"; [ -n "$ip" ] || return 1
  printf 'https://%s:%s' "$ip" "$GW_INGRESS_PORT"
}

# colo_configure IDX -> add (or refresh) the gateway ingress block on node IDX's
# agent.yaml, keeping the node a full hypervisor: enabled=false + a distinct
# ingress listener + the node's ingress endpoint. Keeps the SHARED WireGuard key
# and drops any leftover standalone-gateway key. Backs the original config up
# once so cleanup can restore a plain hypervisor. Restarts the agent. Prints
# nothing to stdout (the endpoint comes from colo_endpoint).
colo_configure() {
  local idx="$1" handle yaml endpoint
  handle="$(smoke_handle "$idx")"; yaml="$(agent_yaml_path "$idx")"
  endpoint="$(colo_endpoint "$idx")" || fail "could not resolve node IP for $(smoke_handle "$idx")"
  # Back the clean config up once, then restore-and-append so a re-run never
  # stacks a second gateway block.
  run_on "$handle" sudo sh -c "test -f '${yaml}.colo-bak' || cp '${yaml}' '${yaml}.colo-bak'" >/dev/null
  run_on "$handle" sudo cp "${yaml}.colo-bak" "${yaml}" >/dev/null
  printf 'gateway:\n  enabled: false\n  listen: "0.0.0.0:%s"\n  advertised_endpoint: "%s"\n' \
    "$GW_INGRESS_PORT" "$endpoint" | run_on "$handle" sudo tee -a "${yaml}" >/dev/null
  # A co-located node keeps its single shared node identity's WireGuard key; the
  # distinct standalone-gateway key must not be present or the gateway would
  # present a public key the CP still has registered to a gone standalone gateway.
  run_on "$handle" sudo rm -f /var/lib/otherix/wg-gateway/private.key >/dev/null 2>&1 || true
  restart_agent "$idx" >/dev/null
  CONFIGURED_IDX+=("$idx")
}

# colo_restore IDX -> restore the node's plain-hypervisor agent.yaml and restart.
colo_restore() {
  local idx="$1" handle yaml; handle="$(smoke_handle "$idx")"; yaml="$(agent_yaml_path "$idx")"
  run_on "$handle" sudo sh -c "test -f '${yaml}.colo-bak' && mv -f '${yaml}.colo-bak' '${yaml}'" >/dev/null 2>&1 || true
  restart_agent "$idx" >/dev/null 2>&1 || true
}

# wait_node_ready NODE -> block until NODE reports ready after an agent restart.
wait_node_ready() {
  local node="$1" deadline=$(( SECONDS + NODE_READY_WAIT ))
  while (( SECONDS < deadline )); do [[ "$(node_status "$node")" == "ready" ]] && return 0; sleep 3; done
  return 1
}

# wait_node_ingress NODE ENDPOINT -> block until NODE self-reports ENDPOINT as its
# ingress-advertised endpoint (heartbeat ingest of the co-located plane).
wait_node_ingress() {
  local node="$1" want="$2" deadline=$(( SECONDS + GW_READY_WAIT ))
  while (( SECONDS < deadline )); do [[ "$(node_ingress "$node")" == "$want" ]] && return 0; sleep 3; done
  return 1
}

# node_path HANDLE SRC -> a path to SRC usable INSIDE the node. On netns the host
# filesystem is shared, so the host path works; on Lima the file is copied in.
node_path() {
  local handle="$1" src="$2" base
  base="$(basename "$src")"
  case "$SMOKE_PLATFORM" in
    netns) printf '%s' "$src" ;;
    lima)  limactl cp "$src" "$handle:/tmp/$base" >/dev/null; printf '/tmp/%s' "$base" ;;
  esac
}

# --- node-side brokered CLI plumbing (resolved in preconditions) -------
NODE_OTX=""
NODE_CFG=""
NODE_WORK=""
NODE_PROBE_PY=""
NODE_FWD_CLIENT_PY=""

# node_forward_start VM PORT LPORT LOGFILE -> launch `otherix forward` on the node
# (backgrounded via run_on; its stdout captured to LOGFILE on the host). The
# listener binds 127.0.0.1:LPORT inside the node.
node_forward_start() {
  local vm="$1" port="$2" lport="$3" logf="$4"
  run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" \
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

# wait_forward_listener LOGFILE -> block until `otherix forward` opened a listener.
wait_forward_listener() {
  local logf="$1" deadline=$(( SECONDS + 20 ))
  while (( SECONDS < deadline )); do grep -q 'forwarding ' "$logf" 2>/dev/null && return 0; sleep 1; done
  return 1
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

# --- the guest echo server cloud-config --------------------------------
# A first-boot runcmd writes a tiny python3 line-echo server bound to the overlay
# and launches it detached. Once the NIC has an IPv4 the guest announces it on the
# serial console as "OTHERIX_GUEST_IP <ip>" (repeatedly), which the harness reads
# off the captured serial.log to learn the address the gateway must dial. python3
# ships in the minimal cloud image (cloud-init depends on it).
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
      # Write to /dev/console - the kernel's active console maps to the serial
      # device the host captures (ttyAMA0 on arm64, ttyS0 on amd64), which a
      # fixed tty name would get wrong across architectures.
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

# Continuous-traffic client: holds ONE TCP connection open to the local
# `otherix forward` listener, sends a monotonic line every TICK seconds, and
# records the arrival time of each echoed line. Writer/reader run on separate
# threads so a brief stall never deadlocks the loop. It exits when the stop file
# appears (or the hard deadline passes) and prints the largest gap between
# consecutive echoes - the seamlessness metric. Staged on the node.
FWD_CLIENT_PY="${WORKDIR}/forward_client.py"
cat >"$FWD_CLIENT_PY" <<'PYEOF'
import os, socket, sys, threading, time

host, port = sys.argv[1], int(sys.argv[2])
arrivals_path, stop_path, max_secs, tick = sys.argv[3], sys.argv[4], float(sys.argv[5]), float(sys.argv[6])

s = socket.create_connection((host, port), timeout=30)
arrivals = open(arrivals_path, "w")
stop = threading.Event()
# Set when a thread sees the connection close before we asked it to stop - a
# clean cutover-time RST would otherwise pass the gap check while having actually
# dropped the session.
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

# --- background-process bookkeeping + cleanup --------------------------
BG_PIDS=()
track_bg() { BG_PIDS+=("$1"); }
kill_bg()  { local p; for p in "${BG_PIDS[@]:-}"; do [ -n "$p" ] || continue; kill "$p" 2>/dev/null || true; pkill -P "$p" 2>/dev/null || true; done; }

CONFIGURED_IDX=()   # node indices given a co-located gateway block (to restore)
ROLE_ON_NODES=()    # node names whose gateway role we enabled (to disable)
SCHED_VM=""         # probe VM from the schedulability check (to delete)
SCHED_CORDONED=()   # nodes cordoned to force placement (to uncordon)

# kill_node_cli -> stop any straggler brokered CLI processes on the node.
kill_node_cli() {
  [ -n "$NODE_OTX" ] || return 0
  run_on "$NODE_CLI_HANDLE" sh -c "pkill -f '$NODE_OTX'; pkill -f 'forward_client'" >/dev/null 2>&1 || true
}

cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the VMs/networks/gateway config up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  kill_bg
  kill_node_cli
  # Turn the gateway role back off so a re-run (or another smoke) finds plain
  # schedulable hypervisors.
  local n
  for n in "${ROLE_ON_NODES[@]:-}"; do
    [ -n "$n" ] || continue
    otx node gateway disable "$n" >/dev/null 2>&1 || true
  done
  # Probe VM + cordoned nodes from the schedulability check (best effort).
  [ -n "$SCHED_VM" ] && otx vm delete "$SCHED_VM" --wait --force >/dev/null 2>&1 || true
  local cn
  for cn in "${SCHED_CORDONED[@]:-}"; do
    [ -n "$cn" ] || continue
    otx node uncordon "$cn" >/dev/null 2>&1 || true
  done
  otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET_A" --force >/dev/null 2>&1 || true
  otx network delete "$NET_B" --force >/dev/null 2>&1 || true
  # Node-side scratch (Lima only; on netns these alias host paths under WORKDIR).
  if [ "$SMOKE_PLATFORM" = "lima" ] && [ -n "$NODE_OTX" ]; then
    run_on "$NODE_CLI_HANDLE" rm -rf \
      "$NODE_WORK" "$NODE_OTX" "$NODE_CFG" "$NODE_PROBE_PY" "$NODE_FWD_CLIENT_PY" >/dev/null 2>&1 || true
  fi
  # Restore each co-located node to a plain hypervisor (drop the gateway block).
  local idx
  for idx in "${CONFIGURED_IDX[@]:-}"; do
    [ -n "$idx" ] || continue
    info "restore plain-hypervisor agent.yaml on $(smoke_handle "$idx") (best effort)"
    colo_restore "$idx"
  done
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== co-located ingress-gateway smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v python3 >/dev/null || fail "python3 is required on the host"
command -v go >/dev/null || fail "go is required on the host (to cross-build the node CLI)"
command -v curl >/dev/null || fail "curl is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" forward --help >/dev/null 2>&1 || fail "this otherix build has no 'forward' command (rebuild from the current tree)"
"$OTX" node gateway --help >/dev/null 2>&1 || fail "this otherix build has no 'node gateway' command (rebuild from the current tree)"

# Cross-build the operator CLI for the co-located node's arch (the host may be
# macOS, whose binary cannot run in the linux node).
CO_ARCH="$(run_on "$CO_HANDLE" uname -m 2>/dev/null)"
case "$CO_ARCH" in
  aarch64) CO_GOARCH=arm64 ;;
  x86_64)  CO_GOARCH=amd64 ;;
  *) fail "unsupported co-located node arch '${CO_ARCH:-unknown}'" ;;
esac
if [ -z "$CLI_BIN" ]; then
  CLI_BIN="bin/linux-${CO_GOARCH}/otherix-cli"
  info "cross-building the operator CLI for linux/${CO_GOARCH}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$CO_GOARCH" go build -trimpath -o "$CLI_BIN" ./cmd/cli \
    || fail "failed to cross-build the operator CLI for linux/${CO_GOARCH}"
fi
[ -x "$CLI_BIN" ] || fail "operator CLI cross-build not found at '$CLI_BIN' (set CLI_BIN=...)"

cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
for node in "$HV_NODE" "$CO_NODE" "$CO_B_NODE"; do
  st="$(node_status "$node")"
  [[ "$st" == "ready" ]] || fail "$node not ready (got '${st:-none}'); run make local-dev-start"
done
# The co-located node and the move target must START as plain hypervisors.
node_has_role "$CO_NODE" hypervisor || fail "$CO_NODE does not start as a hypervisor (roles: $(node_roles "$CO_NODE"))"
node_has_role "$CO_B_NODE" hypervisor || fail "$CO_B_NODE does not start as a hypervisor (roles: $(node_roles "$CO_B_NODE"))"
pass "CP up (${CP_VERSION}); $HV_NODE (vm host) + $CO_NODE + $CO_B_NODE ready hypervisors"

# --- provision the brokered CLI on the co-located node -----------------
echo "=== preconditions: stage the operator CLI on the co-located node ==="
# The node reaches the control plane on its public API host (the same host the
# agent dials for mTLS; the public API shares it on the public port). Derive that
# URL from a running agent's config rather than hard-coding it, using the VM host
# node (untouched by this smoke). Override NODE_CP_URL for a custom topology.
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
_agent_url="$(run_on "$HV_HANDLE" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)"
_agent_host="${_agent_url#*://}"; _agent_host="${_agent_host%%:*}"
_pub_port="${CP_BASE##*:}"; { [ -n "$_pub_port" ] && [ "$_pub_port" != "$CP_BASE" ]; } || _pub_port=8080
[ -n "$_agent_host" ] || fail "could not resolve the node-facing CP host (set NODE_CP_URL=https://host:port)"
NODE_CP_URL="${NODE_CP_URL:-https://${_agent_host}:${_pub_port}}"
info "node CLI -> control plane at ${NODE_CP_URL}"

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

# Stage the CLI binary, its config, and the data-plane client scripts on the node.
NODE_OTX="$(node_path "$NODE_CLI_HANDLE" "$CLI_BIN")"
run_on "$NODE_CLI_HANDLE" chmod +x "$NODE_OTX" >/dev/null 2>&1 || true
NODE_CFG="$(node_path "$NODE_CLI_HANDLE" "${WORKDIR}/node-config")"
NODE_PROBE_PY="$(node_path "$NODE_CLI_HANDLE" "$PROBE_PY")"
NODE_FWD_CLIENT_PY="$(node_path "$NODE_CLI_HANDLE" "$FWD_CLIENT_PY")"
case "$SMOKE_PLATFORM" in
  netns) NODE_WORK="$WORKDIR" ;;
  lima)  NODE_WORK="/tmp/otx-colo-$$" ;;
esac
run_on "$NODE_CLI_HANDLE" mkdir -p "$NODE_WORK" >/dev/null 2>&1 || true

# Prove the staged CLI talks to the control plane with a VERIFIED TLS dial.
run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" node list >/dev/null 2>&1 \
  || fail "the staged node CLI could not reach/verify the control plane at ${NODE_CP_URL}"
pass "operator CLI staged on $CO_HANDLE; verified TLS dial to ${NODE_CP_URL}"

# best-effort delete-first so stale leftovers from a prior run do not 409
otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET_A" --force >/dev/null 2>&1 || true
otx network delete "$NET_B" --force >/dev/null 2>&1 || true

# --- step 1: co-locate the ingress plane on the hypervisor -------------
echo "=== step 1: add a gateway ingress block to $CO_NODE (still a hypervisor) ==="
CO_ENDPOINT="$(colo_endpoint "$CO_INDEX")" || fail "could not resolve $CO_NODE ingress endpoint"
colo_configure "$CO_INDEX"
info "$CO_NODE ingress endpoint = $CO_ENDPOINT (shared wg key retained, enabled=false)"
wait_node_ready "$CO_NODE" || fail "$CO_NODE did not return to ready after the agent restart"
# The ingress plane is a hypervisor capability, decoupled from the role: the node
# self-reports its ingress endpoint through the heartbeat even before the role is
# turned on. The role is still off, so it is still a plain hypervisor.
wait_node_ingress "$CO_NODE" "$CO_ENDPOINT" \
  || fail "$CO_NODE never self-reported its ingress endpoint (got '$(node_ingress "$CO_NODE")')"
node_has_role "$CO_NODE" hypervisor \
  || fail "$CO_NODE unexpectedly holds the gateway role before enable (roles: $(node_roles "$CO_NODE"))"
pass "$CO_NODE serves the co-located ingress plane and self-reports $CO_ENDPOINT (role still off)"

# --- step 2: enable the gateway role -----------------------------------
echo "=== step 2: otherix node gateway enable $CO_NODE ==="
otx node gateway enable "$CO_NODE" || fail "node gateway enable $CO_NODE failed"
ROLE_ON_NODES+=("$CO_NODE")
node_has_role "$CO_NODE" gateway || fail "$CO_NODE did not gain the gateway role (roles: $(node_roles "$CO_NODE"))"
[[ "$(node_ingress "$CO_NODE")" == "$CO_ENDPOINT" ]] \
  || fail "$CO_NODE ingress endpoint changed unexpectedly (got '$(node_ingress "$CO_NODE")')"
# Co-location proof: the SAME node still reports its full hypervisor inventory
# (qemu_version), i.e. the agent kept running the hypervisor runtime while it
# also carries the gateway role. (This revision's role model reports a single
# effective role string; co-location is proven functionally below.)
CO_QEMU="$(node_qemu "$CO_NODE")"
[ -n "$CO_QEMU" ] || fail "$CO_NODE stopped reporting a hypervisor qemu_version after enable - not co-located"
pass "$CO_NODE roles=[$(node_roles "$CO_NODE")], ingress=$CO_ENDPOINT, qemu=$CO_QEMU (hypervisor + gateway on one host)"

# --- co-located node stays VM-schedulable ------------------------------
echo "=== the co-located node stays VM-schedulable ==="
# It owns a storage pool AND holds the gateway role, so its effective role set is
# both hypervisor and gateway. Owning a pool is what keeps it eligible for VM
# placement; a gateway-role node without that fix would be excluded.
SCHED_ROLES="$(node_json "$CO_NODE" | jq -r '.roles // [] | sort | join(",")')"
[ "$SCHED_ROLES" = "gateway,hypervisor" ] \
  || fail "$CO_NODE roles = '${SCHED_ROLES:-none}', want 'gateway,hypervisor'"
pass "$CO_NODE shows both roles [hypervisor,gateway]"

# Force placement onto $CO_NODE: cordon every other hypervisor so it is the only
# eligible target, let the scheduler pick (no --node hint), and assert the new VM
# lands there. Restores the cordoned nodes and deletes the probe VM afterwards.
CO_NODE_ID="$(node_json "$CO_NODE" | jq -r '.id')"
[[ "$CO_NODE_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $CO_NODE id (got '${CO_NODE_ID:-none}')"
SCHED_VM="colo-sched-$$"
SCHED_CORDONED=("$HV_NODE" "$CO_B_NODE")
for n in "${SCHED_CORDONED[@]}"; do otx node cordon "$n" || fail "cordon $n failed"; done
SCHED_LANDED=""
if otx vm create "$SCHED_VM" --image-url "$IMAGE_URL" --arch "$ARCH" \
     --vcpus 2 --memory-mb 2048 \
     --wait --wait-timeout "${CREATE_WAIT}s"; then
  SCHED_LANDED="$(vm_node "$SCHED_VM")"
fi
otx vm delete "$SCHED_VM" --wait --force --wait-timeout 300s >/dev/null 2>&1 || true
SCHED_VM=""
for n in "${SCHED_CORDONED[@]}"; do otx node uncordon "$n" >/dev/null 2>&1 || true; done
SCHED_CORDONED=()
[ -n "$SCHED_LANDED" ] \
  || fail "the co-located node did not accept a new VM (colo-sched-$$ never placed) - it is not schedulable"
{ [ "$SCHED_LANDED" = "$CO_NODE_ID" ] || [ "$SCHED_LANDED" = "$CO_NODE" ]; } \
  || fail "the probe VM landed on '$SCHED_LANDED', want $CO_NODE ($CO_NODE_ID)"
pass "a new VM placed on the co-located gateway node (schedulable)"

# --- step 3: reach a guest on a DIFFERENT node through the gateway -----
echo "=== step 3: overlay $NET_A + guest $VM_A on $HV_NODE, reached through $CO_NODE ==="
otx network create "$NET_A" --type overlay --subnet "$SUBNET_A" --dhcp || fail "network create $NET_A failed"
wait_net_ready "$HV_NODE" "$NET_A" "$NET_WAIT" || fail "$NET_A did not reconcile ready on $HV_NODE within ${NET_WAIT}s"

info "creating $VM_A on $HV_NODE (a DIFFERENT node from the gateway; echo server on :${ECHO_PORT})"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM_A" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$HV_NODE" --network "$NET_A" \
  --vcpus 2 --memory-mb 2048 \
  --user-data - --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_A did not reach running within ${CREATE_WAIT}s"
VM_A_ID="$(otx vm get "$VM_A" --output json | jq -r '.id')"
[[ "$VM_A_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM_A id (got '$VM_A_ID')"
pass "$VM_A running on $HV_NODE (id=$VM_A_ID)"

# Discover the guest overlay IP from the serial announce sentinel.
HV_STATE="$(smoke_state "$HV_INDEX")"
deadline=$(( SECONDS + GUEST_IP_WAIT )); GUEST_IP_A=""
info "waiting for $VM_A to announce its overlay IP (<= ${GUEST_IP_WAIT}s)"
while (( SECONDS < deadline )); do
  GUEST_IP_A="$(run_on "$HV_HANDLE" sudo grep -oE 'OTHERIX_GUEST_IP [0-9.]+' \
      "${HV_STATE}/vms/${VM_A_ID}/serial.log" 2>/dev/null | awk '{print $2}' | tail -n1)" || true
  [ -n "$GUEST_IP_A" ] && break
  sleep 5
done
[ -n "$GUEST_IP_A" ] || fail "$VM_A never announced an overlay IP within ${GUEST_IP_WAIT}s"
info "$VM_A overlay IP is $GUEST_IP_A"

# The CP places a gateway membership on the co-located node for the active overlay;
# wait until the co-located node reports the overlay converged (its forwarding
# database + ingress veth are programmed).
info "waiting for $CO_NODE to report $NET_A ready as a gateway (<= ${GW_READY_WAIT}s)"
wait_net_ready "$CO_NODE" "$NET_A" "$GW_READY_WAIT" \
  || fail "$CO_NODE did not report $NET_A ready within ${GW_READY_WAIT}s (gateway membership/convergence)"

FWD_LOG_A="${WORKDIR}/forward-a.log"
node_forward_start "$VM_A" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG_A"; FWD_A_PID=$!; track_bg "$FWD_A_PID"
wait_forward_listener "$FWD_LOG_A" || fail "otherix forward did not open a listener: $(cat "$FWD_LOG_A" 2>/dev/null)"
PROBE_A="$(node_probe "$FWD_PORT")"
grep -q PROBE_OK <<<"$PROBE_A" \
  || fail "reach-through probe failed: ${PROBE_A}; forward log: $(cat "$FWD_LOG_A" 2>/dev/null)"
kill "$FWD_A_PID" 2>/dev/null || true; wait "$FWD_A_PID" 2>/dev/null || true; node_forward_stop
pass "otherix forward reached $VM_A (on $HV_NODE) THROUGH the co-located gateway $CO_NODE ($PROBE_A)"

# --- step 4: multi-overlay independence --------------------------------
echo "=== step 4: second overlay $NET_B + guest $VM_B, reached through the SAME $CO_NODE ==="
otx network create "$NET_B" --type overlay --subnet "$SUBNET_B" --dhcp || fail "network create $NET_B failed"
wait_net_ready "$HV_NODE" "$NET_B" "$NET_WAIT" || fail "$NET_B did not reconcile ready on $HV_NODE within ${NET_WAIT}s"

info "creating $VM_B on $HV_NODE (second overlay $NET_B)"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM_B" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$HV_NODE" --network "$NET_B" \
  --vcpus 2 --memory-mb 2048 \
  --user-data - --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_B did not reach running within ${CREATE_WAIT}s"
VM_B_ID="$(otx vm get "$VM_B" --output json | jq -r '.id')"
[[ "$VM_B_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve $VM_B id (got '$VM_B_ID')"

deadline=$(( SECONDS + GUEST_IP_WAIT )); GUEST_IP_B=""
info "waiting for $VM_B to announce its overlay IP (<= ${GUEST_IP_WAIT}s)"
while (( SECONDS < deadline )); do
  GUEST_IP_B="$(run_on "$HV_HANDLE" sudo grep -oE 'OTHERIX_GUEST_IP [0-9.]+' \
      "${HV_STATE}/vms/${VM_B_ID}/serial.log" 2>/dev/null | awk '{print $2}' | tail -n1)" || true
  [ -n "$GUEST_IP_B" ] && break
  sleep 5
done
[ -n "$GUEST_IP_B" ] || fail "$VM_B never announced an overlay IP within ${GUEST_IP_WAIT}s"
info "$VM_B overlay IP is $GUEST_IP_B"

info "waiting for $CO_NODE to report $NET_B ready as a gateway (<= ${GW_READY_WAIT}s)"
wait_net_ready "$CO_NODE" "$NET_B" "$GW_READY_WAIT" \
  || fail "$CO_NODE did not report $NET_B ready within ${GW_READY_WAIT}s"

FWD_LOG_B="${WORKDIR}/forward-b.log"
node_forward_start "$VM_B" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG_B"; FWD_B_PID=$!; track_bg "$FWD_B_PID"
wait_forward_listener "$FWD_LOG_B" || fail "otherix forward did not open a listener for $VM_B: $(cat "$FWD_LOG_B" 2>/dev/null)"
PROBE_B="$(node_probe "$FWD_PORT")"
grep -q PROBE_OK <<<"$PROBE_B" \
  || fail "second-overlay reach-through probe failed: ${PROBE_B}; forward log: $(cat "$FWD_LOG_B" 2>/dev/null)"
kill "$FWD_B_PID" 2>/dev/null || true; wait "$FWD_B_PID" 2>/dev/null || true; node_forward_stop
pass "$CO_NODE fronts ingress for a SECOND overlay independently ($VM_B on $NET_B: $PROBE_B)"

# --- step 5: session-safe role move ------------------------------------
echo "=== step 5: move the gateway role $CO_NODE -> $CO_B_NODE without cutting a live session ==="
# Prepare the move target as a second co-located gateway (config + restart), but
# do NOT enable its role yet - the held session must broker to $CO_NODE first.
CO_B_ENDPOINT="$(colo_endpoint "$CO_B_INDEX")" || fail "could not resolve $CO_B_NODE ingress endpoint"
colo_configure "$CO_B_INDEX"
info "$CO_B_NODE ingress endpoint = $CO_B_ENDPOINT (prepared, role still off)"
wait_node_ready "$CO_B_NODE" || fail "$CO_B_NODE did not return to ready after the agent restart"
wait_node_ingress "$CO_B_NODE" "$CO_B_ENDPOINT" \
  || fail "$CO_B_NODE never self-reported its ingress endpoint (got '$(node_ingress "$CO_B_NODE")')"

# Hold ONE brokered session open through $CO_NODE (the only enabled gateway).
FWD_LOG_M="${WORKDIR}/forward-move.log"
node_forward_start "$VM_A" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG_M"; FWD_M_PID=$!; track_bg "$FWD_M_PID"
wait_forward_listener "$FWD_LOG_M" || fail "otherix forward did not open a listener for the move: $(cat "$FWD_LOG_M" 2>/dev/null)"
grep -q PROBE_OK <<<"$(node_probe "$FWD_PORT")" || fail "the move listener could not reach $VM_A before the toggle"

NODE_ARR="${NODE_WORK}/move-arrivals.log"; NODE_STOP="${NODE_WORK}/move-stop"; OUT_M="${WORKDIR}/move-client.out"
run_on "$NODE_CLI_HANDLE" rm -f "$NODE_STOP" "$NODE_ARR" >/dev/null 2>&1 || true
run_on "$NODE_CLI_HANDLE" python3 "$NODE_FWD_CLIENT_PY" 127.0.0.1 "$FWD_PORT" \
  "$NODE_ARR" "$NODE_STOP" "$(( MOVE_HOLD + 60 ))" "$TICK" >"$OUT_M" 2>&1 &
CLIENT_M_PID=$!; track_bg "$CLIENT_M_PID"
sleep 3   # let the session establish and a few echoes flow before the toggle

# Move the capacity: enable the role on B, wait until B is a converged gateway for
# the overlay, then disable the role on A.
otx node gateway enable "$CO_B_NODE" || fail "node gateway enable $CO_B_NODE failed"
ROLE_ON_NODES+=("$CO_B_NODE")
node_has_role "$CO_B_NODE" gateway || fail "$CO_B_NODE did not gain the gateway role"
info "waiting for $CO_B_NODE to report $NET_A ready as a gateway (membership placed) (<= ${GW_READY_WAIT}s)"
wait_net_ready "$CO_B_NODE" "$NET_A" "$GW_READY_WAIT" \
  || fail "$CO_B_NODE did not converge $NET_A within ${GW_READY_WAIT}s (cannot take over ingress)"

otx node gateway disable "$CO_NODE" || fail "node gateway disable $CO_NODE failed"
node_has_role "$CO_NODE" gateway && fail "$CO_NODE still holds the gateway role after disable"
# Remove it from the cleanup disable-list (already disabled).
info "$CO_NODE disabled; $CO_B_NODE now the enabled gateway"

# Let the held session run a bit longer across the drain, then stop and measure.
sleep 8
run_on "$NODE_CLI_HANDLE" touch "$NODE_STOP" >/dev/null 2>&1 || true
wait "$CLIENT_M_PID" 2>/dev/null || true
OUT_M_TXT="$(cat "$OUT_M" 2>/dev/null)"; echo "$OUT_M_TXT"
MAXGAP="$(grep -oE 'maxgap=[0-9.]+' <<<"$OUT_M_TXT" | head -n1 | cut -d= -f2)"
ECHOES="$(grep -oE 'echoes=[0-9]+' <<<"$OUT_M_TXT" | head -n1 | cut -d= -f2)"
DROPPED="$(grep -oE 'dropped=[0-9]+' <<<"$OUT_M_TXT" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP" ] && [ -n "$ECHOES" ] || fail "the held-session client did not report a metric: ${OUT_M_TXT}"
(( ECHOES > 0 )) || fail "no echoes recorded across the role move - the held session dropped"
[ "$DROPPED" = "0" ] || fail "the held session closed during the role move - it was CUT, not drained (session-unsafe)"
awk -v g="$MAXGAP" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "held session stalled ${MAXGAP}s across the move (>= ${GAP_THRESHOLD}s) - not seamless"
kill "$FWD_M_PID" 2>/dev/null || true; wait "$FWD_M_PID" 2>/dev/null || true; node_forward_stop
pass "the live session held through $CO_NODE SURVIVED the role move (echoes=$ECHOES, max gap ${MAXGAP}s < ${GAP_THRESHOLD}s)"

# New brokerage moves to $CO_B_NODE: $CO_NODE lost the role, so it is no longer
# selectable. A fresh brokered connection that still reaches $VM_A therefore went
# through $CO_B_NODE (there is no other enabled+converged gateway for $NET_A).
FWD_LOG_N="${WORKDIR}/forward-new.log"
node_forward_start "$VM_A" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG_N"; FWD_N_PID=$!; track_bg "$FWD_N_PID"
wait_forward_listener "$FWD_LOG_N" || fail "otherix forward did not open a listener for the post-move probe: $(cat "$FWD_LOG_N" 2>/dev/null)"
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do
  P_NEW="$(node_probe "$FWD_PORT")"
  grep -q PROBE_OK <<<"$P_NEW" && { ok=1; break; }
  sleep 3
done
(( ok == 1 )) || fail "a fresh brokered connection did not reach $VM_A after the move (last: ${P_NEW:-none}) - $CO_B_NODE did not take over"
kill "$FWD_N_PID" 2>/dev/null || true; wait "$FWD_N_PID" 2>/dev/null || true; node_forward_stop
pass "a fresh brokerage reached $VM_A through $CO_B_NODE after the move ($CO_NODE de-selected): $P_NEW"

# --- step 6: a live session survives two migrations of the fronted guest
echo "=== step 6: a live session survives two migrations through the co-located gateway ==="
# $VM_A runs on $HV_NODE and is fronted by the now-enabled gateway ($CO_B_NODE,
# which took over the role above). Hold ONE brokered echo session open through
# that gateway and live-migrate the guest between the two hypervisor hosts twice
# ($HV_NODE -> $CO_NODE -> $HV_NODE); the held session must keep emitting and must
# never drop. Migrating ONTO $CO_NODE also exercises the co-located host accepting
# a live guest, not just a fresh create.
FWD_LOG_MIG="${WORKDIR}/forward-mig.log"
node_forward_start "$VM_A" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG_MIG"; FWD_MIG_PID=$!; track_bg "$FWD_MIG_PID"
wait_forward_listener "$FWD_LOG_MIG" \
  || fail "otherix forward did not open a listener for the migration hold: $(cat "$FWD_LOG_MIG" 2>/dev/null)"
grep -q PROBE_OK <<<"$(node_probe "$FWD_PORT")" \
  || fail "the migration-hold listener could not reach $VM_A before the first migration"

NODE_ARR_MIG="${NODE_WORK}/mig-arrivals.log"; NODE_STOP_MIG="${NODE_WORK}/mig-stop"; OUT_MIG="${WORKDIR}/mig-client.out"
run_on "$NODE_CLI_HANDLE" rm -f "$NODE_STOP_MIG" "$NODE_ARR_MIG" >/dev/null 2>&1 || true
run_on "$NODE_CLI_HANDLE" python3 "$NODE_FWD_CLIENT_PY" 127.0.0.1 "$FWD_PORT" \
  "$NODE_ARR_MIG" "$NODE_STOP_MIG" "$(( 2 * MIGRATE_WAIT + 120 ))" "$TICK" >"$OUT_MIG" 2>&1 &
CLIENT_MIG_PID=$!; track_bg "$CLIENT_MIG_PID"
sleep 3   # let the session establish and a few echoes flow before the first cutover

echo "=== migrate $VM_A: $HV_NODE -> $CO_NODE (held session open) ==="
otx vm migrate "$VM_A" --node "$CO_NODE" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "first migration of $VM_A ($HV_NODE -> $CO_NODE) did not complete within ${MIGRATE_WAIT}s"
MIG_NODE1="$(vm_node "$VM_A")"; [ -n "$MIG_NODE1" ] && info "post-first-migrate node reported '$MIG_NODE1'"
sleep 3
echo "=== migrate $VM_A: $CO_NODE -> $HV_NODE (held session still open) ==="
otx vm migrate "$VM_A" --node "$HV_NODE" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "second migration of $VM_A ($CO_NODE -> $HV_NODE) did not complete within ${MIGRATE_WAIT}s"
sleep 3

run_on "$NODE_CLI_HANDLE" touch "$NODE_STOP_MIG" >/dev/null 2>&1 || true
wait "$CLIENT_MIG_PID" 2>/dev/null || true
OUT_MIG_TXT="$(cat "$OUT_MIG" 2>/dev/null)"; echo "$OUT_MIG_TXT"
MAXGAP_MIG="$(grep -oE 'maxgap=[0-9.]+' <<<"$OUT_MIG_TXT" | head -n1 | cut -d= -f2)"
ECHOES="$(grep -oE 'echoes=[0-9]+' <<<"$OUT_MIG_TXT" | head -n1 | cut -d= -f2)"
DROPPED="$(grep -oE 'dropped=[0-9]+' <<<"$OUT_MIG_TXT" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP_MIG" ] && [ -n "$ECHOES" ] || fail "the migration-hold client did not report a metric: ${OUT_MIG_TXT}"
(( ECHOES > 0 )) || fail "no echoes recorded across the two migrations - the held session dropped"
[ "$DROPPED" = "0" ] || fail "the held session closed during a migration - it was CUT, not seamless"
kill "$FWD_MIG_PID" 2>/dev/null || true; wait "$FWD_MIG_PID" 2>/dev/null || true; node_forward_stop
pass "held session survived two live migrations through the co-located gateway (echoes=$ECHOES, max gap ${MAXGAP_MIG}s)"

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the VMs + networks, disables the gateway role on the
# enabled nodes, restores each co-located node's plain-hypervisor agent.yaml, and
# stops the node-side CLI.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== co-located ingress-gateway smoke PASSED ===${NC}"
echo "  co-located hypervisor + gateway: config + role toggle + self-reported ingress endpoint"
echo "  the co-located node stays VM-schedulable: a new VM placed on it"
echo "  reach a guest on a DIFFERENT node THROUGH the co-located gateway; multi-overlay independence"
echo "  role move $CO_NODE -> $CO_B_NODE: the live session survived; new brokerage moved to the other node"
echo "  a live session survived two migrations of the fronted guest through the co-located gateway"
