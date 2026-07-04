# shellcheck shell=sh
# Ensure the AWS SDK credential chain is usable before the harness runs tofu.
#
# SOURCED (not executed) by the harness make targets from the repository root,
# so it uses `return` rather than `exit` and never enables `set -e` (either
# would abort make's shell). It relies only on the standard AWS credential chain
# (environment variables, ~/.aws profiles, SSO, or an instance role) - nothing
# here is tied to any particular secrets manager.
#
# If credentials do not already resolve, it sources an optional, gitignored
# personal hook at dev/aws/aws-creds.local.sh (which should make working AWS_*
# credentials available however you like), then re-checks. It fails with
# guidance if the credentials are still unusable.

if ! command -v aws >/dev/null 2>&1; then
    echo "error: the aws CLI is required for the harness credential preflight." >&2
    echo "Install it (https://aws.amazon.com/cli/) and configure credentials." >&2
    return 1
fi

if ! aws sts get-caller-identity >/dev/null 2>&1; then
    if [ -f dev/aws/aws-creds.local.sh ]; then
        # shellcheck disable=SC1091
        . dev/aws/aws-creds.local.sh
    fi
    if ! aws sts get-caller-identity >/dev/null 2>&1; then
        echo "error: no usable AWS credentials for the harness." >&2
        echo "Provide them via environment variables, an ~/.aws profile, or SSO" >&2
        echo "(e.g. 'aws sso login' or 'export AWS_PROFILE=...'), or create" >&2
        echo "dev/aws/aws-creds.local.sh (gitignored) to export them." >&2
        return 1
    fi
fi
