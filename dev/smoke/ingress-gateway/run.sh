#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM ingress-gateway smoke - proves the operator-facing ingress path end to end:
# `otherix forward` and `otherix ssh` broker a connection through the control
# plane, present the minted session credential to a converged gateway (or the
# control-plane relay), and reach a guest - with the gateway data path surviving a
# live migration without dropping the session. Everything runs through the real
# operator CLI exactly as an operator would, so it exercises the authenticated
# broker path, not a raw connect.
#
# What it proves, end to end (operator CLI only):
#   - operator path: an ingress gateway joins the cluster through the same
#     join-token flow as a node (`otherix node join-token create --kind gateway`
#     + `otherix-gateway bootstrap`) and reports its overlay reconciliation ready;
#   - brokered forward + SEAMLESS LIVE MIGRATION (the load-bearing proof): with
#     continuous in-band traffic flowing through `otherix forward` to a guest echo
#     server on the overlay, the guest is live-migrated to another node and the
#     SAME brokered session stays alive - the byte stream keeps round-tripping
#     with no multi-second gap, converging well within a heartbeat interval - then
#     a second migration moves it again and the session still survives (no stacked
#     blackhole);
#   - brokered ssh over the gateway, surviving migration: `otherix ssh` logs into
#     an overlay guest through the gateway, and a held session keeps emitting
#     output across a live migration;
#   - relay path: `otherix ssh` to a guest on a bridge network connects through
#     the control-plane relay (the broker selects the relay transport when no
#     overlay/gateway path exists);
#   - recovery: with the gateway killed mid-use, the broker reports the overlay as
#     unreachable; once a gateway is serving again, the next connection through the
#     same local listener re-brokers and succeeds (the CLI brokers per connection,
#     so recovery needs no operator restart);
#   - unavailability: brokering for an overlay guest with no gateway covering its
#     overlay is refused (409) rather than silently blackholed;
#   - authorization: a developer who does not own the guest cannot broker it - the
#     broker returns not-found (404), never leaking that the guest exists.
#
# HOW SEAMLESSNESS IS MEASURED:
#   A continuous-traffic client holds ONE TCP connection open to the local
#   `otherix forward` listener and sends a monotonically increasing line every
#   TICK seconds, reading each line echoed back by the guest and recording its
#   arrival time. Because the forward brokers once per accepted connection and
#   splices it through the gateway, that single connection rides the gateway data
#   path; during the migration the client keeps sending, and the smoke asserts the
#   LARGEST gap between consecutive echoes stayed under GAP_THRESHOLD and that the
#   session never dropped. A reboot or a blackholed path shows as a long gap (or a
#   dropped session) and fails loudly. The guest runs a tiny echo server on the
#   overlay (started by cloud-init).
#
# GATEWAY PLACEMENT (dev stack):
#   The three-node dev stack runs an agent on every node; an ingress gateway is a
#   separate VM-less daemon that joins the same overlay substrate. This smoke
#   dedicates the third node to the gateway: it stops that node's agent and runs
#   otherix-gateway there, leaving the first two nodes as VM hosts for the
#   create + migrate. The multi-hop step bounces the guest between those two hosts
#   (a true three-distinct-host hop needs a fourth node the dev stack does not
#   provide). GW_INDEX / the host node indices are overrideable so a larger stack
#   can place the gateway on a dedicated node and hop across three hosts.
#
# WHERE THE BROKERED CLI RUNS:
#   The brokered DATA-PLANE flows (`otherix forward` and `otherix ssh`) run INSIDE
#   the dedicated gateway node, not on the host. In this dev stack the host can
#   reach the control plane but NOT the gateway's data-plane endpoint (no route to
#   the inter-node subnet), so a host-run forward/ssh would broker fine and then
#   fail to dial the gateway. The gateway node reaches both: the control plane on
#   its public API (host.lima.internal:8080, cluster-CA-verified) and the gateway
#   itself (its own advertised endpoint). It is also the stable choice - its agent
#   is intentionally stopped and it is never a live-migration endpoint. The smoke
#   cross-builds the operator CLI for the node's arch, stages it on the node with a
#   config that pins the public API URL + the cluster CA, and runs every brokered
#   data-plane flow there. The broker-status assertions (409 / 404) need no data
#   plane, so they run from the host CLI. Override NODE_CP_URL only when a custom
#   topology needs a different public-API URL for the node.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# plus jq, python3, and go on the host, and python3 + the ssh client on the
# gateway node (provisioned in the Lima VM image / present in the netns).
#
# Usage: make smoke-ingress-gateway   (or: bash dev/smoke/ingress-gateway/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
# Resolved in preconditions to the linux cross-build matching the gateway node's
# arch (the node runs linux, the host may be macOS), overridable via GW_BIN.
GW_BIN="${GW_BIN:-}"
# Cross-build of the operator CLI for the gateway node's arch, staged on the node
# to run the brokered data-plane flows there. Resolved in preconditions,
# overridable via CLI_BIN.
CLI_BIN="${CLI_BIN:-}"

