#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM ingress-gateway smoke - proves an L4 forward through a VM-less ingress
# gateway reaches a guest on an overlay AND survives a live migration of that
# guest without dropping the session, driven through the operator CLIs as a real
# operator would provision the cluster.
#
# What it proves, end to end:
#   - operator path: an ingress gateway joins the cluster through the same
#     join-token flow as a node (`otherix node join-token create --kind gateway`
#     + `otherix-gateway bootstrap`), and reports its overlay reconciliation
#     ready;
#   - reachability: a session opened through the gateway's connect route reaches
#     a guest echo server on the overlay and bytes round-trip in both directions
#     (the gateway dials the guest's overlay IP and splices);
#   - SEAMLESS LIVE MIGRATION (the load-bearing proof): with continuous in-band
#     traffic flowing through the gateway, the guest is live-migrated to another
#     node and the SAME session stays alive - the byte stream keeps round-tripping
#     with no multi-second gap, converging well within a heartbeat interval (the
#     forwarding-database nudge re-points the gateway at the guest's new node
#     immediately, instead of waiting for the next heartbeat);
#   - multi-hop: a second live migration moves the guest again and the session
#     still survives, proving repeated cutovers do not accumulate a dead path.
#
# THE FORWARD PATH (what the connect route does here):
#   The gateway exposes POST /v1/connect under its control-plane-identity gate.
#   A caller holding the cluster control-plane identity opens a session carrying
#   the target (guest_ip, port); the gateway dials that target over the overlay
#   and splices the connection to it byte for byte. This smoke drives that route
#   directly with a control-plane-identity client (a short-lived client cert
#   minted from the dev cluster CA) so it isolates the gateway forward + overlay
#   reachability + the live-migration seam. The operator-facing forward client
#   and its per-session credential are a separate concern and not exercised here;
#   this smoke is the reachability + seamless-migration proof for the gateway
#   data path itself.
#
# HOW SEAMLESSNESS IS MEASURED:
#   A continuous-traffic client (run inside the gateway node) holds ONE forward
#   session open and sends a monotonically increasing line every TICK seconds,
#   reading each line echoed back by the guest and recording its arrival time.
#   The guest runs a tiny echo server on the overlay (started by cloud-init).
#   During the live migration the client keeps sending; afterwards the smoke
#   reads the arrival log and asserts the LARGEST gap between consecutive echoes
#   stayed under GAP_THRESHOLD - a reboot or a blackholed path would show as a
#   long gap (or a dropped session) and fail loudly.
#
# GATEWAY PLACEMENT (dev stack):
#   The three-node dev stack runs an agent on every node; an ingress gateway is a
#   separate VM-less daemon that joins the same overlay substrate. This smoke
#   dedicates the third node to the gateway: it stops that node's agent and runs
#   otherix-gateway there, leaving the first two nodes as VM hosts for the
#   create + migrate. The multi-hop step bounces the guest between those two
#   hosts (a true three-distinct-host hop needs a fourth node the dev stack does
#   not provide). GW_HANDLE / the host node indices are overrideable so a larger
#   stack can place the gateway on a dedicated node and hop across three hosts.
#
# PREREQUISITES: a seeded three-node dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# plus python3 and openssl on the host, and python3 available on the gateway node.
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

NODE1="node-1"                         # VM host + live-migration source
NET="ingress-ovl"                      # dhcp-enabled overlay
SUBNET="10.93.0.0/24"                  # unlikely to clash with the dev stack
VM="ingress-vm"                        # the forwarded guest
GW_NAME="${GW_NAME:-ingress-gw}"       # the ingress gateway's cluster name
ECHO_PORT="${ECHO_PORT:-9000}"         # the guest echo server port the gateway dials
GW_LISTEN_PORT="${GW_LISTEN_PORT:-9443}" # the gateway control listener (mTLS) inside its node

