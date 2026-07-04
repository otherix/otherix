#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Published-LB-port TRAFFIC smoke - proves the full L4 datapath: a raw external
# client, with NO Otherix tooling, reaches a guest VM THROUGH a published port on
# a gateway-role node. This is the datapath tier's end-to-end proof, not just the
# socket lifecycle:
#
#   - a gateway-role node (co-located: it keeps its full hypervisor agent and
#     gains the gateway role via `otherix node gateway enable`) binds a real
#     public listener on the published port from control-plane-declared state;
#   - TRAFFIC (the core proof): a bare python3 socket on a PEER node dials
#     gateway_ip:published_port and reads back the BACKEND guest's identity
#     string - the gateway spliced the raw connection over the overlay to the
#     labelled backend VM (which runs on a DIFFERENT node), with the control
#     plane out of the data path;
#   - SOURCE-CIDR ACL: restricting the listener to a client CIDR that excludes
#     the peer closes its connections with NO data (the data-plane ACL, fail
#     closed); restoring an inclusive CIDR lets traffic flow again;
#   - BACKEND DISABLE: powering the backend off (its observed phase leaves
#     running) leaves the port bound but every new connection closed with no
#     data - no eligible backend, never a mis-route to a stale target;
#   - UNPUBLISH: `otherix lb update <lb> --no-publish` reaps the listener and the
#     port goes back to refused.
#
# GATEWAY / BACKEND PLACEMENT (dev stack): all three dev nodes run a full
# hypervisor agent. This smoke turns node-3 into a CO-LOCATED gateway with
# `node gateway enable` (its agent keeps running - no stop/swap), hosts the
# backend VM on node-1, and dials from node-2. The gateway becomes a member of
# the backend's overlay automatically: the control-plane gateway coverage
# reconcile places a membership on every gateway-role node for any overlay that
# carries a VM NIC, which programs the gateway's overlay veth + forwarding
# database. So there is no explicit "join the gateway to the overlay" step - the
# backend VM living on the overlay is what activates it, and the smoke waits for
# node-3 to report the overlay converged before dialing.
#
# PREREQUISITES: a dev stack seeded from the CURRENT tree - the agents must carry
# the published-listener datapath. Run `make local-dev-deploy` (rebuild+restart
# api+agents+CLI, state preserved) before this smoke, then:
#
#   make smoke-published-port
#
set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"

# A unique per-run suffix keeps the LB/VM/network/label re-runnable without a
# clean restart.
RUN="${RUN:-$(date +%H%M%S)-${RANDOM}}"

GW_INDEX="${GW_INDEX:-3}"                    # the dev node that takes the gateway role
GW_NODE="${GW_NODE:-node-3}"                 # its CP name
BACKEND_INDEX="${BACKEND_INDEX:-1}"         # the node that hosts the backend VM
BACKEND_NODE="${BACKEND_NODE:-node-1}"      # its CP name
PROBE_INDEX="${PROBE_INDEX:-2}"             # a peer node that dials the gateway (the raw client)

LB_NAME="publb-${RUN}"
NET="publb-ovl-${RUN}"                       # dhcp-enabled overlay (unique per run)
SUBNET="${SUBNET:-10.98.0.0/24}"             # unlikely to clash with the dev stack
VM_BE="publb-be-${RUN}"                      # the backend VM (identity server)
LABEL_KEY="app"
LABEL_VAL="publb-${RUN}"
BACKEND_PORT="${BACKEND_PORT:-9000}"         # the guest TCP port the backend serves on
# A high, unlikely-to-clash published port.
PUB_PORT="${PUB_PORT:-31$(printf '%03d' $((RANDOM % 1000)))}"

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"
ARCH="${ARCH:-${SMOKE_ARCH}}"

