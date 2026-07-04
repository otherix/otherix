#!/usr/bin/env bash
# Otherix AWS harness: 90-day spot-price STABILITY survey.
# For each region/type/AZ: N samples, min, max, mean, stddev, CV% (stddev/mean).
# MAX = worst spike over 90d (interruption-pressure proxy); CV% = volatility.
# AWS retains only 90 days of spot history, so that is the full window.
set -uo pipefail

REGIONS="eu-north-1 eu-central-1 ap-south-1 us-east-2 us-east-1 eu-west-1"
TYPES="c6gd.metal m6g.large t4g.medium"

if ! aws sts get-caller-identity >/dev/null 2>&1; then
  echo "AWS creds not valid in this shell." >&2
  echo "Run inside your op wrapper, e.g.:" >&2
  echo "  eval \"\$(op plugin run -- aws configure export-credentials --format env)\" && bash $0" >&2
  exit 1
fi

# 90 days back. BSD/macOS date first, GNU date fallback.
START=$(date -u -v-90d +%Y-%m-%dT%H:%M:%S 2>/dev/null || date -u -d '90 days ago' +%Y-%m-%dT%H:%M:%S)
END=$(date -u +%Y-%m-%dT%H:%M:%S)
echo "window: $START .. $END (UTC)"

TMP=$(mktemp)
for r in $REGIONS; do
  for it in $TYPES; do
    aws ec2 describe-spot-price-history --region "$r" \
      --instance-types "$it" --product-descriptions "Linux/UNIX" \
      --start-time "$START" --end-time "$END" \
      --query 'SpotPriceHistory[].[AvailabilityZone,SpotPrice]' \
      --output text 2>/dev/null \
    | awk -v r="$r" -v it="$it" '
        { az=$1; p=$2+0; n[az]++; sum[az]+=p; sumsq[az]+=p*p;
          if(min[az]==""||p<min[az])min[az]=p;
          if(p>max[az])max[az]=p; }
        END{
          for(a in n){
            mean=sum[a]/n[a];
            var=sumsq[a]/n[a]-mean*mean; if(var<0)var=0; sd=sqrt(var);
            cv=(mean>0)?sd/mean*100:0;
            printf "%s %s %s %d %.4f %.4f %.4f %.4f %.1f\n", r,it,a,n[a],min[a],max[a],mean,sd,cv;
          }
        }' >> "$TMP"
  done
done

echo
echo "=== FULL (all types, by region / AZ) ==="
{ echo "REGION TYPE AZ N MIN MAX MEAN STDDEV CV%"; sort -k1,1 -k2,2 -k3,3 "$TMP"; } | column -t
echo
echo "=== c6gd.metal ranked by MEAN (low=cheap; check MAX ceiling + CV% for stability) ==="
{ echo "REGION TYPE AZ N MIN MAX MEAN STDDEV CV%"; grep 'c6gd.metal' "$TMP" | sort -k7,7g; } | column -t
rm -f "$TMP"