# The gateway runs on the third dev node (its agent is stopped first); the VM
# hosts are the first two nodes. Override to place the gateway elsewhere.
GW_INDEX="${GW_INDEX:-3}"

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"      # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"            # seconds for the overlay to reconcile ready
GW_READY_WAIT="${GW_READY_WAIT:-180}"  # seconds for the gateway to report overlay-ready
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"  # seconds for the guest to lease an overlay IP and announce it
MIGRATE_WAIT="${MIGRATE_WAIT:-600}"    # seconds for a live migrate cutover (disk copy on TCG is slow)
TICK="${TICK:-0.2}"                    # seconds between continuous-traffic lines
GAP_THRESHOLD="${GAP_THRESHOLD:-10}"   # max tolerated seconds with no echo across a cutover (< heartbeat)

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx() { "$OTX" "$@"; }

GW_HANDLE="$(smoke_handle "$GW_INDEX")"

# net_ready NODE NET -> 0 when the overlay reconciled "ready" on NODE.
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

# --- workdir + cleanup -------------------------------------------------
WORKDIR="$(mktemp -d)"
GW_LAUNCHED=""
cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the VM/network/gateway up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  run_on "$GW_HANDLE" sudo pkill -f 'otherix-gateway serve' >/dev/null 2>&1 || true
  otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  otx node delete "$GW_NAME" --force >/dev/null 2>&1 || true
  # Bring the dedicated node's agent back so a subsequent run finds three hosts.
  if [ -n "$GW_LAUNCHED" ]; then
    info "restart the agent on the gateway node (best effort) so the stack returns to three hosts"
    run_on "$GW_HANDLE" sudo sh -c 'command -v systemctl >/dev/null && systemctl start otherix-agent' >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- the guest echo server cloud-config --------------------------------
# A first-boot runcmd writes a tiny python3 line-echo server bound to the overlay
# and launches it detached so it survives cloud-init exiting. Once the overlay
# NIC has an IPv4 the guest announces it on the serial console as
# "OTHERIX_GUEST_IP <ip>" (repeatedly), which the harness reads off the captured
# serial.log to learn the address the gateway must dial. python3 ships in the
# minimal cloud image (cloud-init depends on it), so no extra package is needed.
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

# --- the continuous-traffic client (runs inside the gateway node) ------
# Opens ONE forward session through the gateway's connect route presenting the
# control-plane-identity client cert, then sends a monotonic line every TICK
# seconds and records the arrival time of each echoed line. Writing and reading
# run on separate threads so a brief cutover stall never deadlocks the loop. It
# exits when the stop file appears (or the hard deadline passes) and prints the
# largest gap between consecutive echoes - the seamlessness metric.
CLIENT_PY="${WORKDIR}/forward_client.py"
cat >"$CLIENT_PY" <<'PYEOF'
import os, socket, ssl, sys, threading, time

gw_host, gw_port = sys.argv[1], int(sys.argv[2])
guest_ip, guest_port = sys.argv[3], sys.argv[4]
cert, key, ca = sys.argv[5], sys.argv[6], sys.argv[7]
arrivals_path, stop_path, max_secs, tick = sys.argv[8], sys.argv[9], float(sys.argv[10]), float(sys.argv[11])

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.load_cert_chain(cert, key)
ctx.load_verify_locations(ca)
# The gateway server cert identifies the gateway node, not the dialed address, so
# verify the chain to the cluster CA but skip hostname matching for the dev dial.
ctx.check_hostname = False

raw = socket.create_connection((gw_host, gw_port), timeout=30)
s = ctx.wrap_socket(raw, server_hostname=None)
s.sendall(("POST /v1/connect?guest_ip=%s&port=%s HTTP/1.1\r\nHost: gw\r\n\r\n" % (guest_ip, guest_port)).encode())

# Consume the status line + headers up to the blank line.
buf = b""
while b"\r\n\r\n" not in buf:
    d = s.recv(1)
    if not d:
        print("FORWARD_CLIENT no response before headers"); sys.exit(2)
    buf += d