NODE1="node-1"                         # VM host + live-migration source
NET="ingress-ovl"                      # dhcp-enabled overlay
SUBNET="10.93.0.0/24"                  # unlikely to clash with the dev stack
VM="ingress-vm"                        # the forwarded + ssh'd overlay guest
# The gateway's cluster name. Defaults to a unique-per-run value: the cluster has
# no node-delete command yet, so a fixed name would leave a stale agent cert that
# 409s the next join. A unique suffix keeps the smoke re-runnable without a manual
# override or a clean restart; GW_NAME=... still pins it when a run must reuse one.
GW_NAME="${GW_NAME:-ingress-gw-$(date +%H%M%S)-${RANDOM}}"
ECHO_PORT="${ECHO_PORT:-9000}"         # the guest echo server port the gateway dials
GW_LISTEN_PORT="${GW_LISTEN_PORT:-9443}" # the gateway control listener (mTLS) inside its node

# The bridge guest (relay-path proof): a managed bridge network so the agent
# learns the guest IP from the CP-IPAM DHCP reservation, exactly as the relay
# resolves it for ssh.
NET_BR="ingress-br"                    # managed bridge network
BR_SUBNET="10.94.0.0/24"               # unlikely to clash with the dev stack
BR_NAME="otxingbr0"                    # Linux bridge device name (IFNAMSIZ-safe)
VM_BR="ingress-vm-br"                  # the relay-path guest

SUFFIX="${SUFFIX:-ingress.otherix.local}" # cluster SSH-ingress DNS suffix
LOGIN="${LOGIN:-ubuntu}"               # default user in the Noble minimal cloudimg

# Developer (non-owner) used to prove the ownership gate returns 404.
DEV_USER="${DEV_USER:-ingress-dev}"
DEVPASS="${DEVPASS:-ingress-dev-pw-12345}"      # >= 12 chars (NIST SP 800-63B)
DEV_CLUSTER="${DEV_CLUSTER:-ingress-dev-cluster}"

# Local listener ports for `otherix forward`. FWD_PORT is the node-side
# seamless-migration + recovery listener; PROBE_FWD_PORT is the host-side
# 409 / 404 broker-status probes (broker-only, no data plane).
FWD_PORT="${FWD_PORT:-19000}"
PROBE_FWD_PORT="${PROBE_FWD_PORT:-19001}"

# The gateway runs on the third dev node (its agent is stopped first); the VM
# hosts are the first two nodes. Override to place the gateway elsewhere.
GW_INDEX="${GW_INDEX:-3}"

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"      # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"            # seconds for a network to reconcile ready
GW_READY_WAIT="${GW_READY_WAIT:-180}"  # seconds for the gateway to report overlay-ready
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"  # seconds for the guest to lease an overlay IP and announce it
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"    # seconds for a live migrate cutover (disk copy on TCG is slow)
CONNECT_WAIT="${CONNECT_WAIT:-600}"    # seconds to retry the first ssh connect while a guest boots
SSH_TIMEOUT="${SSH_TIMEOUT:-30}"       # per-connect ssh ConnectTimeout
TICK="${TICK:-0.2}"                    # seconds between continuous-traffic lines
GAP_THRESHOLD="${GAP_THRESHOLD:-10}"   # max tolerated seconds with no echo across a cutover (< heartbeat)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

GW_HANDLE="$(smoke_handle "$GW_INDEX")"
# The brokered data-plane CLI runs on the gateway node (see "WHERE THE BROKERED
# CLI RUNS"). It is the same node that hosts the gateway daemon.
NODE_CLI_HANDLE="$GW_HANDLE"

# net_ready NODE NET -> 0 when the network reconciled "ready" on NODE.
net_ready() {
  [[ "$(otx network get "$2" --output json 2>/dev/null \
      | jq -r --arg n "$1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
      == "ready" ]]
}

# gw_overlay_ready -> 0 when the gateway reports the overlay reconciled ready
# (the selection gate: a gateway that has not programmed its forwarding database
# would blackhole the forward).
gw_overlay_ready() { net_ready "$GW_NAME" "$NET"; }

# vm_node VM -> the node the VM currently runs on.
vm_node() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.current_node_id // .status.current_node // empty'; }

# node_path HANDLE SRC -> echo a path to SRC usable INSIDE the node. On netns the
# host filesystem is shared, so the host path works as-is; on Lima the file is
# copied into the node's /tmp.
node_path() {
  local handle="$1" src="$2" base
  base="$(basename "$src")"
  case "$SMOKE_PLATFORM" in
    netns) printf '%s' "$src" ;;
    lima)  limactl cp "$src" "$handle:/tmp/$base" >/dev/null; printf '/tmp/%s' "$base" ;;
  esac
}

# launch the gateway data plane on the dedicated node (detached).
gw_serve() {
  run_on "$GW_HANDLE" sudo sh -c 'setsid nohup otherix-gateway serve >/var/log/otherix-gateway.log 2>&1 < /dev/null &'
}

# wait_gw_ready -> block (up to GW_READY_WAIT) until the gateway reports NET ready.
wait_gw_ready() {
  local deadline=$(( SECONDS + GW_READY_WAIT ))
  while (( SECONDS < deadline )); do gw_overlay_ready && return 0; sleep 3; done
  return 1
}

# ticks_count FILE / max_tick_gap FILE -> readouts of the ssh tick stream (a
# "TICK <guest-epoch> <seq>" line per second). max_tick_gap is the largest jump
# between consecutive guest epochs, computed on the guest's own clock so it is
# free of host/guest skew.
ticks_count() { grep -c '^TICK' "$1" 2>/dev/null || echo 0; }
max_tick_gap() { awk '/^TICK/{e=$2+0; if(p!=""){g=e-p; if(g>m)m=g} p=e} END{print m+0}' "$1"; }

