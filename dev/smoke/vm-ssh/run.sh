#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Andrei Taranik
#
# VM SSH-ingress smoke - proves a real SSH login lands on a guest through the
# control-plane relay, with no inbound path to the guest and no exposed guest
# IP, driven entirely through the operator CLIs as a real operator would.
#
# The SSH bytes are end-to-end between the client and the guest sshd; the
# control plane mints a short-lived guest certificate (the guest trusts the
# cluster SSH user-CA, provisioned at create) and splices the session over a
# WebSocket relay to the agent that owns the VM, which pipes it to the guest's
# sshd on the guest network.
#
# What it proves, end to end, as a real operator (CLI only):
#   - operator path: `otherix ssh <vm> --login <user>` runs a command IN the
#     guest and the guest echoes back a unique nonce over the tunnel (the
#     load-bearing CP->agent->guest seam: a real guest login);
#   - external path: `otherix ssh-grant create` mints a shareable bundle, the
#     thin `otherix-ssh add` connector imports it in an isolated HOME, and
#     `ssh <vm>.<suffix>` reaches the guest using only the grant token;
#   - incremental: `otherix ssh-grant add-vm` widens the grant to a second VM
#     and `ssh <vm2>.<suffix>` works with NO re-import (the connector resolves
#     the scope at connect time);
#   - revocation: `otherix ssh-grant revoke` cuts access and the next
#     `ssh <vm>.<suffix>` is rejected.
#
# Network: the guest sits on an Otherix-managed-DHCP OVERLAY (--type overlay
# --subnet --dhcp). The agent's ssh-pipe handler resolves the guest IP from the
# CP-IPAM DHCP reservation it serves for the VM's NIC MAC (its anti-SSRF IP
# source); the overlay reconciler is the path that loads those reservations into
# the per-node DHCP responder, so the agent can address the guest.
#
# Connector HOME isolation: both the operator credential cache and the external
# connector state (the grant token is a bearer secret) are written under
# throwaway temp HOME dirs, so the smoke never touches the real ~/.ssh or
# ~/.otherix and is fully re-runnable. The cluster SSH-ingress switch is
# restored to its prior value on exit.
#
# Readiness: rather than a guest-side serial sentinel (an extra cloud-config
# part would collide with the CP's CA-trust part under cloud-init's
# last-part-wins merge of write_files/runcmd), the smoke gates readiness by
# RETRYING the real connect until the guest answers - which is also exactly the
# assertion. The guest needs no custom user-data, only --ssh-ingress.
#
# HA-through-proxy variant (manual): the operator path also works with the api
# fronted by a reverse proxy across HA replicas, because the relay carries a
# single-step bearer and all state lives in etcd (no stickiness). To exercise
# it by hand, put an nginx/caddy WebSocket proxy in front of two api replicas
# (forward `Upgrade`/`Connection` on `GET /v1/vms/{vm}/ssh-stream`, raise the
# idle timeout above the 15s keepalive - see docs/guides/ssh-access.md), point
# the CLI cluster endpoint at the proxy, and re-run the operator step. It is
# not automated here for the same reason smoke-ha is standalone: it needs its
# own multi-replica front end, not the single dev api.
#
# PREREQUISITES: a seeded dev stack built from the CURRENT tree:
#   make build && make local-dev-start
# Node-1 ready with the cluster default pool reconciled and the overlay fabric
# (WireGuard) up. The system `ssh` client must be on PATH (the operator and
# external paths exec it).
#
# Usage: make smoke-vm-ssh   (or: bash dev/smoke/vm-ssh/run.sh)

set -euo pipefail

# shellcheck source=../lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- configuration -----------------------------------------------------
OTX="${OTX:-./bin/otherix}"
OTX_SSH="${OTX_SSH:-./bin/otherix-ssh}"
NODE1="node-1"

NET="vm-ssh-ovl"                   # dhcp-enabled overlay (agent learns guest IP via the CP reservation)
SUBNET="10.91.0.0/24"              # unlikely to clash with the dev stack
VM_A="vm-ssh-a"                    # operator + external + revoke target
VM_B="vm-ssh-b"                    # incremental add-vm target
SUFFIX="ssh.otherix.local"         # cluster DNS suffix for <vm>.<suffix>
LOGIN="ubuntu"                     # default user in the Noble minimal cloudimg