status = buf.split(b"\r\n", 1)[0]
if b" 200" not in status:
    print("FORWARD_CLIENT bad status: %r" % status); sys.exit(3)

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

# Report the largest gap between consecutive echo arrivals.
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

# --- preconditions -----------------------------------------------------
echo "=== ingress-gateway smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v openssl >/dev/null || fail "openssl is required (mints the control-plane-identity client cert)"
command -v python3 >/dev/null || fail "python3 is required on the host"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
# The gateway runs on a linux node; pick the cross-build for that node's arch
# (the host may be macOS, whose binary cannot run there), building it on demand.
if [ -z "$GW_BIN" ]; then
  GW_ARCH="$(run_on "$GW_HANDLE" uname -m 2>/dev/null)"
  case "$GW_ARCH" in
    aarch64) GW_GOARCH=arm64 ;;
    x86_64)  GW_GOARCH=amd64 ;;
    *) fail "unsupported gateway node arch '${GW_ARCH:-unknown}'" ;;
  esac
  GW_BIN="bin/linux-${GW_GOARCH}/otherix-gateway"
  [ -x "$GW_BIN" ] || make "build-linux-${GW_GOARCH}" >/dev/null 2>&1 || true
fi
[ -x "$GW_BIN" ] || fail "otherix-gateway not found at '$GW_BIN' (run make build-linux-arm64 / build-linux-amd64, or set GW_BIN=...)"
[ -f .local/pki/cluster-ca.crt ] && [ -f .local/pki/cluster-ca.key ] \
  || fail "dev cluster CA not found at .local/pki/cluster-ca.{crt,key} (run make local-dev-start)"
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

# best-effort delete-first so a stale leftover from a prior run does not 409
otx vm delete "$VM" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
otx node delete "$GW_NAME" --force >/dev/null 2>&1 || true

# --- step 1: join an ingress gateway -----------------------------------
echo "=== step 1: join ingress gateway $GW_NAME ==="
# The gateway's control-plane URL: reuse the agent's, read from the dev agent
# config staged on the node (control_plane.url), so the smoke follows whatever
# endpoint the stack bootstrapped its agents against.
# shellcheck disable=SC2016  # $2 is an awk field, kept literal on purpose
GW_CP_URL="${GW_CP_URL:-$(run_on "$GW_HANDLE" awk '/^[ \t]*url:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)}"
[ -n "$GW_CP_URL" ] || fail "could not resolve the gateway control-plane URL (set GW_CP_URL=https://...:PORT)"
GW_NODE_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1")"
[ -n "$GW_NODE_IP" ] || fail "could not resolve the gateway node IP"

# The gateway joins the WireGuard mesh, so it needs a WG UDP advertised endpoint
# the VM-host agents dial for the handshake. Learn the inter-node WG subnet + port
# a peer agent advertises, then pick the gateway node's address on THAT subnet -
# the node also carries a WG overlay address (10.x) that is not reachable for the
# handshake, so selecting by the peer's subnet avoids advertising the wrong one.
# The gateway node's agent is stopped below, freeing UDP 51820 for the gateway.
# shellcheck disable=SC2016  # $2/$4 are awk fields, kept literal on purpose
PEER_WG_EP="$(run_on "$SMOKE_HANDLE_1" awk '/^[ \t]*advertised_endpoint:/{gsub(/"/,"",$2); print $2; exit}' /etc/otherix/agent.yaml 2>/dev/null)"
GW_WG_PORT="${GW_WG_PORT:-51820}"
GW_WG_IP=""
if [ -n "$PEER_WG_EP" ]; then
  WG_SUBNET_PREFIX="$(printf '%s' "${PEER_WG_EP%:*}" | cut -d. -f1-3)"   # e.g. 192.168.104
  GW_WG_PORT="${PEER_WG_EP##*:}"
  # shellcheck disable=SC2016
  GW_WG_IP="$(run_on "$GW_HANDLE" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1" \
    | grep -E "^${WG_SUBNET_PREFIX}\." | head -n1)"
