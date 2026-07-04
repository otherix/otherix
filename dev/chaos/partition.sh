#!/usr/bin/env bash
# Otherix chaos: fully network-isolate a harness node without stopping it.
#
# Swaps the node's security group for the rule-less blackhole SG, which
# drops all traffic in and out. The instance keeps running, so the
# instance-store NVMe pool survives - this models a network partition, not
# a host loss. Reverse it with heal.sh.
#
# Usage: partition.sh <env> <role> [index]
#   Run from the test-harness dir with the matching tofu workspace selected
#   (the harness-chaos-partition make target does both).
set -euo pipefail

ENV_NAME="${1:?usage: partition.sh <env> <role> [index]}"
ROLE="${2:?usage: partition.sh <env> <role> [index]}"
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

BLACKHOLE_SG="$(tofu output -raw blackhole_sg_id)"
if [ -z "${BLACKHOLE_SG}" ]; then
  echo "blackhole_sg_id output is empty in env '${ENV_NAME}'" >&2
  exit 1
fi

echo "PARTITION env=${ENV_NAME} role=${ROLE} node=${NODE_NAME} instance=${INSTANCE_ID} -> sg=${BLACKHOLE_SG}"
aws ec2 modify-instance-attribute --instance-id "${INSTANCE_ID}" --groups "${BLACKHOLE_SG}"
