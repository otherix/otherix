#!/usr/bin/env bash
# Otherix AWS harness: point the local `otherix` CLI at a running stand.
# Reads the stand's cp_url from OpenTofu, fetches the cluster CA over a
# trust-on-first-use fetch, pins it under ~/.otherix, and registers a
# CLI cluster profile for the stand.
#
# Usage: harness-config.sh <env>
#   Run from the test-harness dir with the matching tofu workspace selected
#   (the harness-config make target does both). Operator credentials come
#   from the environment: OTHERIX_LOGIN + OTHERIX_PASSWORD (see
#   `otherix config add cluster`), since the CLI bootstrap logs in to mint
#   an API token.
set -euo pipefail

ENV_NAME="${1:?usage: harness-config.sh <env>}"
CA_DIR="${HOME}/.otherix"
CA_FILE="${CA_DIR}/${ENV_NAME}-ca.pem"

# cp_url from tofu output; the caller selects the workspace first. Allow an
# explicit CP_URL override for reruns without a live tofu state.
CP_URL="${CP_URL:-$(tofu output -raw cp_url)}"
if [ -z "${CP_URL}" ]; then
  echo "cp_url is empty: is the '${ENV_NAME}' workspace selected and applied?" >&2
  exit 1
fi

mkdir -p "${CA_DIR}"

# Fetch the cluster CA. The CP TLS server cert does not chain to the cluster
# CA (trust on first use), so -k is expected here; the fingerprint below is
# the value the operator pins out of band.
CA_JSON="$(curl -sk "${CP_URL}/v1/ca")"
printf '%s\n' "${CA_JSON}" | jq -r '.cas[].cert_pem' > "${CA_FILE}"
if [ ! -s "${CA_FILE}" ]; then
  echo "failed to fetch cluster CA from ${CP_URL}/v1/ca" >&2
  exit 1
fi
chmod 0600 "${CA_FILE}"

FINGERPRINT="$(printf '%s\n' "${CA_JSON}" | jq -r '.signer_fingerprint_sha256')"
echo "cluster CA written: ${CA_FILE}"
echo "signer fingerprint (pin this out of band): sha256:${FINGERPRINT}"

# Register the stand as a CLI cluster profile. --ca-file is the authoritative
# trust; --force lets a re-run replace an existing profile of the same name.
if command -v otherix >/dev/null 2>&1; then
  otherix config add cluster \
    --name "${ENV_NAME}" \
    --server "${CP_URL}" \
    --ca-file "${CA_FILE}" \
    --set-current \
    --force
  echo "cluster profile '${ENV_NAME}' registered and set current."
else
  echo
  echo "otherix CLI not on PATH; register manually:"
  echo "  otherix config add cluster --name ${ENV_NAME} --server ${CP_URL} --ca-file ${CA_FILE}"
fi