CREATE_WAIT="${CREATE_WAIT:-600}"           # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"                 # seconds for the overlay to reconcile ready on the host
GW_READY_WAIT="${GW_READY_WAIT:-180}"       # seconds for the gateway to converge the overlay (coverage reconcile ~30s + a heartbeat)
GUEST_IP_WAIT="${GUEST_IP_WAIT:-600}"       # seconds for the guest to lease its overlay IP and start the server
OP_WAIT="${OP_WAIT:-180}"                   # seconds for an async poweroff task to reach terminal
PHASE_WAIT="${PHASE_WAIT:-120}"             # seconds for the CP-projected status.phase to leave running
# BIND_WAIT covers control-plane propagation to a LIVE listener: the first splice
# after publish additionally waits on the gateway coverage reconcile (~30s) plus a
# heartbeat, and an ACL / backend-set change reaches the live listener within a
# heartbeat (5s in dev). 120s leaves ample headroom without masking a real hang.
BIND_WAIT="${BIND_WAIT:-120}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-4}"         # per-connect timeout inside the probe

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { SMOKE_FAILED=1; echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx()  { "$OTX" "$@"; }

# node_ip <index> -> the node's first global IPv4 (its inter-node address), the
# address a peer node dials to reach a published port, and the source address the
# gateway sees for the raw client.
node_ip() {
  run_on "$(smoke_handle "$1")" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1"
}

# net_ready NODE -> 0 when the overlay reconciled "ready" on NODE. For the gateway
# this is the signal that its overlay veth + forwarding database are programmed
# (it holds a coverage membership), so the datapath can dial a backend over it.
net_ready() {
  [[ "$(otx network get "$NET" --output json 2>/dev/null \
      | jq -r --arg n "$1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
      == "ready" ]]
}

# wait_net_ready NODE SECONDS -> block until the overlay reconciled ready on NODE.
wait_net_ready() {
  local node="$1" deadline=$(( SECONDS + $2 ))
  while (( SECONDS < deadline )); do net_ready "$node" && return 0; sleep 3; done
  return 1
}

# vm_phase NAME -> the CP-observed status.phase ("" if the VM is gone).
vm_phase() { otx vm get "$1" --output json 2>/dev/null | jq -r '.status.phase' 2>/dev/null || true; }

# wait_phase_left_running NAME -> block until NAME's observed phase is a non-empty
# value other than running (the poweroff converged; the CP now drops it from the
# eligible backend set).
wait_phase_left_running() {
  local name="$1" deadline=$(( SECONDS + PHASE_WAIT )) got
  info "waiting for $name observed phase to leave running (<= ${PHASE_WAIT}s)"
  while (( SECONDS < deadline )); do
    got="$(vm_phase "$name")"
    [[ -n "$got" && "$got" != "running" ]] && { pass "$name phase=$got (left running)"; return 0; }
    sleep 2
  done
  fail "$name still 'running' after ${PHASE_WAIT}s (poweroff did not converge)"
}

# lb_backend_count -> the number of resolved backends the CP currently reports for
# the LB (the eligible set the gateway is splicing to).
lb_backend_count() { otx lb get "$LB_NAME" --output json 2>/dev/null | jq -r '.backends | length' 2>/dev/null || echo 0; }

# wait_lb_backends N -> block until the LB reports at least N resolved backends.
wait_lb_backends() {
  local want="$1" deadline=$(( SECONDS + GW_READY_WAIT )) got
  info "waiting for the LB to resolve >= ${want} backend(s) (<= ${GW_READY_WAIT}s)"
  while (( SECONDS < deadline )); do
    got="$(lb_backend_count)"
    (( got >= want )) && { pass "LB reports ${got} resolved backend(s)"; return 0; }
    sleep 3
  done
  return 1
}

# probe <exec_index> <ip> <port> -> prints one classification line:
#   refused              connect was refused (no listener bound)
#   unreachable          connect failed for another reason (routing/timeout)
#   accepted-eof         connect ok, peer closed cleanly with no bytes
#   accepted-reset       connect ok, peer reset before any bytes
#   accepted-open        connect ok, peer held the connection open with no bytes
#   accepted-data <str>  connect ok and the peer sent bytes; <str> is the first
#                        line (the spliced backend's identity)
# The probe runs ON a peer node so it dials the gateway over the inter-node IP with
# no Otherix tooling - a bare socket, the whole point of a published port.
probe() {
  run_on "$(smoke_handle "$1")" python3 - "$2" "$3" "$PROBE_TIMEOUT" <<'PY'
import socket, sys
ip, port, tmo = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
try:
    s = socket.create_connection((ip, port), timeout=tmo)
except ConnectionRefusedError:
    print("refused"); sys.exit(0)
except OSError:
    print("unreachable"); sys.exit(0)
s.settimeout(tmo)
buf = b""
cls = None
try:
    while b"\n" not in buf and len(buf) < 256:
        chunk = s.recv(256)
        if not chunk:
            break
        buf += chunk
except socket.timeout:
    cls = "accepted-open"
except OSError:
    cls = "accepted-reset"
finally:
    try:
        s.close()
    except OSError:
        pass
if buf:
    line = buf.split(b"\n", 1)[0].decode(errors="replace").strip()
    print("accepted-data " + (line if line else "<empty>"))
elif cls:
    print(cls)
else:
    print("accepted-eof")
PY
}

# wait_probe <exec_index> <ip> <port> <want> -> block until the probe reaches the
# wanted state, up to BIND_WAIT. The final classification is left in $LAST_PROBE.
# Wanted states:
#   data      accepted-data (the splice delivered the backend identity)
#   nodata    a closed-without-data state (refused / accepted-eof / accepted-reset)
#             - the ACL blocked, no backend was eligible, or the port is unbound
#   refused   the port is refused (no listener bound)
LAST_PROBE=""
wait_probe() {
  local idx="$1" ip="$2" port="$3" want="$4" deadline=$(( SECONDS + BIND_WAIT ))
  while (( SECONDS < deadline )); do
    LAST_PROBE="$(probe "$idx" "$ip" "$port")"
    case "$want" in
      data)    [[ "$LAST_PROBE" == accepted-data\ * ]] && return 0 ;;
      nodata)  [[ "$LAST_PROBE" == refused || "$LAST_PROBE" == accepted-eof || "$LAST_PROBE" == accepted-reset ]] && return 0 ;;
      refused) [[ "$LAST_PROBE" == refused ]] && return 0 ;;
    esac
    sleep 3
  done
  return 1
}

