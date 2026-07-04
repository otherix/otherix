#!/usr/bin/env bash
# Otherix chaos: permanently terminate a harness node.
#
# Simulates a total host loss: terminate is irreversible and also destroys
# the instance-store NVMe pool, so this is a realistic host + local-disk
# failure (not a stop, which would preserve the disks).
#
# Usage: kill.sh <env> <role> [index]
#   Run from the test-harness dir with the matching tofu workspace selected
#   (the harness-chaos-kill make target does both). Resolves the target
#   instance id from `tofu output`, prints exactly which node it will
#   terminate, then terminates it. Fails closed: an empty env or an
#   unresolved instance id aborts before any AWS call.
set -euo pipefail

ENV_NAME="${1:?usage: kill.sh <env> <role> [index]}"
ROLE="${2:?usage: kill.sh <env> <role> [index]}"
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

echo "TERMINATE env=${ENV_NAME} role=${ROLE} node=${NODE_NAME} instance=${INSTANCE_ID}"
aws ec2 terminate-instances --instance-ids "${INSTANCE_ID}"
