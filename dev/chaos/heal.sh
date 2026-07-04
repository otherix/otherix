#!/usr/bin/env bash
# Otherix chaos: reverse a partition and/or latency fault on a harness node.
#
# Restores the node's role security group (undoing partition.sh) and makes a
# best-effort attempt to clear any tc netem qdisc via SSM (undoing
# latency.sh). The SSM step is tolerated to fail: a node that was never given
# latency, or that is not SSM-managed, still heals its security group.
#
# Usage: heal.sh <env> <role> [index]
#   Run from the test-harness dir with the matching tofu workspace selected
#   (the harness-chaos-heal make target does both).
set -euo pipefail

ENV_NAME="${1:?usage: heal.sh <env> <role> [index]}"
ROLE="${2:?usage: heal.sh <env> <role> [index]}"
INDEX="${3:-0}"

case "${ROLE}" in
  cp) PREFIX="cp" ;;
  agent) PREFIX="agent" ;;
  gateway) PREFIX="gw" ;;
  *) echo "unknown role '${ROLE}' (want cp|agent|gateway)" >&2; exit 1 ;;
esac
NODE_NAME="${PREFIX}-${INDEX}"

# Guard against acting on the wrong stand: tofu output reads whatever workspace
# is currently selected, so a direct invocation that bypasses the make target
# (which selects the workspace) must match the requested env.
SELECTED="$(tofu workspace show)"
if [ "${SELECTED}" != "${ENV_NAME}" ]; then
  echo "selected tofu workspace '${SELECTED}' != requested env '${ENV_NAME}'; run via the make target or 'tofu workspace select ${ENV_NAME}'" >&2
  exit 1
fi

INSTANCE_ID="$(tofu output -json instance_ids | jq -r --arg k "${NODE_NAME}" '.[$k] // empty')"
if [ -z "${INSTANCE_ID}" ]; then
  echo "no instance id for node '${NODE_NAME}' in env '${ENV_NAME}': is the workspace selected and applied?" >&2
  exit 1
fi

ROLE_SG="$(tofu output -json role_security_group_ids | jq -r --arg r "${ROLE}" '.[$r] // empty')"
if [ -z "${ROLE_SG}" ]; then
  echo "no role security group for role '${ROLE}' in env '${ENV_NAME}'" >&2
  exit 1
fi

echo "HEAL env=${ENV_NAME} role=${ROLE} node=${NODE_NAME} instance=${INSTANCE_ID} -> sg=${ROLE_SG}"
aws ec2 modify-instance-attribute --instance-id "${INSTANCE_ID}" --groups "${ROLE_SG}"

# Best-effort clear of any tc netem left by latency.sh. The remote detects the
# same primary interface latency.sh used. Tolerate SSM being unavailable.
echo "clearing tc netem via SSM (best effort)"
# The single-quoted payload is intentional: $iface and $5 must expand on the
# remote host, not locally.
# shellcheck disable=SC2016
if aws ssm send-command \
    --document-name AWS-RunShellScript \
    --instance-ids "${INSTANCE_ID}" \
    --parameters 'commands=["iface=$(ip route get 1.1.1.1 | awk '\''{print $5; exit}'\''); tc qdisc del dev $iface root || true"]' \
    >/dev/null 2>&1; then
  echo "tc netem clear command dispatched"
else
  echo "SSM clear skipped (node not SSM-managed or SSM unavailable); security group already healed"
fi