# --- scratch + the backend guest cloud-config --------------------------
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

# The backend guest cloud-config: a first-boot runcmd writes a tiny python3 TCP
# server bound to BACKEND_PORT that answers every connection with its identity
# (VM_BE) and closes - server-speaks-first, so a raw spliced connection reads the
# identity back without sending anything. A second script announces the guest's
# leased overlay IP on /dev/console (the kernel's active console maps to the
# serial device the host captures - ttyAMA0 on arm64, ttyS0 on amd64 - which a
# fixed tty name would get wrong across arches), which the harness reads off the
# captured serial.log to prove the guest booted and configured its NIC.
CI_FILE="${WORKDIR}/cloud-init.yaml"
cat >"$CI_FILE" <<EOF
#cloud-config
write_files:
  - path: /usr/local/bin/otherix-identity-server.py
    permissions: '0755'
    content: |
      import socket
      IDENTITY = "${VM_BE}"
      s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
      s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
      s.bind(('0.0.0.0', ${BACKEND_PORT}))
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
  - [ sh, -c, "setsid nohup python3 /usr/local/bin/otherix-identity-server.py >/dev/null 2>&1 < /dev/null &" ]
  - [ sh, -c, "setsid nohup /usr/local/bin/otherix-announce-ip.sh >/dev/null 2>&1 < /dev/null &" ]
EOF