# --- node-side brokered CLI plumbing (resolved in preconditions) -------
# NODE_OTX / NODE_CFG: the staged operator CLI and its config on the node.
# NODE_WORK: a node-local scratch dir for the ssh cert cache, the ssh shim, and
# the per-run stop / arrivals files (kept off the host because the forward
# listener binds the node's loopback, so its clients must run on the node too).
NODE_OTX=""
NODE_CFG=""
NODE_WORK=""
NODE_OP_HOME=""
NODE_SHIM=""
NODE_PROBE_PY=""
NODE_FWD_CLIENT_PY=""
NODE_OPSSH=""

# node_forward_start VM PORT LPORT LOGFILE -> launch `otherix forward` on the node
# (backgrounded on the host side via run_on; its stdout is captured to LOGFILE on
# the host). The listener binds 127.0.0.1:LPORT inside the node.
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

# op_ssh_cmd VM LOGIN CMD -> run CMD in the guest as the operator through the
# brokered ssh path on the node, printing the guest stdout. The node-side wrapper
# mints the operator cert with `otherix ssh` under a no-op ssh shim (so it mints
# without connecting) and then runs the system ssh whose ProxyCommand re-enters
# `otherix ssh proxy` to broker the ingress and splice over the selected transport
# (gateway for the overlay guest, relay for the bridge guest). CMD is base64-coded
# end to end so an arbitrary guest command survives the host -> node hop unmangled.
op_ssh_cmd() {
  local vm="$1" login="$2" cmd="$3" cmd_b64
  cmd_b64="$(printf '%s' "$cmd" | base64 | tr -d '\n')"
  run_on "$NODE_CLI_HANDLE" env \
    OTX="$NODE_OTX" CFG="$NODE_CFG" OP_HOME="$NODE_OP_HOME" SHIM="$NODE_SHIM" \
    SSH_TIMEOUT="$SSH_TIMEOUT" \
    sh "$NODE_OPSSH" "$vm" "$login" "$cmd_b64"
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
# and launches it detached so it survives cloud-init exiting. Once the NIC has an
# IPv4 the guest announces it on the serial console as "OTHERIX_GUEST_IP <ip>"
# (repeatedly), which the harness reads off the captured serial.log to learn the
# address the gateway must dial. This guest is also created with --ssh-ingress, so
# the control plane MERGES its SSH-CA-trust cloud-config with this one (multipart
# with an explicit merge_how), letting one guest serve both the forward and the
# ssh proofs. python3 ships in the minimal cloud image (cloud-init depends on it).
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
# threads so a brief cutover stall never deadlocks the loop. It exits when the
# stop file appears (or the hard deadline passes) and prints the largest gap
# between consecutive echoes - the seamlessness metric. Staged on the node.
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
# otherwise. Used (on the node) to assert a brokered connection works, and (on the
# host) by the broker-status probes. Staged on both.
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

# Node-side brokered-ssh wrapper. Mints the operator cert with `otherix ssh` under
# a no-op ssh shim (so the cert lands in OP_HOME without a connect), then connects
# with the system ssh whose ProxyCommand re-enters `otherix ssh proxy` to broker +
# splice. Args: VM LOGIN CMD_B64. Env: OTX CFG OP_HOME SHIM SSH_TIMEOUT.
OPSSH_FILE="${WORKDIR}/node_op_ssh.sh"
cat >"$OPSSH_FILE" <<'SHEOF'
#!/bin/sh
set -u
vm="$1"; login="$2"; cmd="$(printf '%s' "$3" | base64 -d)"
# Mint the operator cert. The shim shadows ssh so `otherix ssh` mints without
# connecting; failures here are tolerated (the real connect surfaces them).
PATH="$SHIM:$PATH" HOME="$OP_HOME" OTHERIX_CONFIG="$CFG" \
  "$OTX" ssh "$vm" --login "$login" </dev/null >/dev/null 2>&1 || true
# Real connect. HOME / OTHERIX_CONFIG are exported so the ProxyCommand re-enters
# the same CLI + config when it brokers the ingress.
export HOME="$OP_HOME" OTHERIX_CONFIG="$CFG"
exec ssh \
  -i "$OP_HOME/.otherix/ssh/id_ed25519" \
  -o "CertificateFile=$OP_HOME/.otherix/ssh/id_ed25519-cert.pub" \
  -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new \
  -o "UserKnownHostsFile=$OP_HOME/known_hosts" -o BatchMode=yes \
  -o "ConnectTimeout=$SSH_TIMEOUT" \
  -o "ProxyCommand=$OTX ssh proxy %h %p" \
  "$login@$vm" "$cmd"
SHEOF

# forward_status_probe WANT LPORT OTXARGS... -> start `otherix <OTXARGS> --listen
# 127.0.0.1:LPORT` on the HOST, make ONE local connection (which forces a broker
# attempt), and assert the forward's stderr carries WANT (e.g. "HTTP 409" /
# "HTTP 404"). This asserts the broker's status without a raw token and needs no
# data plane, so it runs on the host: the forward prints the broker failure to
# stderr per connection.
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

# --- background-process bookkeeping + cleanup --------------------------
BG_PIDS=()
track_bg() { BG_PIDS+=("$1"); }
kill_bg()  { local p; for p in "${BG_PIDS[@]:-}"; do [ -n "$p" ] || continue; kill "$p" 2>/dev/null || true; pkill -P "$p" 2>/dev/null || true; done; }

GW_LAUNCHED=""
PRIOR_SSH_ENABLED=""
PRIOR_SSH_SUFFIX=""
restore_ssh_ingress() {
  [ -n "$PRIOR_SSH_ENABLED" ] || return 0
  if [ "$PRIOR_SSH_ENABLED" = "true" ]; then
    otx cluster set-ssh-ingress --enabled --suffix "$PRIOR_SSH_SUFFIX" >/dev/null 2>&1 || true
  else
    otx cluster set-ssh-ingress --enabled=false >/dev/null 2>&1 || true
  fi
}

# kill_node_cli -> stop any straggler brokered CLI / ssh processes on the node.
kill_node_cli() {
  [ -n "$NODE_OTX" ] || return 0
  run_on "$NODE_CLI_HANDLE" sh -c "pkill -f '$NODE_OTX'; pkill -f 'ProxyCommand='; pkill -f 'forward_client'" >/dev/null 2>&1 || true
}

cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the VMs/networks/gateway up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  kill_bg
  kill_node_cli
  run_on "$GW_HANDLE" sudo pkill -f 'otherix-gateway serve' >/dev/null 2>&1 || true
  # Node-side scratch (Lima only; on netns these alias host paths under WORKDIR,
  # removed below). The staged config carries an operator token, so drop it too.
  if [ "$SMOKE_PLATFORM" = "lima" ] && [ -n "$NODE_OTX" ]; then
    run_on "$NODE_CLI_HANDLE" rm -rf \
      "$NODE_WORK" "$NODE_OTX" "$NODE_CFG" "$NODE_PROBE_PY" "$NODE_FWD_CLIENT_PY" "$NODE_OPSSH" >/dev/null 2>&1 || true
  fi
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM_BR" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  otx network delete "$NET_BR" --force >/dev/null 2>&1 || true
  otx user delete "$DEV_USER" --force >/dev/null 2>&1 || true
  otx config remove "$DEV_CLUSTER" --force >/dev/null 2>&1 || true
  restore_ssh_ingress
  # Bring the dedicated node's agent back so a subsequent run finds three hosts.
  if [ -n "$GW_LAUNCHED" ]; then
    info "restart the agent on the gateway node (best effort) so the stack returns to three hosts"
    run_on "$GW_HANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl start otherix-agent' >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== ingress-gateway smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v python3 >/dev/null || fail "python3 is required on the host"
command -v go >/dev/null || fail "go is required on the host (to cross-build the node CLI)"
command -v curl >/dev/null || fail "curl is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" forward --help >/dev/null 2>&1 || fail "this otherix build has no 'forward' command (rebuild from the current tree)"
"$OTX" ssh --help >/dev/null 2>&1 || fail "this otherix build has no 're-homed' ssh command (rebuild from the current tree)"
# The gateway runs on a linux node; pick the cross-build for that node's arch
# (the host may be macOS, whose binary cannot run there), building it on demand.
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
# The operator CLI for the node. The make cross-build targets do NOT build the
# operator CLI (cmd/cli), so build it explicitly for the node's arch.
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

# --- provision the brokered CLI on the gateway node --------------------
echo "=== preconditions: stage the operator CLI on the gateway node ==="
# The node reaches the control plane on its public API host (the agent dials the
# same host for mTLS; the public API shares it on the public port). Derive that
# URL from a running agent's config rather than hard-coding it, and pin the public
# port from CP_BASE. Override NODE_CP_URL for a custom topology.
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
_agent_url="$(run_on "$SMOKE_HANDLE_1" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)"
_agent_host="${_agent_url#*://}"; _agent_host="${_agent_host%%:*}"
_pub_port="${CP_BASE##*:}"; { [ -n "$_pub_port" ] && [ "$_pub_port" != "$CP_BASE" ]; } || _pub_port=8080
[ -n "$_agent_host" ] || fail "could not resolve the node-facing CP host (set NODE_CP_URL=https://host:port)"
NODE_CP_URL="${NODE_CP_URL:-https://${_agent_host}:${_pub_port}}"
info "node CLI -> control plane at ${NODE_CP_URL}"

# The node trusts the dev cluster CA (the same CA that signs both the public CP
# cert and the gateway cert), so a verified (no insecure-skip) dial works for both
# the broker call and the gateway data-plane leg.
CLUSTER_CA_PEM="${CLUSTER_CA_PEM:-.local/pki/cluster-ca.crt}"
[ -f "$CLUSTER_CA_PEM" ] || fail "cluster CA PEM not found at '$CLUSTER_CA_PEM' (set CLUSTER_CA_PEM=...)"
# The node CLI reuses the operator token the seeded host profile already holds
# (the bootstrap admin), so no extra user/token plumbing is needed.
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
NODE_OPSSH="$(node_path "$NODE_CLI_HANDLE" "$OPSSH_FILE")"

# Node-local scratch: the ssh cert cache, the no-op ssh shim, and per-run stop /
# arrivals files (the forward listener binds the node's loopback, so the clients
# that drive it run on the node too).
case "$SMOKE_PLATFORM" in
  netns) NODE_WORK="$WORKDIR" ;;
  lima)  NODE_WORK="/tmp/otx-ingress-$$" ;;
