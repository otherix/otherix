#!/usr/bin/env bash
# Otherix AWS harness: spot CAPACITY-AVAILABILITY survey.
#
# The price scripts say where spot is cheap and stable. They do NOT say where
# capacity exists right now: InsufficientInstanceCapacity comes from the
# (instance-type x AZ) pool, which price history cannot predict. This adds the
# two signals that can:
#   1. Spot Placement Score (ec2:GetSpotPlacementScores) - AWS's own 1-10 capacity
#      forecast for a given type-set + target capacity. Price is NOT part of it.
#   2. Spot Advisor feed - historical interruption-frequency bucket per type/region.
#
# How to read the combined harness-spot-report: gate on SPS first (keep regions
# scoring >=7), THEN pick the cheapest / most stable survivor from the price and
# stability tables above. Capacity is the gate; price is the tie-break - not the
# other way round.
set -uo pipefail

REGIONS="eu-north-1 eu-central-1 ap-south-1 us-east-2 us-east-1 eu-west-1"

# role => target instance count => interchangeable instance-type pool.
# SPS scores the whole pool as one question: "can this region supply <target>
# units from ANY of these types?" - so a pool with more members is far more
# likely to score high. That diversification IS the real fix for the capacity
# gap; a single type in a single region is what fails.
#
# Agents need bare-metal (nested KVM) + arm64 (Graviton) + local NVMe, so the
# "d" Graviton metals are the substitution set. CP/gateway are ubiquitous types
# and rarely the bottleneck, scored here only for completeness.
AGENT_TARGET=2
AGENT_POOL="c6gd.metal c7gd.metal m6gd.metal m7gd.metal r6gd.metal"
CP_TARGET=3
CP_POOL="m6g.large m7g.large c6g.large c7g.large"
GW_TARGET=2
GW_POOL="t4g.medium"

if ! aws sts get-caller-identity >/dev/null 2>&1; then
  echo "AWS creds not valid in this shell." >&2
  echo "Configure AWS credentials first (env vars, an ~/.aws profile, or SSO), e.g.:" >&2
  echo "  AWS_PROFILE=<your-profile> bash $0" >&2
  exit 1
fi

# GetSpotPlacementScores is a regional API call but scores whatever --region-names
# asks for; pick any reachable region as the endpoint.
API_REGION=$(echo "$REGIONS" | awk '{print $1}')

# sps_report LABEL TARGET "POOL" -> per-region score table, ranked high first.
# Prints the capacity-safe shortlist (SPS>=7) as the actionable line.
#
# SPS has a low, per-minute request quota and - unhelpfully - reports a throttle
# as "instance types ... not valid" (InvalidParameterValue), not a throttle error.
# We capture stderr and treat any error as soft so it never poisons the table;
# re-run after a minute if you see the rate-limit line.
sps_report() {
  local label=$1 target=$2 pool=$3
  echo "=== $label: Spot Placement Score (target=$target units, pool: $pool) ==="
  local out
  out=$(aws ec2 get-spot-placement-scores --region "$API_REGION" \
          --region-names $REGIONS \
          --instance-types $pool \
          --target-capacity "$target" --target-capacity-unit-type units \
          --query 'SpotPlacementScores[].[Region,Score]' --output text 2>&1)
  if printf '%s' "$out" | grep -qi 'error occurred'; then
    if printf '%s' "$out" | grep -qiE 'not valid|throttl|rate exceeded|requestlimit'; then
      echo "  SPS rate-limited (or unsupported type mix) - wait ~1 min and re-run"
    else
      echo "  SPS call failed: $(printf '%s' "$out" | head -1)"
    fi
    echo
    return
  fi
  if [ -z "$out" ]; then
    echo "  (no scores returned)"
    echo
    return
  fi
  { echo "REGION SCORE VERDICT"
    echo "$out" | sort -k2,2rn | awk '{
      v = ($2>=8)?"safe" : ($2>=7)?"ok" : ($2>=5)?"risky" : "avoid";
      printf "%s %s %s\n", $1, $2, v; }'
  } | column -t
  local safe
  safe=$(echo "$out" | awk '$2>=7{print $1}' | sort | paste -sd' ' -)
  echo "capacity-safe (SPS>=7): ${safe:-<none - widen the pool or add regions>}"
  echo
}

echo "Scores are point-in-time and relative to target capacity; re-run near deploy."
echo
# Space the calls: SPS throttles hard and its quota replenishes per-minute.
sps_report "AGENTS" "$AGENT_TARGET" "$AGENT_POOL"; sleep 3
sps_report "CONTROL-PLANE" "$CP_TARGET" "$CP_POOL"; sleep 3
sps_report "GATEWAY" "$GW_TARGET" "$GW_POOL"

# --- Spot Advisor: historical interruption frequency (needs jq + curl) ---
# Feed schema: .spot_advisor[<region>].Linux[<instance-type>] = {s:<savings%>, r:<rate-index>}
# rate-index buckets: 0=<5%  1=5-10%  2=10-15%  3=15-20%  4=>20% interruptions/mo.
if command -v jq >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
  FEED="https://spot-bid-advisor.s3.amazonaws.com/spot-advisor-data.json"
  ADV=$(curl -fsS "$FEED" 2>/dev/null)
  if [ -n "$ADV" ]; then
    echo "=== AGENTS: Spot Advisor interruption frequency (Linux, lower=steadier) ==="
    { echo "REGION TYPE INTERRUPTION"
      for r in $REGIONS; do
        for it in $AGENT_POOL; do
          idx=$(echo "$ADV" | jq -r --arg r "$r" --arg it "$it" \
                  '.spot_advisor[$r].Linux[$it].r // "-"' 2>/dev/null)
          case "$idx" in
            0) lbl="<5%";; 1) lbl="5-10%";; 2) lbl="10-15%";;
            3) lbl="15-20%";; 4) lbl=">20%";; *) lbl="n/a";;
          esac
          echo "$r $it $lbl"
        done
      done
    } | column -t
    echo
  fi
else
  echo "(install jq + curl for the Spot Advisor interruption-frequency table)"
  echo
fi

cat <<'EOF'
NOTE: to stop hitting InsufficientInstanceCapacity, don't pin one type in one AZ.
Launch the agents from an EC2 Fleet / ASG over the whole AGENT_POOL across every
AZ with allocation strategy `price-capacity-optimized` (not lowest-price) plus an
on-demand fallback. Region choice then only improves the odds instead of gating
the whole deploy on a single pool.

Gateways (small arm64) score poorly on spot in every region and can't be widened
into a large capacity pool - they are cheap, so run them on-demand rather than
chasing spot capacity for them.
EOF