# --- cleanup -----------------------------------------------------------
GW_ENABLED_BY_US=0
cleanup() {
  if [ -n "${KEEP_FAILED:-}" ] && [ -n "${SMOKE_FAILED:-}" ]; then
    echo "--- KEEP_FAILED set and the run failed: leaving the LB/VM/network/gateway up for inspection ---"
    return
  fi
  echo "--- cleanup ---"
  # --force skips the confirmation prompt (a non-TTY delete aborts without it).
  otx lb delete "$LB_NAME" --force >/dev/null 2>&1 || true
  otx vm delete "$VM_BE" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  if (( GW_ENABLED_BY_US )); then
    otx node gateway disable "$GW_NODE" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== published-port traffic smoke (run ${RUN}) ==="
command -v jq >/dev/null || fail "jq is required"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" lb create --help 2>/dev/null | grep -q -- '--publish' \
  || fail "this otherix build has no 'lb --publish' (rebuild from the current tree: make local-dev-deploy)"
"$OTX" node gateway --help >/dev/null 2>&1 \
  || fail "this otherix build has no 'node gateway' command"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
for n in "$GW_NODE" "$BACKEND_NODE"; do
  st="$(otx node get "$n" --output json 2>/dev/null | jq -r '.status' || true)"
  [[ "$st" == "ready" ]] || fail "$n not ready (got '${st:-none}'); run make local-dev-start"
done

GW_IP="$(node_ip "$GW_INDEX")"
[ -n "$GW_IP" ] || fail "could not resolve node-${GW_INDEX} inter-node IP"
PROBE_IP="$(node_ip "$PROBE_INDEX")"
[ -n "$PROBE_IP" ] || fail "could not resolve the probe node-${PROBE_INDEX} inter-node IP"
info "gateway ${GW_NODE} at ${GW_IP}; published port ${PUB_PORT}; backend on ${BACKEND_NODE}"
info "raw client dials from node-${PROBE_INDEX} (source ${PROBE_IP})"

# best-effort delete-first so stale leftovers from a prior run do not clash
otx lb delete "$LB_NAME" --force >/dev/null 2>&1 || true
otx vm delete "$VM_BE" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true

# --- step 1: baseline - the port is not bound before publish ------------
echo "=== step 1: baseline (port not yet published -> refused) ==="
r="$(probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT")"
[ "$r" = "refused" ] || fail "baseline: port ${PUB_PORT} was ${r}, want refused (nothing should be bound yet)"
pass "port ${PUB_PORT} refused before publish"

# --- step 2: give node-3 the gateway role -------------------------------
echo "=== step 2: otherix node gateway enable ${GW_NODE} ==="
if otx node get "$GW_NODE" --output json 2>/dev/null | jq -e '.roles // [] | index("gateway")' >/dev/null 2>&1; then
  info "${GW_NODE} already holds the gateway role"
else
  otx node gateway enable "$GW_NODE" || fail "node gateway enable ${GW_NODE} failed"
  GW_ENABLED_BY_US=1
  pass "gateway role enabled on ${GW_NODE}"
fi

# --- step 3: overlay + one labelled backend VM --------------------------
echo "=== step 3: dhcp overlay ${NET} + backend ${VM_BE} on ${BACKEND_NODE} (${LABEL_KEY}=${LABEL_VAL}) ==="
otx network create "$NET" --type overlay --subnet "$SUBNET" --dhcp \
  || fail "network create ${NET} failed"
wait_net_ready "$BACKEND_NODE" "$NET_WAIT" \
  || fail "${NET} did not reconcile ready on ${BACKEND_NODE} within ${NET_WAIT}s"
pass "${NET} reconciled ready on ${BACKEND_NODE}"

