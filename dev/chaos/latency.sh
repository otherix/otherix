#!/usr/bin/env bash
# Otherix chaos: inject latency and packet loss on a harness node via tc netem.
#
# Applies a `tc qdisc replace ... netem` on the node's primary interface using
# SSM RunCommand. Models a degraded (not severed) link. Reverse it with
# heal.sh.
#
# PREREQUISITE: SSM RunCommand requires the amazon-ssm-agent to be installed
# and running on the target, with the node registered in SSM. Ubuntu AMIs do
# not always ship or enable it. If the instance is not listed by
# `aws ssm describe-instance-information`, this script prints that the node is
# not SSM-managed (latency chaos unavailable) and exits non-zero rather than
# hang on a command that will never be picked up.
#
# Usage: latency.sh <env> <role> [index] [delay_ms] [loss_pct]
#   Run from the test-harness dir with the matching tofu workspace selected
#   (the harness-chaos-latency make target does both).
set -euo pipefail

ENV_NAME="${1:?usage: latency.sh <env> <role> [index] [delay_ms] [loss_pct]}"
ROLE="${2:?usage: latency.sh <env> <role> [index] [delay_ms] [loss_pct]}"
INDEX="${3:-0}"
DELAY_MS="${4:-200}"
LOSS_PCT="${5:-5}"

case "${ROLE}" in
  cp) PREFIX="cp" ;;
  agent) PREFIX="agent" ;;
  gateway) PREFIX="gw" ;;
  *) echo "unknown role '${ROLE}' (want cp|agent|gateway)" >&2; exit 1 ;;
esac
NODE_NAME="${PREFIX}-${INDEX}"

INSTANCE_ID="$(tofu output -json instance_ids | jq -r --arg k "${NODE_NAME}" '.[$k] // empty')"
if [ -z "${INSTANCE_ID}" ]; then
  echo "no instance id for node '${NODE_NAME}' in env '${ENV_NAME}': is the workspace selected and applied?" >&2
  exit 1
fi

# Fail fast if the node is not SSM-managed rather than dispatch a command that
# nothing will ever pick up.
SSM_LISTED="$(aws ssm describe-instance-information \
  --filters "Key=InstanceIds,Values=${INSTANCE_ID}" \
  --query 'InstanceInformationList[0].InstanceId' --output text 2>/dev/null || true)"
if [ "${SSM_LISTED}" != "${INSTANCE_ID}" ]; then
  echo "node '${NODE_NAME}' (${INSTANCE_ID}) is not SSM-managed: latency chaos unavailable" >&2
  echo "install and enable amazon-ssm-agent on the target to use latency.sh" >&2
  exit 1
fi

echo "LATENCY env=${ENV_NAME} role=${ROLE} node=${NODE_NAME} instance=${INSTANCE_ID} delay=${DELAY_MS}ms loss=${LOSS_PCT}%"
aws ssm send-command \
  --document-name AWS-RunShellScript \
  --instance-ids "${INSTANCE_ID}" \
  --parameters "commands=[\"iface=\$(ip route get 1.1.1.1 | awk '{print \$5; exit}'); tc qdisc replace dev \$iface root netem delay ${DELAY_MS}ms loss ${LOSS_PCT}%\"]" \
  >/dev/null
echo "latency command dispatched"