esac
NODE_OP_HOME="${NODE_WORK}/op-home"
NODE_SHIM="${NODE_WORK}/shim"
run_on "$NODE_CLI_HANDLE" sh -c \
  "mkdir -p '$NODE_OP_HOME' '$NODE_SHIM'; printf '#!/bin/sh\nexit 0\n' > '$NODE_SHIM/ssh'; chmod +x '$NODE_SHIM/ssh'" \
  || fail "could not prepare the node scratch dir at $NODE_WORK"

# Prove the staged CLI talks to the control plane with a VERIFIED TLS dial (no
# insecure-skip) before relying on it for the brokered flows.
run_on "$NODE_CLI_HANDLE" env OTHERIX_CONFIG="$NODE_CFG" "$NODE_OTX" node list >/dev/null 2>&1 \
  || fail "the staged node CLI could not reach/verify the control plane at ${NODE_CP_URL} (TLS trust or reachability)"
pass "operator CLI staged on $GW_HANDLE; verified TLS dial to ${NODE_CP_URL}"

# best-effort delete-first so stale leftovers from a prior run do not 409
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_BR" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
otx network delete "$NET_BR" --force >/dev/null 2>&1 || true
otx user delete "$DEV_USER" --force >/dev/null 2>&1 || true
otx config remove "$DEV_CLUSTER" --force >/dev/null 2>&1 || true

