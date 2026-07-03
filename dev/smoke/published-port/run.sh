#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# Published-LB-port smoke - proves the agent-side published-listener reconciler
# binds a REAL kernel socket on a gateway-role node from control-plane-declared
# state, and releases it on unpublish. This tier has no data path (an accepted
# connection is immediately closed / reset), so the smoke asserts the socket
# lifecycle, not traffic:
#
#   - with a node holding the gateway role, `otherix lb create <lb> --port P
#     --selector ... --publish --publish-port PP` makes that node begin accepting
#     on PP: a raw TCP connect from a PEER node SUCCEEDS and is immediately closed
#     by the peer (connect ok + EOF/RST, never a real payload) - no Otherix
#     tooling on the client, just a socket;
#   - a control port that was never published is REFUSED on the same node, so the
#     reconciler binds only the declared port, not everything;
#   - `otherix lb update <lb> --no-publish` makes the node stop accepting on PP (a
#     later connect is REFUSED) - the reconciler reaps the listener.
#
# This is the real-agent counterpart to the mock round-trip the api e2e suite
# covers: it exercises the real Listen(":PP") on a Linux agent plus the reconciler
# goroutine driven by the real heartbeat, which a mock agent cannot. The
# traffic-forwarding smoke (a raw external client reaches a backend VM) belongs to
# the datapath tier and is separate.
#
# PREREQUISITES: a dev stack seeded from the CURRENT tree - the agents must carry
# the published-listener reconciler. Run `make local-dev-deploy` (rebuild+restart
# api+agents+CLI, state preserved) before this smoke, then:
#
#   make smoke-published-port
#
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"

# A unique per-run suffix keeps the LB/label re-runnable without a clean restart.
RUN="${RUN:-$(date +%H%M%S)-${RANDOM}}"

GW_INDEX="${GW_INDEX:-3}"                    # the dev node that takes the gateway role
GW_NODE="${GW_NODE:-node-3}"                 # its CP name
PROBE_INDEX="${PROBE_INDEX:-1}"             # a peer node that dials the gateway

LB_NAME="publb-${RUN}"
LABEL_KEY="app"
LABEL_VAL="publb-${RUN}"
BACKEND_PORT="${BACKEND_PORT:-9000}"        # the LB traffic port (no backend needed here)
# A high, unlikely-to-clash published port, plus a never-published control port.
PUB_PORT="${PUB_PORT:-31$(printf '%03d' $((RANDOM % 1000)))}"
CTRL_PORT="${CTRL_PORT:-$((PUB_PORT + 1))}"

BIND_WAIT="${BIND_WAIT:-90}"                # seconds for the gateway heartbeat to bind/reap the port
PROBE_TIMEOUT="${PROBE_TIMEOUT:-3}"         # per-connect timeout inside the probe

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }
otx()  { "$OTX" "$@"; }

# node_ip <index> -> the node's first global IPv4 (its inter-node ingress
# address), the address a peer node dials to reach a published port.
node_ip() {
  run_on "$(smoke_handle "$1")" sh -c "ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -n1"
}

# probe <exec_index> <ip> <port> -> prints one classification word:
#   refused        connect was refused (no listener bound)
#   unreachable    connect failed for another reason (routing/timeout)
#   accepted-eof   connect ok, peer closed cleanly (accept-then-close stub)
#   accepted-reset connect ok, peer reset the connection (accept-then-close stub)
#   accepted-open  connect ok but the peer kept the connection open (NOT our stub)
#   accepted-data  connect ok and the peer sent bytes (NOT our stub)
# The probe runs ON a peer node so it dials the gateway over the inter-node IP,
# with no Otherix tooling - a bare socket, the whole point of a published port.
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
try:
    data = s.recv(16)
except socket.timeout:
    print("accepted-open")
except OSError:
    print("accepted-reset")
else:
    print("accepted-eof" if data == b"" else "accepted-data")
finally:
    s.close()
PY
}