fi
[ -n "$GW_WG_IP" ] || GW_WG_IP="$GW_NODE_IP"   # fall back to the discovered global IP
[ -n "$GW_WG_IP" ] || fail "could not resolve the gateway WireGuard endpoint IP"
info "gateway WireGuard endpoint: ${GW_WG_IP}:${GW_WG_PORT}"

# Mint a gateway join token through the operator CLI (the new --kind gateway).
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
# agent's shared ca.crt, which otherwise reads as partial cert state, and it
# also makes a re-run idempotent over a prior gateway identity.
run_on "$GW_HANDLE" sudo otherix-gateway bootstrap --force \
  --token "$GW_TOKEN" --ca-fingerprint "$GW_FP" \
  --cp-url "$GW_CP_URL" --node-name "$GW_NAME" \
  --advertised-endpoint "https://${GW_NODE_IP}:${GW_LISTEN_PORT}" \
  --wireguard-endpoint "${GW_WG_IP}:${GW_WG_PORT}" \
  --heartbeat-interval "${GW_HEARTBEAT_INTERVAL:-5s}" \
  --listen "0.0.0.0:${GW_LISTEN_PORT}" \
  || fail "otherix-gateway bootstrap failed"
run_on "$GW_HANDLE" sudo sh -c 'setsid nohup otherix-gateway serve >/var/log/otherix-gateway.log 2>&1 < /dev/null &'
GW_LAUNCHED=1
pass "ingress gateway $GW_NAME bootstrapped and serving on $GW_HANDLE"

# --- step 2: overlay + a forwarded guest -------------------------------
echo "=== step 2: dhcp overlay $NET + guest $VM ==="
otx network create "$NET" --type overlay --subnet "$SUBNET" --dhcp \
  || fail "network create $NET failed"
# Membership: the gateway must be a member of the overlay it forwards onto. The
# CP attaches a gateway to the overlays its forwarded guests use; wait for the
# gateway to report the overlay reconciled ready before forwarding.
deadline=$(( SECONDS + NET_WAIT )); ok=0
info "waiting for $NET to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do net_ready "$NODE1" "$NET" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NET did not reconcile ready on $NODE1 within ${NET_WAIT}s"

info "creating $VM on $NODE1 (echo server on :${ECHO_PORT})"
printf '%s' "$CLOUD_INIT" | otx vm create "$VM" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
  --vcpus 2 --memory-mb 2048 --user-data - \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create $VM did not reach running within ${CREATE_WAIT}s"