# --- step 1: enable cluster SSH ingress --------------------------------
echo "=== step 1: enable cluster SSH ingress (--suffix $SUFFIX) ==="
PRIOR="$(otx cluster get-ssh-ingress --output json 2>/dev/null || echo '{}')"
PRIOR_SSH_ENABLED="$(jq -r '.enabled // false' <<<"$PRIOR")"
PRIOR_SSH_SUFFIX="$(jq -r '.cluster_suffix // ""' <<<"$PRIOR")"
otx cluster set-ssh-ingress --enabled --suffix "$SUFFIX" >/dev/null \
  || fail "cluster set-ssh-ingress failed"
pass "cluster SSH ingress enabled (suffix $SUFFIX)"

# --- step 2: overlay + the forwarded/ssh guest -------------------------
echo "=== step 2: dhcp overlay $NET + guest $VM (echo server + ssh ingress) ==="
otx network create "$NET" --type overlay --subnet "$SUBNET" --dhcp \
  || fail "network create $NET failed"
deadline=$(( SECONDS + NET_WAIT )); ok=0
info "waiting for $NET to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do net_ready "$NODE1" "$NET" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NET did not reconcile ready on $NODE1 within ${NET_WAIT}s"

info "creating $VM on $NODE1 (echo server on :${ECHO_PORT}, --ssh-ingress)"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --ssh-ingress --vcpus 2 --memory-mb 2048 \
  --user-data - --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
pass "$VM running on $NODE1 (id=$VMID)"

# Discover the guest's overlay IP from the serial announce sentinel.
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

# --- step 3: no converged gateway -> ingress_unavailable ---------------
echo "=== step 3: broker before any gateway joins -> 409 ingress unavailable ==="
# No gateway has joined yet, so the broker cannot select a converged gateway for
# the overlay guest and refuses with 409 rather than blackholing. This is a
# broker-status assertion (no data plane), so it runs from the host CLI.
forward_status_probe "HTTP 409" "$PROBE_FWD_PORT" forward "$VM" "$ECHO_PORT" \
  || fail "broker did not return 409 for an overlay guest with no gateway (expected ingress unavailable)"
pass "broker refuses an overlay guest with no converged gateway (409)"

# --- step 4: join an ingress gateway -----------------------------------
echo "=== step 4: join ingress gateway $GW_NAME ==="
# The gateway's control-plane URL: reuse the agent's, read from the dev agent
# config staged on the node, so the smoke follows whatever endpoint the stack
# bootstrapped its agents against.
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
GW_CP_URL="${GW_CP_URL:-$(run_on "$GW_HANDLE" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)}"
[ -n "$GW_CP_URL" ] || fail "could not resolve the gateway control-plane URL (set GW_CP_URL=https://...:PORT)"
GW_NODE_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1")"
[ -n "$GW_NODE_IP" ] || fail "could not resolve the gateway node IP"

# The gateway joins the WireGuard mesh, so it needs a WG UDP advertised endpoint
# the VM-host agents dial for the handshake. Learn the inter-node WG subnet + port
# a peer agent advertises, then pick the gateway node's address on THAT subnet.
# The gateway node's agent is stopped below, freeing UDP 51820 for the gateway.
# shellcheck disable=SC2016  # $2/$4 are awk fields, kept literal on purpose
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
# config (bootstrap never overwrites an existing config, even with --force) and
# the gateway generates a fresh WireGuard identity. This keeps a re-run clean.
run_on "$GW_HANDLE" sudo rm -f /etc/otherix/gateway.yaml /var/lib/otherix/wg-gateway/private.key >/dev/null 2>&1 || true
# --force re-issues the gateway cert material: the repurposed node carries the
# agent's shared ca.crt, which otherwise reads as partial cert state, and it also
# makes a re-run idempotent over a prior gateway identity.
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

