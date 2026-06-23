#!/usr/bin/env bash
# lima-ensure-vm.sh VM HOSTPORT - idempotently ensure one dev Lima VM exists,
# is Running, and is fully provisioned. It self-heals the two failure modes that
# previously forced a destructive full-stack rebuild:
#
#   1. a transient first-boot timeout (the host briefly saturates while the Go
#      build + sibling VM boots + apt all run at once, so the new VM misses
#      Lima's default boot deadline). Fixed by a generous --timeout and one
#      automatic retry that tears the half-created VM down before re-creating.
#
#   2. a half-provisioned VM left by an interrupted create (provisioning aborted
#      before it wrote the agent systemd unit). A plain restart can never stage
#      the agent on such a VM ("Unit otherix-agent.service not found" at seed
#      time). Detected by the provision marker and healed by a recreate.
#
# Tunables (env): LIMA_BOOT_TIMEOUT (default 10m), LIMA_CREATE_ATTEMPTS (default 2).
set -euo pipefail

VM="${1:?usage: lima-ensure-vm.sh VM HOSTPORT}"
HOSTPORT="${2:?usage: lima-ensure-vm.sh VM HOSTPORT}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
LIMA_YAML="${REPO_ROOT}/dev/lima/otherix-dev.yaml"

# Provisioning writes the agent systemd unit last, so its presence is a reliable
# "provisioning finished" marker - the same file whose absence surfaced as the
# seed-time "Unit otherix-agent.service not found" failure.
PROVISION_MARKER="/etc/systemd/system/otherix-agent.service"
BOOT_TIMEOUT="${LIMA_BOOT_TIMEOUT:-10m}"
CREATE_ATTEMPTS="${LIMA_CREATE_ATTEMPTS:-2}"

# nested_extra echoes the Lima flag that exposes /dev/kvm in the guest, but only
# on Apple M3+ AND macOS 15+. Injecting it on M1/M2 hard-fails Lima start, so it
# is set at create time on a capable host only, never in the committed yaml.
nested_extra() {
  [ "$(uname -s)" = Darwin ] || return 0
  local gen osmaj
  gen=$(sysctl -n machdep.cpu.brand_string 2>/dev/null | sed -nE 's/.*Apple M([0-9]+).*/\1/p')
  osmaj=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
  if [ -n "$gen" ] && [ "$gen" -ge 3 ] && [ "$osmaj" -ge 15 ]; then
    echo "--set .nestedVirtualization=true"
  fi
}

exists()      { limactl list -q 2>/dev/null | grep -qx "$VM"; }
running()     { [ "$(limactl list "$VM" --format '{{.Status}}' 2>/dev/null)" = Running ]; }
provisioned() { limactl shell "$VM" -- test -f "$PROVISION_MARKER" >/dev/null 2>&1; }

# create_vm creates the VM from scratch with a bounded retry. Each failed attempt
# force-deletes the half-created instance so the retry starts clean.
create_vm() {
  local extra attempt
  extra="$(nested_extra)"
  for attempt in $(seq 1 "$CREATE_ATTEMPTS"); do
    echo ">> creating Lima VM $VM (host port $HOSTPORT; attempt $attempt/$CREATE_ATTEMPTS, timeout $BOOT_TIMEOUT)"
    # shellcheck disable=SC2086 # $extra is an intentional multi-word flag or empty
    if limactl start --tty=false --name="$VM" \
         --set ".portForwards[0].hostPort = $HOSTPORT" $extra \
         --timeout "$BOOT_TIMEOUT" "$LIMA_YAML"; then
      return 0
    fi
    echo ">> $VM create attempt $attempt failed; tearing the half-created VM down before retry" >&2
    limactl stop --force "$VM" 2>/dev/null || true
    limactl delete --force "$VM" 2>/dev/null || true
  done
  echo "x $VM failed to create after $CREATE_ATTEMPTS attempts" >&2
  return 1
}

if ! exists; then
  create_vm
else
  if ! running; then
    echo ">> starting existing Lima VM $VM"
    if ! limactl start --timeout "$BOOT_TIMEOUT" "$VM"; then
      echo ">> $VM failed to start; recreating" >&2
      limactl delete --force "$VM" 2>/dev/null || true
      create_vm
    fi
  else
    echo ">> Lima VM $VM already running"
  fi
  # Heal a half-provisioned VM: the instance exists but provisioning never
  # finished, so a plain start cannot stage the agent. Recreate it.
  if ! provisioned; then
    echo ">> $VM exists but provisioning is incomplete; recreating" >&2
    limactl delete --force "$VM" 2>/dev/null || true
    create_vm
  fi
fi

if ! provisioned; then
  echo "x $VM provisioning marker $PROVISION_MARKER missing after ensure" >&2
  exit 1
fi
echo "   + $VM running and provisioned"