# wait_probe <exec_index> <ip> <port> <want:accepted|refused> -> block until the
# probe reaches the wanted state, up to BIND_WAIT. The final classification is
# left in $LAST_PROBE for the caller to inspect.
LAST_PROBE=""
wait_probe() {
  local idx="$1" ip="$2" port="$3" want="$4" deadline=$(( SECONDS + BIND_WAIT ))
  while (( SECONDS < deadline )); do
    LAST_PROBE="$(probe "$idx" "$ip" "$port")"
    case "$want" in
      accepted) [[ "$LAST_PROBE" == accepted-* ]] && return 0 ;;
      refused)  [[ "$LAST_PROBE" == refused    ]] && return 0 ;;
    esac
    sleep 3
  done
  return 1
}

# --- cleanup -----------------------------------------------------------
GW_ENABLED_BY_US=0
cleanup() {
  local rc=$?
  # --force skips the confirmation prompt (a non-TTY delete aborts without it).
  otx lb delete "$LB_NAME" --force >/dev/null 2>&1 || true
  if (( GW_ENABLED_BY_US )); then
    otx node gateway disable "$GW_NODE" >/dev/null 2>&1 || true
  fi
  exit $rc
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== published-port smoke (run ${RUN}) ==="
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
"$OTX" lb create --help 2>/dev/null | grep -q -- '--publish' \
  || fail "this otherix build has no 'lb --publish' (rebuild from the current tree: make local-dev-deploy)"
"$OTX" node gateway --help >/dev/null 2>&1 \
  || fail "this otherix build has no 'node gateway' command"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"

GW_IP="$(node_ip "$GW_INDEX")"
[ -n "$GW_IP" ] || fail "could not resolve node-${GW_INDEX} inter-node IP"
info "gateway node ${GW_NODE} at ${GW_IP}; published port ${PUB_PORT}, control port ${CTRL_PORT}"
info "probing from node-${PROBE_INDEX}"

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

# --- step 3: publish a port on a new LB ----------------------------------
echo "=== step 3: otherix lb create ${LB_NAME} --publish --publish-port ${PUB_PORT} ==="
otx lb create "$LB_NAME" --port "$BACKEND_PORT" --selector "${LABEL_KEY}=${LABEL_VAL}" \
  --publish --publish-port "$PUB_PORT" \
  || fail "lb create ${LB_NAME} (published) failed"
# The API view must reflect the published port immediately.
got_port="$(otx lb get "$LB_NAME" --output json 2>/dev/null | jq -r '.published_port // empty')"
[ "$got_port" = "$PUB_PORT" ] || fail "lb get published_port = '${got_port}', want ${PUB_PORT}"
pass "LB published on port ${PUB_PORT} (API view confirms)"

# --- step 4: the gateway agent binds the real socket --------------------
echo "=== step 4: gateway binds ${PUB_PORT} (accept-then-close) ==="
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" accepted; then
  case "$LAST_PROBE" in
    accepted-eof|accepted-reset) pass "gateway accepts on ${PUB_PORT} and closes (${LAST_PROBE}) - real socket bound" ;;
    *) fail "port ${PUB_PORT} was ${LAST_PROBE}, want accepted-eof/accepted-reset (accept-then-close stub, no payload)" ;;
  esac
else
  fail "gateway never bound ${PUB_PORT} within ${BIND_WAIT}s (last: ${LAST_PROBE:-none})"
fi

# --- step 5: control port stays refused (only declared ports bind) ------
echo "=== step 5: control port ${CTRL_PORT} (never published -> refused) ==="
r="$(probe "$PROBE_INDEX" "$GW_IP" "$CTRL_PORT")"
[ "$r" = "refused" ] || fail "control port ${CTRL_PORT} was ${r}, want refused (only the declared port must bind)"
pass "control port ${CTRL_PORT} refused (reconciler binds only declared ports)"

# --- step 6: unpublish -> the gateway reaps the listener ----------------
echo "=== step 6: otherix lb update ${LB_NAME} --no-publish ==="
otx lb update "$LB_NAME" --no-publish || fail "lb update ${LB_NAME} --no-publish failed"
if wait_probe "$PROBE_INDEX" "$GW_IP" "$PUB_PORT" refused; then
  pass "gateway stopped accepting on ${PUB_PORT} after unpublish (listener reaped)"
else
  fail "gateway still accepting on ${PUB_PORT} ${BIND_WAIT}s after unpublish (last: ${LAST_PROBE:-none})"
fi

echo
echo "${GREEN}=== published-port smoke PASSED ===${NC}"