# --- step 5: brokered forward reachability + seamless migration --------
echo "=== step 5: otherix forward reachability + seamless live migration ==="
FWD_LOG="${WORKDIR}/forward.log"
node_forward_start "$VM" "$ECHO_PORT" "$FWD_PORT" "$FWD_LOG"
FWD_PID=$!; track_bg "$FWD_PID"
deadline=$(( SECONDS + 20 )); ok=0
while (( SECONDS < deadline )); do grep -q 'forwarding ' "$FWD_LOG" 2>/dev/null && { ok=1; break; }; sleep 1; done
(( ok == 1 )) || fail "otherix forward did not open a local listener on the node: $(cat "$FWD_LOG" 2>/dev/null)"

PROBE_OUT="$(node_probe "$FWD_PORT")"
grep -q PROBE_OK <<<"$PROBE_OUT" \
  || fail "forward reachability probe failed: ${PROBE_OUT}; forward log: $(cat "$FWD_LOG" 2>/dev/null)"
pass "otherix forward reached the guest echo server through the gateway ($PROBE_OUT)"

# Seamless migration window: one held connection through the forward listener,
# migrate the guest, assert the byte stream never stalled or dropped.
NODE_ARR1="${NODE_WORK}/arrivals1.log"; NODE_STOP1="${NODE_WORK}/stop1"; OUT1="${WORKDIR}/client1.out"
run_on "$NODE_CLI_HANDLE" rm -f "$NODE_STOP1" "$NODE_ARR1" >/dev/null 2>&1 || true
run_on "$NODE_CLI_HANDLE" python3 "$NODE_FWD_CLIENT_PY" 127.0.0.1 "$FWD_PORT" \
  "$NODE_ARR1" "$NODE_STOP1" "$(( MIGRATE_WAIT + 60 ))" "$TICK" >"$OUT1" 2>&1 &
CLIENT_PID=$!; track_bg "$CLIENT_PID"
sleep 3   # let the session establish and a few echoes flow before the cutover
echo "=== live migrate $NODE1 -> $TARGET with the brokered session held open ==="
otx vm migrate "$VM" --node "$TARGET" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "live migrate $NODE1 -> $TARGET did not complete within ${MIGRATE_WAIT}s"
NOW_NODE="$(vm_node "$VM")"
[[ "$NOW_NODE" == *"$TARGET"* ]] || info "post-migrate current node reported as '$NOW_NODE'"
sleep 3
run_on "$NODE_CLI_HANDLE" touch "$NODE_STOP1" >/dev/null 2>&1 || true
wait "$CLIENT_PID" 2>/dev/null || true
OUT1_TXT="$(cat "$OUT1" 2>/dev/null)"; echo "$OUT1_TXT"
MAXGAP="$(grep -oE 'maxgap=[0-9.]+' <<<"$OUT1_TXT" | head -n1 | cut -d= -f2)"
ECHOES="$(grep -oE 'echoes=[0-9]+' <<<"$OUT1_TXT" | head -n1 | cut -d= -f2)"
DROPPED="$(grep -oE 'dropped=[0-9]+' <<<"$OUT1_TXT" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP" ] && [ -n "$ECHOES" ] || fail "continuous-traffic client did not report a metric: ${OUT1_TXT}"
(( ECHOES > 0 )) || fail "no echoes recorded across the migration - the brokered session dropped"
[ "$DROPPED" = "0" ] || fail "the brokered session closed during the migration - not seamless (a cutover-time reset)"
awk -v g="$MAXGAP" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "session stalled ${MAXGAP}s across the cutover (>= ${GAP_THRESHOLD}s) - not seamless"
pass "brokered forward survived live migration (echoes=$ECHOES, max gap ${MAXGAP}s < ${GAP_THRESHOLD}s)"

# Multi-hop: migrate back, a fresh held connection still survives (no stacked
# blackhole from repeated cutovers).
echo "=== multi-hop migrate $TARGET -> $NODE1 (no stacked blackhole) ==="
NODE_ARR2="${NODE_WORK}/arrivals2.log"; NODE_STOP2="${NODE_WORK}/stop2"; OUT2="${WORKDIR}/client2.out"
run_on "$NODE_CLI_HANDLE" rm -f "$NODE_STOP2" "$NODE_ARR2" >/dev/null 2>&1 || true
run_on "$NODE_CLI_HANDLE" python3 "$NODE_FWD_CLIENT_PY" 127.0.0.1 "$FWD_PORT" \
  "$NODE_ARR2" "$NODE_STOP2" "$(( MIGRATE_WAIT + 60 ))" "$TICK" >"$OUT2" 2>&1 &
CLIENT_PID2=$!; track_bg "$CLIENT_PID2"
sleep 3
otx vm migrate "$VM" --node "$NODE1" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "multi-hop migrate $TARGET -> $NODE1 did not complete within ${MIGRATE_WAIT}s"
sleep 3
run_on "$NODE_CLI_HANDLE" touch "$NODE_STOP2" >/dev/null 2>&1 || true
wait "$CLIENT_PID2" 2>/dev/null || true
OUT2_TXT="$(cat "$OUT2" 2>/dev/null)"; echo "$OUT2_TXT"
MAXGAP2="$(grep -oE 'maxgap=[0-9.]+' <<<"$OUT2_TXT" | head -n1 | cut -d= -f2)"
ECHOES2="$(grep -oE 'echoes=[0-9]+' <<<"$OUT2_TXT" | head -n1 | cut -d= -f2)"
DROPPED2="$(grep -oE 'dropped=[0-9]+' <<<"$OUT2_TXT" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP2" ] && [ -n "$ECHOES2" ] || fail "multi-hop client did not report a metric: ${OUT2_TXT}"
(( ECHOES2 > 0 )) || fail "no echoes recorded across the multi-hop migration - the session dropped"
[ "$DROPPED2" = "0" ] || fail "the brokered session closed during the multi-hop migration - not seamless"
awk -v g="$MAXGAP2" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "session stalled ${MAXGAP2}s across the multi-hop cutover (>= ${GAP_THRESHOLD}s)"
pass "brokered forward survived the multi-hop migration (echoes=$ECHOES2, max gap ${MAXGAP2}s < ${GAP_THRESHOLD}s)"