info "creating ${VM_BE} on ${BACKEND_NODE} (identity server on :${BACKEND_PORT})"
otx vm create "$VM_BE" \
  --image-url "$IMAGE_URL" --arch "$ARCH" --node "$BACKEND_NODE" --network "$NET" \
  --vcpus 2 --memory-mb 2048 \
  --label "${LABEL_KEY}=${LABEL_VAL}" \
  --user-data "$CI_FILE" --network-config "$NC_FILE" \
  --wait --wait-timeout "${CREATE_WAIT}s" \
  || fail "vm create ${VM_BE} did not reach running within ${CREATE_WAIT}s"
VM_BE_ID="$(otx vm get "$VM_BE" --output json | jq -r '.id')"
[[ "$VM_BE_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "could not resolve ${VM_BE} id (got '${VM_BE_ID}')"
pass "${VM_BE} running on ${BACKEND_NODE} (id=${VM_BE_ID})"

# Wait for the guest to announce its overlay IP (proves it booted, configured its
# NIC, and - same runcmd - started the identity server).
BE_STATE="$(smoke_state "$BACKEND_INDEX")"; BE_HANDLE="$(smoke_handle "$BACKEND_INDEX")"
deadline=$(( SECONDS + GUEST_IP_WAIT )); GUEST_IP=""
info "waiting for ${VM_BE} to announce its overlay IP (<= ${GUEST_IP_WAIT}s)"
while (( SECONDS < deadline )); do
  GUEST_IP="$(run_on "$BE_HANDLE" sudo grep -oE 'OTHERIX_GUEST_IP [0-9.]+' \
      "${BE_STATE}/vms/${VM_BE_ID}/serial.log" 2>/dev/null | awk '{print $2}' | tail -n1)" || true
  [ -n "$GUEST_IP" ] && break
  sleep 5
done
[ -n "$GUEST_IP" ] || fail "${VM_BE} never announced an overlay IP within ${GUEST_IP_WAIT}s"
pass "${VM_BE} overlay IP is ${GUEST_IP} (booted; identity server started)"

# The gateway becomes an overlay member automatically (coverage reconcile): wait
# for node-3 to report the overlay converged so its veth + forwarding database are
# programmed before we dial a backend over it.
info "waiting for ${GW_NODE} to converge ${NET} as a gateway (<= ${GW_READY_WAIT}s)"
wait_net_ready "$GW_NODE" "$GW_READY_WAIT" \
  || fail "${GW_NODE} did not report ${NET} ready within ${GW_READY_WAIT}s (gateway overlay membership/convergence)"
pass "${GW_NODE} converged ${NET} (overlay veth + forwarding database programmed)"

# --- step 4: publish the LB on the gateway ------------------------------
echo "=== step 4: otherix lb create ${LB_NAME} --publish --publish-port ${PUB_PORT} ==="
otx lb create "$LB_NAME" --port "$BACKEND_PORT" --selector "${LABEL_KEY}=${LABEL_VAL}" \
  --publish --publish-port "$PUB_PORT" \
  || fail "lb create ${LB_NAME} (published) failed"
got_port="$(otx lb get "$LB_NAME" --output json 2>/dev/null | jq -r '.published_port // empty')"
[ "$got_port" = "$PUB_PORT" ] || fail "lb get published_port = '${got_port}', want ${PUB_PORT}"
wait_lb_backends 1 || fail "the LB never resolved the backend ${VM_BE} within ${GW_READY_WAIT}s"
pass "LB published on port ${PUB_PORT} with backend ${VM_BE} resolved"

# --- step 5: TRAFFIC - a raw client reaches the backend through the port
echo "=== step 5: raw client (node-${PROBE_INDEX}) dials ${GW_IP}:${PUB_PORT} -> reads ${VM_BE} identity ==="
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" data; then
  ident="${LAST_PROBE#accepted-data }"
  [ "$ident" = "$VM_BE" ] \
    || fail "raw client read identity '${ident}', want '${VM_BE}' (spliced to the wrong backend?)"
  pass "raw client reached backend ${VM_BE} through ${GW_IP}:${PUB_PORT} (identity='${ident}') - the datapath splices to the guest"
else
  fail "raw client never read the backend identity within ${BIND_WAIT}s (last: ${LAST_PROBE:-none})"
fi

# --- step 6: source-CIDR ACL excludes the client ------------------------
echo "=== step 6: restrict to a CIDR excluding node-${PROBE_INDEX} -> connections closed with no data ==="
EXCLUDE_CIDR="203.0.113.7/32"                # RFC 5737 TEST-NET-3, never the probe node
otx lb update "$LB_NAME" --source-cidr "$EXCLUDE_CIDR" \
  || fail "lb update ${LB_NAME} --source-cidr ${EXCLUDE_CIDR} failed"
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" nodata; then
  [[ "$LAST_PROBE" == accepted-data\ * ]] \
    && fail "the excluded client still received backend data (${LAST_PROBE}) - the source-CIDR ACL did not fail closed"
  pass "excluded client closed with no data (${LAST_PROBE}) - the source-CIDR ACL fails closed"
else
  fail "the excluded client still received backend data after ${BIND_WAIT}s (last: ${LAST_PROBE:-none}) - ACL did not take"
fi

echo "=== step 6b: restore an inclusive CIDR (${PROBE_IP}/32) -> data flows again ==="
otx lb update "$LB_NAME" --source-cidr "${PROBE_IP}/32" \
  || fail "lb update ${LB_NAME} --source-cidr ${PROBE_IP}/32 failed"
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" data; then
  ident="${LAST_PROBE#accepted-data }"
  [ "$ident" = "$VM_BE" ] || fail "after restoring the CIDR the client read '${ident}', want '${VM_BE}'"
  pass "restoring an inclusive CIDR let the client reach ${VM_BE} again (identity='${ident}')"
else
  fail "the client did not regain access after an inclusive CIDR within ${BIND_WAIT}s (last: ${LAST_PROBE:-none})"
fi

# --- step 7: backend disable -> no eligible backend, no mis-route -------
echo "=== step 7: poweroff ${VM_BE}; a new raw connection finds no eligible backend ==="
otx vm poweroff "$VM_BE" --wait --wait-timeout "${OP_WAIT}s" \
  || fail "poweroff ${VM_BE} did not complete within ${OP_WAIT}s"
wait_phase_left_running "$VM_BE"
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" nodata; then
  [[ "$LAST_PROBE" == accepted-data\ * ]] \
    && fail "a raw connection still received data after the backend was powered off (${LAST_PROBE}) - mis-routed to a stale target"
  pass "with no eligible backend the connection closed with no data (${LAST_PROBE}) - never mis-routed"
else
  fail "a raw connection still received backend data ${BIND_WAIT}s after poweroff (last: ${LAST_PROBE:-none})"
fi

# --- step 8: unpublish -> the gateway reaps the listener ----------------
echo "=== step 8: otherix lb update ${LB_NAME} --no-publish ==="
otx lb update "$LB_NAME" --no-publish || fail "lb update ${LB_NAME} --no-publish failed"
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" refused; then
  pass "gateway stopped accepting on ${PUB_PORT} after unpublish (listener reaped)"
else
  fail "gateway still accepting on ${PUB_PORT} ${BIND_WAIT}s after unpublish (last: ${LAST_PROBE:-none})"
fi

# --- teardown ----------------------------------------------------------
# The EXIT trap deletes the LB + VM + network and disables the gateway role.
echo "=== teardown (handled by the exit trap) ==="
echo
echo "${GREEN}=== published-port traffic smoke PASSED ===${NC}"
echo "  a raw external client reached a backend guest THROUGH the published port"
echo "  a source-CIDR ACL excluding the client fails closed; restoring it lets traffic flow"
echo "  a powered-off backend leaves no eligible target - the connection is never mis-routed"
echo "  unpublish reaps the listener (the port goes back to refused)"
