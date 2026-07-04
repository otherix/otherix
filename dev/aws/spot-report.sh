#!/usr/bin/env bash
# Otherix AWS harness: c6gd.metal (agents) / m6g.large (CP) / t4g.medium (gateway)
# spot-price + AZ-availability survey across candidate regions.
set -uo pipefail

REGIONS="eu-west-1 eu-central-1 eu-north-1 us-east-1 us-east-2 ap-south-1"
TYPES="c6gd.metal m6g.large t4g.medium"

if ! aws sts get-caller-identity >/dev/null 2>&1; then
  echo "AWS creds not valid in this shell." >&2
  echo "Configure AWS credentials first (env vars, an ~/.aws profile, or SSO), e.g.:" >&2
  echo "  AWS_PROFILE=<your-profile> bash $0" >&2
  exit 1
fi

NOW=$(date -u +%Y-%m-%dT%H:%M:%S)
printf "%-14s %-12s %-5s %-11s %s\n" REGION TYPE AZs MIN_SPOT AZ
printf "%-14s %-12s %-5s %-11s %s\n" "------" "----" "---" "--------" "--"
for r in $REGIONS; do
  for it in $TYPES; do
    azc=$(aws ec2 describe-instance-type-offerings --region "$r" \
            --location-type availability-zone \
            --filters "Name=instance-type,Values=$it" \
            --query 'length(InstanceTypeOfferings)' --output text 2>/dev/null)
    line=$(aws ec2 describe-spot-price-history --region "$r" \
            --instance-types "$it" --product-descriptions "Linux/UNIX" \
            --start-time "$NOW" \
            --query 'SpotPriceHistory[].[SpotPrice,AvailabilityZone]' \
            --output text 2>/dev/null | sort -g | head -1)
    price=$(echo "$line" | awk '{print $1}')
    az=$(echo "$line" | awk '{print $2}')
    printf "%-14s %-12s %-5s %-11s %s\n" "$r" "$it" "${azc:-?}" "${price:-NA}" "${az:-}"
  done
  echo
done