kill "$FWD_PID" 2>/dev/null || true; wait "$FWD_PID" 2>/dev/null || true
node_forward_stop

# --- step 6: brokered ssh over the gateway + survives migration --------
echo "=== step 6: otherix ssh (overlay, gateway transport) + survives migration ==="
# The guest now sits on $NODE1 (returned by the multi-hop). Retry the brokered
# login while the guest finishes booting; a returned nonce is the real-login
# assertion over the gateway transport.
NONCE_OV="ingress-ov-${RANDOM}-${RANDOM}"
deadline=$(( SECONDS + CONNECT_WAIT )); ok=0
info "waiting for otherix ssh $VM to log in over the gateway (<= ${CONNECT_WAIT}s)"
while (( SECONDS < deadline )); do
  OUT_OV="$(op_ssh_cmd "$VM" "$LOGIN" "echo ${NONCE_OV}" 2>/dev/null || true)"
  grep -q "$NONCE_OV" <<<"$OUT_OV" && { ok=1; break; }
  sleep 8
done
(( ok == 1 )) || { echo "--- last ssh attempt (stderr) ---"; op_ssh_cmd "$VM" "$LOGIN" "echo ${NONCE_OV}" 2>&1 | tail -8 || true; \
  fail "otherix ssh $VM (overlay, gateway) never returned the nonce within ${CONNECT_WAIT}s"; }
pass "otherix ssh $VM logged into the overlay guest over the gateway (nonce returned)"

# Survives migration: hold a brokered ssh session running an indefinite tick loop,
# migrate the guest, and assert the stream kept flowing across the cutover.
SSH_TICKS="${WORKDIR}/ssh-ticks.log"; : >"$SSH_TICKS"
# shellcheck disable=SC2016  # the loop body must run in the GUEST, not expand on the host
op_ssh_cmd "$VM" "$LOGIN" 'i=0; while true; do echo "TICK $(date +%s) $i"; i=$((i+1)); sleep 1; done' >"$SSH_TICKS" 2>/dev/null &
SSH_BG=$!; track_bg "$SSH_BG"
sleep 6
(( $(ticks_count "$SSH_TICKS") > 0 )) || fail "the brokered ssh tick stream produced nothing before migration"
BEFORE="$(ticks_count "$SSH_TICKS")"
echo "=== live migrate $NODE1 -> $TARGET with the ssh session held open ==="
otx vm migrate "$VM" --node "$TARGET" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "live migrate (ssh held) $NODE1 -> $TARGET did not complete within ${MIGRATE_WAIT}s"
sleep 5
AFTER="$(ticks_count "$SSH_TICKS")"
(( AFTER > BEFORE )) || fail "the ssh session produced no ticks after the migration ($BEFORE -> $AFTER) - the session dropped"
SSH_GAP="$(max_tick_gap "$SSH_TICKS")"
awk -v g="$SSH_GAP" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "the ssh session stalled ${SSH_GAP}s across the cutover (>= ${GAP_THRESHOLD}s) - not seamless"
kill "$SSH_BG" 2>/dev/null || true; pkill -P "$SSH_BG" 2>/dev/null || true
kill_node_cli
pass "otherix ssh session survived live migration (ticks ${BEFORE}->${AFTER}, max gap ${SSH_GAP}s < ${GAP_THRESHOLD}s)"

# --- step 7: brokered ssh over the relay (bridge guest) ----------------
echo "=== step 7: otherix ssh (bridge, relay transport) ==="
otx network create "$NET_BR" --type bridge --managed --bridge-name "$BR_NAME" \
  --egress nat --subnet "$BR_SUBNET" --dhcp \
  || fail "network create $NET_BR failed"
deadline=$(( SECONDS + NET_WAIT )); ok=0
info "waiting for $NET_BR to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do net_ready "$NODE1" "$NET_BR" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NET_BR did not reconcile ready on $NODE1 within ${NET_WAIT}s"

info "creating $VM_BR on $NODE1 (managed bridge $NET_BR, --ssh-ingress)"
otx vm create "$VM_BR" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET_BR" \
  --ssh-ingress --vcpus 2 --memory-mb 2048 \
  --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM_BR did not reach running within ${CREATE_WAIT}s"
pass "$VM_BR running on $NODE1 (bridge $NET_BR)"

# A bridge guest has no overlay/gateway path, so the broker selects the
# control-plane relay transport; a returned nonce proves the relay reaches the
# guest sshd.
NONCE_BR="ingress-br-${RANDOM}-${RANDOM}"
deadline=$(( SECONDS + CONNECT_WAIT )); ok=0
info "waiting for otherix ssh $VM_BR to log in over the relay (<= ${CONNECT_WAIT}s)"
while (( SECONDS < deadline )); do
  OUT_BR="$(op_ssh_cmd "$VM_BR" "$LOGIN" "echo ${NONCE_BR}" 2>/dev/null || true)"
  grep -q "$NONCE_BR" <<<"$OUT_BR" && { ok=1; break; }
  sleep 8
