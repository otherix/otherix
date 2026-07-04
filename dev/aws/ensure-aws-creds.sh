# shellcheck shell=sh
# Verify usable AWS credentials before the harness runs tofu.
#
# SOURCED (not executed) by the harness make targets, so it uses `return`
# rather than `exit` and never enables `set -e` (either would abort make's
# shell). It relies only on the standard AWS credential chain (environment
# variables, an ~/.aws profile, SSO, or an instance role) - configure
# credentials however you like before running the harness. It fails with
# guidance if none resolve.

if ! command -v aws >/dev/null 2>&1; then
    echo "error: the aws CLI is required for the harness credential preflight." >&2
    echo "Install it (https://aws.amazon.com/cli/) and configure credentials." >&2
    return 1
fi

if ! aws sts get-caller-identity >/dev/null 2>&1; then
    echo "error: no usable AWS credentials for the harness." >&2
    echo "Configure them via environment variables, an ~/.aws profile, or SSO" >&2
    echo "(e.g. 'export AWS_PROFILE=<profile>' or 'aws sso login')." >&2
    return 1
fi