VMID="$(otx vm get "$VM" --output json | jq -r '.id')"
[[ "$VMID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve VM id (got '$VMID')"
pass "$VM running on $NODE1 (id=$VMID)"

# Wait for the gateway to report the overlay ready (the selection gate).
deadline=$(( SECONDS + GW_READY_WAIT )); ok=0
info "waiting for gateway $GW_NAME to report $NET ready (<= ${GW_READY_WAIT}s)"
while (( SECONDS < deadline )); do gw_overlay_ready && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "gateway $GW_NAME did not report $NET ready within ${GW_READY_WAIT}s"
pass "gateway $GW_NAME reports $NET converged (forwarding database programmed)"

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

# --- step 3: mint a control-plane-identity client + prove reachability --
echo "=== step 3: forward reachability through the gateway ==="
# A short-lived client cert with the control-plane identity (CN), signed by the
# dev cluster CA, satisfies the gateway's control-plane-identity gate on the
# connect route. The gateway verifies the chain to the cluster CA and the CN.
CLIENT_KEY="${WORKDIR}/cp-client.key"
CLIENT_CRT="${WORKDIR}/cp-client.crt"
CLIENT_CSR="${WORKDIR}/cp-client.csr"
openssl ecparam -name prime256v1 -genkey -noout -out "$CLIENT_KEY" 2>/dev/null \
  || fail "mint client key failed"
openssl req -new -key "$CLIENT_KEY" -subj "/CN=otherix-cp-replica" -out "$CLIENT_CSR" 2>/dev/null \
  || fail "mint client csr failed"
openssl x509 -req -in "$CLIENT_CSR" \
  -CA .local/pki/cluster-ca.crt -CAkey .local/pki/cluster-ca.key -CAcreateserial \
  -days 1 -out "$CLIENT_CRT" \
  -extfile <(printf 'extendedKeyUsage=clientAuth\n') 2>/dev/null \
  || fail "sign client cert with the cluster CA failed"

# Stage the client script + cert material + cluster CA into the gateway node.
CLIENT_PY_NODE="$(node_path "$GW_HANDLE" "$CLIENT_PY")"
CLIENT_KEY_NODE="$(node_path "$GW_HANDLE" "$CLIENT_KEY")"
CLIENT_CRT_NODE="$(node_path "$GW_HANDLE" "$CLIENT_CRT")"
CA_NODE="$(node_path "$GW_HANDLE" ".local/pki/cluster-ca.crt")"

# Reachability probe: a short session (no stop file, so it runs its full window)
# proves the path round-trips bytes through the gateway.
PROBE_OUT="$(run_on "$GW_HANDLE" python3 "$CLIENT_PY_NODE" \
  127.0.0.1 "$GW_LISTEN_PORT" "$GUEST_IP" "$ECHO_PORT" \
  "$CLIENT_CRT_NODE" "$CLIENT_KEY_NODE" "$CA_NODE" \
  /tmp/ingress-arrivals-probe.log /tmp/ingress-stop-never 8 "$TICK" 2>&1)" || true
echo "$PROBE_OUT" | grep -qE 'FORWARD_CLIENT echoes=[1-9]' \
  || fail "forward reachability probe got no echoes through the gateway: ${PROBE_OUT}"
pass "forward session reached the guest echo server through the gateway ($PROBE_OUT)"

# --- step 4: seamless live migration -----------------------------------
echo "=== step 4: live migrate $NODE1 -> $TARGET with the session held open ==="
STOP_FILE_NODE="/tmp/ingress-stop-1"
run_on "$GW_HANDLE" rm -f "$STOP_FILE_NODE" /tmp/ingress-arrivals.log >/dev/null 2>&1 || true
# Start the continuous-traffic client in the background (hard cap a bit above the
# migrate budget so it never outlives the test). It stops when the node-local
# stop file appears.
CLIENT_LOG="${WORKDIR}/client.out"
run_on "$GW_HANDLE" python3 "$CLIENT_PY_NODE" \
  127.0.0.1 "$GW_LISTEN_PORT" "$GUEST_IP" "$ECHO_PORT" \
  "$CLIENT_CRT_NODE" "$CLIENT_KEY_NODE" "$CA_NODE" \
  /tmp/ingress-arrivals.log "$STOP_FILE_NODE" "$(( MIGRATE_WAIT + 60 ))" "$TICK" >"$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
sleep 3   # let the session establish and a few echoes flow before the cutover

otx vm migrate "$VM" --node "$TARGET" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "live migrate $NODE1 -> $TARGET did not complete within ${MIGRATE_WAIT}s"
NOW_NODE="$(vm_node "$VM")"
[[ "$NOW_NODE" == *"$TARGET"* ]] || info "post-migrate current node reported as '$NOW_NODE'"
sleep 3   # let traffic flow on the target before stopping the client

# Stop the client and read the seamlessness metric.
run_on "$GW_HANDLE" touch "$STOP_FILE_NODE" >/dev/null 2>&1 || true
wait "$CLIENT_PID" 2>/dev/null || true
CLIENT_OUT="$(cat "$CLIENT_LOG" 2>/dev/null)"
echo "$CLIENT_OUT"
MAXGAP="$(grep -oE 'maxgap=[0-9.]+' <<<"$CLIENT_OUT" | head -n1 | cut -d= -f2)"
ECHOES="$(grep -oE 'echoes=[0-9]+' <<<"$CLIENT_OUT" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP" ] && [ -n "$ECHOES" ] || fail "continuous-traffic client did not report a metric: ${CLIENT_OUT}"
DROPPED="$(grep -oE 'dropped=[0-9]+' <<<"$CLIENT_OUT" | head -n1 | cut -d= -f2)"
(( ECHOES > 0 )) || fail "no echoes recorded across the migration - the session dropped"
[ "$DROPPED" = "0" ] || fail "the gateway session closed during the migration - not seamless (a cutover-time reset)"
awk -v g="$MAXGAP" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "session stalled ${MAXGAP}s across the cutover (>= ${GAP_THRESHOLD}s) - not seamless"
pass "session stayed alive across live migration (echoes=$ECHOES, max gap ${MAXGAP}s < ${GAP_THRESHOLD}s)"

# --- step 5: multi-hop - migrate again, session still survives ---------
echo "=== step 5: multi-hop migrate $TARGET -> $NODE1 (no stacked blackhole) ==="
STOP_FILE2_NODE="/tmp/ingress-stop-2"
run_on "$GW_HANDLE" rm -f "$STOP_FILE2_NODE" /tmp/ingress-arrivals2.log >/dev/null 2>&1 || true
CLIENT_LOG2="${WORKDIR}/client2.out"
run_on "$GW_HANDLE" python3 "$CLIENT_PY_NODE" \
  127.0.0.1 "$GW_LISTEN_PORT" "$GUEST_IP" "$ECHO_PORT" \
  "$CLIENT_CRT_NODE" "$CLIENT_KEY_NODE" "$CA_NODE" \
  /tmp/ingress-arrivals2.log "$STOP_FILE2_NODE" "$(( MIGRATE_WAIT + 60 ))" "$TICK" >"$CLIENT_LOG2" 2>&1 &
CLIENT_PID2=$!
sleep 3
otx vm migrate "$VM" --node "$NODE1" --wait --wait-timeout "${MIGRATE_WAIT}s" \
  || fail "multi-hop migrate $TARGET -> $NODE1 did not complete within ${MIGRATE_WAIT}s"
sleep 3
run_on "$GW_HANDLE" touch "$STOP_FILE2_NODE" >/dev/null 2>&1 || true
wait "$CLIENT_PID2" 2>/dev/null || true
CLIENT_OUT2="$(cat "$CLIENT_LOG2" 2>/dev/null)"
echo "$CLIENT_OUT2"
MAXGAP2="$(grep -oE 'maxgap=[0-9.]+' <<<"$CLIENT_OUT2" | head -n1 | cut -d= -f2)"
ECHOES2="$(grep -oE 'echoes=[0-9]+' <<<"$CLIENT_OUT2" | head -n1 | cut -d= -f2)"
[ -n "$MAXGAP2" ] && [ -n "$ECHOES2" ] || fail "multi-hop client did not report a metric: ${CLIENT_OUT2}"
DROPPED2="$(grep -oE 'dropped=[0-9]+' <<<"$CLIENT_OUT2" | head -n1 | cut -d= -f2)"
(( ECHOES2 > 0 )) || fail "no echoes recorded across the multi-hop migration - the session dropped"
[ "$DROPPED2" = "0" ] || fail "the gateway session closed during the multi-hop migration - not seamless"
awk -v g="$MAXGAP2" -v t="$GAP_THRESHOLD" 'BEGIN{exit !(g < t)}' \
  || fail "session stalled ${MAXGAP2}s across the multi-hop cutover (>= ${GAP_THRESHOLD}s)"
pass "session survived the multi-hop migration (echoes=$ECHOES2, max gap ${MAXGAP2}s < ${GAP_THRESHOLD}s)"

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the VM + overlay + gateway and restarts the gateway
# node's agent, so a re-run finds the stack back at three hosts.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== ingress-gateway smoke PASSED ===${NC}"