done
(( ok == 1 )) || { echo "--- last ssh attempt (stderr) ---"; op_ssh_cmd "$VM_BR" "$LOGIN" "echo ${NONCE_BR}" 2>&1 | tail -8 || true; \
  fail "otherix ssh $VM_BR (bridge, relay) never returned the nonce within ${CONNECT_WAIT}s"; }
pass "otherix ssh $VM_BR logged into the bridge guest over the control-plane relay (nonce returned)"

# --- step 8: ownership gate - a developer cannot reach another's VM ----
echo "=== step 8: a developer who does not own $VM is refused with 404 ==="
otx user create "$DEV_USER" --role developer --password-stdin <<<"$DEVPASS" >/dev/null 2>&1 \
  || fail "could not create developer user $DEV_USER"
DEV_FP="$(curl -fsSk "${CP_BASE}/v1/ca" | jq -r '.signer_fingerprint_sha256')"
[[ -n "$DEV_FP" && "$DEV_FP" != "null" ]] || fail "could not read cluster CA fingerprint from ${CP_BASE}/v1/ca"
OTHERIX_PASSWORD="$DEVPASS" otx config add cluster \
  --name "$DEV_CLUSTER" --server "$CP_BASE" --login "$DEV_USER" \
  --ca-fingerprint "$DEV_FP" --set-current=false --force >/dev/null 2>&1 \
  || fail "could not log in as $DEV_USER / create the $DEV_CLUSTER profile"
# The developer does not own $VM (the bootstrap admin created it), so the broker
# returns 404 - never 403 - so it leaks no existence of a VM the caller cannot see.
# This is a broker-status assertion (no data plane), so it runs from the host CLI.
forward_status_probe "HTTP 404" "$PROBE_FWD_PORT" --cluster "$DEV_CLUSTER" forward "$VM" "$ECHO_PORT" \
  || fail "developer broker for $VM did not return 404 (the ownership gate must hide a VM the caller does not own)"
pass "developer $DEV_USER refused $VM through the broker with 404 (existence not leaked)"

# --- step 9: gateway death -> the next connection re-brokers -----------
echo "=== step 9: gateway death mid-use -> re-broker + reconnect ==="
RFWD_LOG="${WORKDIR}/forward-recover.log"
node_forward_start "$VM" "$ECHO_PORT" "$FWD_PORT" "$RFWD_LOG"
RFWD_PID=$!; track_bg "$RFWD_PID"
deadline=$(( SECONDS + 20 )); ok=0
while (( SECONDS < deadline )); do grep -q 'forwarding ' "$RFWD_LOG" 2>/dev/null && { ok=1; break; }; sleep 1; done
(( ok == 1 )) || fail "otherix forward did not open a local listener for the recovery probe: $(cat "$RFWD_LOG" 2>/dev/null)"

# A connection works while the gateway is alive.
P1="$(node_probe "$FWD_PORT")"
grep -q PROBE_OK <<<"$P1" || fail "pre-kill recovery probe failed (gateway should be reachable): $P1"
info "pre-kill connection ok ($P1)"

# Kill the gateway data plane.
run_on "$GW_HANDLE" sudo pkill -f 'otherix-gateway serve' >/dev/null 2>&1 || true
sleep 3
# A new connection through the SAME listener now fails (no serving gateway) -
# operator-visible loss, not a silent hang.
P2="$(node_probe "$FWD_PORT")"
grep -q PROBE_OK <<<"$P2" && fail "a connection still succeeded after the gateway was killed - expected a failure"
info "with the gateway down the connection fails as expected ($P2)"

# Bring a gateway back and let it re-converge on the overlay.
gw_serve
info "waiting for gateway $GW_NAME to report $NET ready again (<= ${GW_READY_WAIT}s)"
wait_gw_ready || fail "gateway $GW_NAME did not re-report $NET ready within ${GW_READY_WAIT}s after restart"

# The next connection through the SAME listener re-brokers (the CLI brokers per
# connection) and succeeds - recovery needs no operator restart.
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do
  P3="$(node_probe "$FWD_PORT")"
  grep -q PROBE_OK <<<"$P3" && { ok=1; break; }
  sleep 3
done
(( ok == 1 )) || fail "no connection through the same listener recovered after the gateway came back (last: ${P3:-none})"
pass "the next connection re-brokered and reconnected once a gateway was serving again ($P3)"
kill "$RFWD_PID" 2>/dev/null || true; wait "$RFWD_PID" 2>/dev/null || true
node_forward_stop

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the VMs + networks + developer, stops the node-side CLI
# and gateway, restores the ssh-ingress switch, and restarts the gateway node's
# agent so a re-run finds the stack back at three hosts.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== ingress-gateway smoke PASSED ===${NC}"
echo "  forward reachability + seamless live migration + multi-hop (brokered, gateway data path, node-run CLI)"
echo "  otherix ssh over the gateway (overlay) survives migration; otherix ssh over the relay (bridge)"
echo "  no-gateway -> 409; developer non-owner -> 404; gateway death -> re-broker + reconnect"