IMAGE_URL="${IMAGE_URL:-${SMOKE_IMAGE_URL}}"  # default: host-arch Noble minimal cloudimg
ARCH="${ARCH:-${SMOKE_ARCH}}"
CREATE_WAIT="${CREATE_WAIT:-600}"  # seconds for vm create -> running (incl. cold image fetch)
NET_WAIT="${NET_WAIT:-180}"        # seconds to wait for the overlay to reconcile ready
CONNECT_WAIT="${CONNECT_WAIT:-600}" # seconds to retry the first connect while a guest boots
SSH_TIMEOUT="${SSH_TIMEOUT:-30}"   # per-connect ssh ConnectTimeout

# Absolute path to the operator CLI config the seeded dev cluster lives in. The
# operator SSH steps run with an isolated HOME (to keep the cert cache out of
# the real ~/.otherix), so the CLI is pointed back at the seeded config here.
REAL_CONFIG="${OTHERIX_CONFIG:-${HOME}/.otherix/config}"

# --- helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YEL=$'\033[33m'; NC=$'\033[0m'
pass() { echo "${GREEN}PASS${NC} $*"; }
info() { echo "${YEL}..${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

otx() { "$OTX" "$@"; }

# net_ready_node1 NET -> 0 when the network reconciled to "ready" on node-1.
net_ready_node1() {
  [[ "$(otx network get "$1" --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.status.nodes[]? | select(.node_name==$n) | .reconciliation_status')" \
      == "ready" ]]
}

# pool_ready_node1 -> 0 when the cluster default pool reconciled on node-1.
pool_ready_node1() {
  [[ "$(otx pool get default --output json 2>/dev/null \
      | jq -r --arg n "$NODE1" '.instances[]? | select(.node==$n) | .reconciliation_status')" == "ready" ]]
}

# operator_ssh VM LOGIN CMD -> run CMD in the guest as the operator and print
# the guest stdout. This is the `otherix ssh proxy` ProxyCommand form (the brief-
# endorsed scriptable path): a guest cert is minted via the operator connector,
# then the system ssh tunnels through the relay with a remote command.
#
# The cert is minted by `otherix ssh` itself (the only operator cert-mint path),
# run with a no-op `ssh` shim on PATH so it mints into OP_HOME and returns
# without connecting (the mint is cache-aware, so retries are cheap). The real
# connect then runs system ssh with every host-key/identity option on the
# COMMAND LINE: the ssh client resolves its default config and known_hosts via
# the password database (getpwuid), NOT $HOME, so a HOME-redirected ssh_config
# would be ignored - command-line -o is the portable way to isolate. The
# ProxyCommand re-enters `otherix ssh proxy`, inheriting OTHERIX_CONFIG to
# resolve the same cluster.
operator_ssh() {
  local vm="$1" login="$2" cmd="$3"
  PATH="${SHIM_DIR}:${PATH}" env HOME="$OP_HOME" OTHERIX_CONFIG="$REAL_CONFIG" \
    "$OTX" ssh "$vm" --login "$login" </dev/null >/dev/null 2>&1 || true
  env HOME="$OP_HOME" OTHERIX_CONFIG="$REAL_CONFIG" ssh \
    -i "${OP_HOME}/.otherix/ssh/id_ed25519" \
    -o "CertificateFile=${OP_HOME}/.otherix/ssh/id_ed25519-cert.pub" \
    -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new \
    -o "UserKnownHostsFile=${OP_HOME}/known_hosts" -o BatchMode=yes \
    -o "ConnectTimeout=${SSH_TIMEOUT}" \
    -o "ProxyCommand=${OTX_ABS} ssh proxy %h %p" \
    "${login}@${vm}" "$cmd"
}

# ext_ssh VM_FQDN CMD -> run CMD in the guest as the external user. `otherix-ssh
# add` wrote a managed ssh_config fragment (ProxyCommand + cert/identity +
# accept-new) under EXT_HOME; ssh is pointed straight at it with -F (the default
# ~/.ssh/config is resolved via getpwuid, not $HOME, so -F is the portable way
# to isolate). HOME=EXT_HOME is still exported so the otherix-ssh proxy
# subprocess the fragment invokes finds its stored grant state; that proxy mints
# the guest cert inside the connect using only the grant token.
ext_ssh() {
  local fqdn="$1" cmd="$2"
  env HOME="$EXT_HOME" ssh -F "${EXT_HOME}/.otherix/ssh/config" \
    -o BatchMode=yes -o "ConnectTimeout=${SSH_TIMEOUT}" \
    "${LOGIN}@${fqdn}" "$cmd"
}

# run_until_nonce TIMEOUT NONCE CMD... -> retry CMD (a connect helper that
# echoes NONCE in the guest) until its stdout carries NONCE or the deadline
# passes. Used both as the guest-boot readiness gate and as the assertion: a
# returned nonce proves a real guest login over the relay.
run_until_nonce() {
  local to="$1" nonce="$2"; shift 2
  local deadline=$(( SECONDS + to )) out
  while (( SECONDS < deadline )); do
    out="$("$@" 2>/dev/null || true)"
    grep -q "$nonce" <<<"$out" && return 0
    sleep 8
  done
  return 1
}

# --- isolated HOME dirs + cloud-init network-config --------------------
WORKDIR="$(mktemp -d)"
OP_HOME="${WORKDIR}/op-home"
EXT_HOME="${WORKDIR}/ext-home"
SHIM_DIR="${WORKDIR}/shim"
NC_FILE="${WORKDIR}/network-config.yaml"
mkdir -p "$OP_HOME" "$EXT_HOME" "$SHIM_DIR"

# Absolute path to the operator CLI, for the ssh ProxyCommand (ssh runs it
# through the shell and the working directory there is not guaranteed).
OTX_ABS="$(cd "$(dirname "$OTX")" && pwd)/$(basename "$OTX")"

# A no-op `ssh` shim: `otherix ssh <vm>` mints the guest cert and then execs the
# system ssh; with this shim first on PATH that exec is a no-op, so the operator
# cert mints into OP_HOME without a connect. The real connect uses the system
# ssh directly (this shim is NOT on PATH for it).
printf '#!/bin/sh\nexit 0\n' >"${SHIM_DIR}/ssh"
chmod +x "${SHIM_DIR}/ssh"

# network-config (netplan v2): DHCP the single overlay NIC, marked optional so
# systemd-networkd-wait-online does not block boot waiting for the lease (the
# sibling overlay-isolated-dns smoke uses the same pattern). No user-data: the
# CP injects the SSH-CA-trust cloud-config at create (--ssh-ingress) and the
# image's default `ubuntu` user is the login.
cat >"$NC_FILE" <<'EOF'
network:
  version: 2
  ethernets:
    overlay-nic:
      match:
        name: "en*"
      dhcp4: true
      dhcp6: false
      optional: true
EOF

# --- cleanup -----------------------------------------------------------
PRIOR_SSH_ENABLED=""
PRIOR_SSH_SUFFIX=""
restore_ssh_ingress() {
  # Restore the cluster SSH-ingress switch to whatever it was before the smoke.
  [ -n "$PRIOR_SSH_ENABLED" ] || return 0
  if [ "$PRIOR_SSH_ENABLED" = "true" ]; then
    otx cluster set-ssh-ingress --enabled --suffix "$PRIOR_SSH_SUFFIX" >/dev/null 2>&1 || true
  else
    otx cluster set-ssh-ingress --enabled=false >/dev/null 2>&1 || true
  fi
}
cleanup() {
  echo "--- cleanup ---"
  otx ssh-grant revoke ext-ssh >/dev/null 2>&1 || true
  otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
  otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
  otx network delete "$NET" --force >/dev/null 2>&1 || true
  restore_ssh_ingress
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# --- preconditions -----------------------------------------------------
echo "=== vm-ssh smoke: preconditions ==="
command -v jq >/dev/null || fail "jq is required"
command -v ssh >/dev/null || fail "the system 'ssh' client is required on PATH"
[ -x "$OTX" ] || fail "otherix CLI not found at '$OTX' (run make build, or set OTX=...)"
[ -x "$OTX_SSH" ] || fail "otherix-ssh connector not found at '$OTX_SSH' (run make build-ssh, or set OTX_SSH=...)"
cp_ready || fail "CP not up on :8080 (run make local-dev-start)"
CP_VERSION="$(cp_version)"
info "CP version: ${CP_VERSION}"
st="$(otx node get "$NODE1" --output json 2>/dev/null | jq -r '.status' || true)"
[[ "$st" == "ready" ]] || fail "$NODE1 not ready (got '${st:-none}'); run make local-dev-start"
deadline=$(( SECONDS + 60 )); ok=0
while (( SECONDS < deadline )); do pool_ready_node1 && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "default pool not ready on $NODE1 within 60s (CP auto-provision)"
pass "CP up (${CP_VERSION}); $NODE1 ready; default pool ready on $NODE1"

# best-effort delete-first so a stale leftover from a prior run does not 409
otx ssh-grant revoke ext-ssh >/dev/null 2>&1 || true
otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true

# --- step 1: enable cluster SSH ingress --------------------------------
echo "=== step 1: enable cluster SSH ingress (--suffix $SUFFIX) ==="
# Capture the prior switch so cleanup restores it (re-runnable).
PRIOR="$(otx cluster get-ssh-ingress --output json 2>/dev/null || echo '{}')"
PRIOR_SSH_ENABLED="$(jq -r '.enabled // false' <<<"$PRIOR")"
PRIOR_SSH_SUFFIX="$(jq -r '.cluster_suffix // ""' <<<"$PRIOR")"
otx cluster set-ssh-ingress --enabled --suffix "$SUFFIX" >/dev/null \
  || fail "cluster set-ssh-ingress failed"
GOT_SUFFIX="$(otx cluster get-ssh-ingress --output json | jq -r '.cluster_suffix')"
[[ "$GOT_SUFFIX" == "$SUFFIX" ]] || fail "ssh-ingress suffix is '$GOT_SUFFIX', want '$SUFFIX'"
pass "cluster SSH ingress enabled (suffix $SUFFIX)"

# --- step 2: dhcp overlay + two SSH-ingress VMs ------------------------
echo "=== step 2: dhcp overlay $NET + two SSH-ingress VMs ==="
otx network create "$NET" --type overlay --subnet "$SUBNET" --dhcp \
  || fail "network create $NET failed"
deadline=$(( SECONDS + NET_WAIT )); ok=0
info "waiting for $NET to reconcile ready on $NODE1 (<= ${NET_WAIT}s)"
while (( SECONDS < deadline )); do net_ready_node1 "$NET" && { ok=1; break; }; sleep 3; done
(( ok == 1 )) || fail "$NET did not reconcile ready on $NODE1 within ${NET_WAIT}s"
pass "dhcp overlay $NET ready (the agent learns each guest IP from the CP reservation it serves)"

# Both VMs: SSH-ingress on, pinned to node-1, on the dhcp overlay. The first
# create cold-fetches the image; the second reuses the per-node cache.
for vm in "$VM_A" "$VM_B"; do
  info "creating $vm (--ssh-ingress, $NET, node $NODE1)"
  otx vm create "$vm" \
    --image-url "$IMAGE_URL" --arch "$ARCH" --node "$NODE1" --network "$NET" \
    --ssh-ingress --vcpus 2 --memory-mb 2048 \
    --network-config "$NC_FILE" \
    --wait --wait-timeout "${CREATE_WAIT}s" \
    || fail "vm create $vm did not reach running within ${CREATE_WAIT}s"
done
pass "both SSH-ingress VMs running ($VM_A, $VM_B)"

# --- step 3: operator path - a real guest login over the tunnel --------
echo "=== step 3: operator path (otherix ssh) - real guest login ==="
# Retry the operator connect while the guest finishes booting (sshd up, lease
# held, CA trust applied); a returned nonce is the real-guest-login assertion.
NONCE_OP="otx-op-${RANDOM}-${RANDOM}"
if ! run_until_nonce "$CONNECT_WAIT" "$NONCE_OP" operator_ssh "$VM_A" "$LOGIN" "echo ${NONCE_OP}"; then
  echo "--- last operator connect attempt (stderr) ---"
  operator_ssh "$VM_A" "$LOGIN" "echo ${NONCE_OP}" 2>&1 | tail -8 || true
  fail "operator 'otherix ssh $VM_A' never returned the nonce from the guest within ${CONNECT_WAIT}s"
fi
pass "operator 'otherix ssh $VM_A --login $LOGIN' logged into the guest (nonce returned over the relay)"

# --- step 4: external path - grant bundle + otherix-ssh add ------------
echo "=== step 4: external path (ssh-grant + otherix-ssh) ==="
# Mint a grant for VM_A and import it in the isolated external HOME. The bundle
# carries the one-time grant token (a bearer secret), so it is piped straight
# into the connector and never echoed.
BUNDLE="$(otx ssh-grant create ext-ssh --vm "$VM_A" --login "$LOGIN" --ttl 1h -o json)" \
  || fail "ssh-grant create failed"
printf '%s' "$BUNDLE" | env HOME="$EXT_HOME" "$OTX_SSH" add - --suffix "$SUFFIX" >/dev/null \
  || fail "otherix-ssh add failed"
pass "grant minted and imported by the external connector (isolated HOME, no real ~/.ssh touched)"

NONCE_EXT="otx-ext-${RANDOM}-${RANDOM}"
if ! run_until_nonce 180 "$NONCE_EXT" ext_ssh "${VM_A}.${SUFFIX}" "echo ${NONCE_EXT}"; then
  echo "--- last external ssh attempt (stderr) ---"
  ext_ssh "${VM_A}.${SUFFIX}" "echo ${NONCE_EXT}" 2>&1 | tail -8 || true
  fail "external 'ssh ${VM_A}.${SUFFIX}' did not return the nonce from the guest"
fi
pass "external 'ssh ${VM_A}.${SUFFIX}' logged into the guest using only the grant token"

# --- step 5: incremental - add a VM to the grant, no re-import ----------
echo "=== step 5: incremental (ssh-grant add-vm) - no re-import ==="
otx ssh-grant add-vm ext-ssh "$VM_B" --login "$LOGIN" >/dev/null \
  || fail "ssh-grant add-vm failed"
NONCE_INC="otx-inc-${RANDOM}-${RANDOM}"
if ! run_until_nonce "$CONNECT_WAIT" "$NONCE_INC" ext_ssh "${VM_B}.${SUFFIX}" "echo ${NONCE_INC}"; then
  echo "--- last external ssh attempt (stderr) ---"
  ext_ssh "${VM_B}.${SUFFIX}" "echo ${NONCE_INC}" 2>&1 | tail -8 || true
  fail "after add-vm, 'ssh ${VM_B}.${SUFFIX}' did not reach the guest (the connector should resolve the new scope at connect time)"
fi
pass "external 'ssh ${VM_B}.${SUFFIX}' works after add-vm with NO re-import"

# --- step 6: revocation - the next connect is rejected -----------------
echo "=== step 6: revocation (ssh-grant revoke) ==="
otx ssh-grant revoke ext-ssh >/dev/null || fail "ssh-grant revoke failed"
NONCE_REV="otx-rev-${RANDOM}-${RANDOM}"
if OUT_REV="$(ext_ssh "${VM_A}.${SUFFIX}" "echo ${NONCE_REV}" 2>/dev/null)" \
   && grep -q "$NONCE_REV" <<<"$OUT_REV"; then
  fail "external ssh still reached the guest AFTER revoke - the grant must no longer authorize"
fi
pass "external 'ssh ${VM_A}.${SUFFIX}' rejected after revoke (grant no longer authorizes)"

# --- step 7: teardown --------------------------------------------------
echo "=== step 7: teardown VMs + overlay + restore ssh-ingress ==="
otx vm delete "$VM_A" --wait --force >/dev/null 2>&1 || true
otx vm delete "$VM_B" --wait --force >/dev/null 2>&1 || true
otx network delete "$NET" --force >/dev/null 2>&1 || true
restore_ssh_ingress
pass "VMs + overlay deleted; ssh-ingress restored"

trap - EXIT
rm -rf "$WORKDIR" 2>/dev/null || true
echo
echo "${GREEN}=== vm-ssh smoke PASSED ===${NC}"
