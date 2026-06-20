# Create and manage VMs

Create VMs directly from an image URL and drive their full lifecycle
with the `otherix vm` CLI (the control-plane `/v1/vms` surface). VMs
are name-keyed: every command takes the VM name as its positional
argument; UUID literals are rejected by the server with
`400 validation_failed`.

There is no template entity. A VM is created from an image reference;
the agent owns a content-addressed image cache and materializes the URL
on first use. For how that cache works - pinning, pull policy, cross-node
peer pull, and eviction - see [Images](../concepts/images.md). To capture
and recreate VMs from snapshots, see the [Snapshots guide](snapshots.md).

## Create

```bash
otherix vm create web-1 \
  --image-url https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-arm64.img \
  --arch arm64 \
  --vcpus 2 --memory-mb 2048 \
  --wait
```

`vm create` is **async**: the VM is admitted in the `pending` phase and
the command returns immediately. `--wait` blocks until the VM reaches
the `running` phase.

```text
created vm=<name> status=<phase>
.....
vm running name=<name>
```

### Flags

`--image-url` and `--arch` are the only required flags.

| Flag | Default | Notes |
|---|---|---|
| `--image-url` | - | Source image URL to download and boot from. Required. |
| `--arch` | - | `amd64` or `arm64`. Required. |
| `--image-sha256` | (unset) | Expected sha256; verified after download. Pins the image to exact content. |
| `--pull-policy` | `if-not-present` | `if-not-present` reuses a cached image for the URL; `always` forces a fresh re-fetch from `--image-url`. See the warning below. |
| `--firmware` | (unset) | Firmware name. Mutually exclusive with `--firmware-id`. |
| `--firmware-id` | (unset) | Firmware uuid. Mutually exclusive with `--firmware`. |
| `--format` | (server default) | Disk format, e.g. `qcow2` or `raw`. |
| `--disk-gib` | (image virtual size) | Root disk size in GiB. |
| `--pool` | (cluster default) | Storage pool name or uuid. When omitted the server resolves the cluster default-pool; if none is set it returns `400 default_pool_not_set`. |
| `--node` | (scheduler picks) | Placement hint - node name or uuid. The VM is still admitted as `pending`; if the pool is not present on the requested node it stays pending with `status.reason=pool_not_on_node` (visible via `vm get`). |
| `--network` | (unset) | Network name or uuid to attach one NIC. Bridge or overlay are both accepted; an unknown name becomes a pending scheduling reason at bind, not an admission error. When omitted the VM has no NIC and the agent falls back to SLIRP networking. |
| `--vcpus` | `2` | vCPU count (1..128). |
| `--memory-mb` | `2048` | Memory in MiB (128..524288). |
| `--user-data` | (unset) | Path to a `#cloud-config` user-data YAML, or `-` for stdin. Mutually exclusive with `--no-cloud-init`. |
| `--network-config` | (unset) | Path to a cloud-init network-config YAML (netplan v2), or `-` for stdin. Mutually exclusive with `--no-cloud-init`. |
| `--no-cloud-init` | `false` | Explicitly disable cloud-init. Mutually exclusive with `--user-data` and `--network-config`. |
| `--wait` | `false` | Block until the task reaches terminal status. |
| `--wait-timeout` | `10m` | Max wait when `--wait` is set. |

When `--pool` is omitted the cluster default-pool reference is used and
the scheduler picks the (node, pool instance) target. A `--node` hint
pins placement to exactly that node.

!!! warning "A mutable URL is not re-fetched under the default pull policy"
    The default `--pull-policy if-not-present` follows Kubernetes
    `IfNotPresent` semantics: once the cluster has imported an
    `--image-url`, later unpinned creates of the same URL reuse the
    cached image and do **not** re-download. If the URL is a rolling tag
    whose content was republished (for example a
    `noble-minimal-cloudimg-arm64.img` that upstream rebuilt), the new
    VM silently boots the previously-cached image, not the current one.

    To always boot the current content of a mutable URL, pass
    `--pull-policy always`, which forces a fresh fetch from the URL. For
    an exact, reproducible image instead, pin it with
    `--image-sha256 <hex>`: the download is verified against the digest
    and the same digest is reused everywhere.

## Inspect

```bash
# List VMs visible to the caller. UUIDs hidden by default.
otherix vm list

# Reveal the UUID column.
otherix vm list --show-ids
```

```text
NAME       STATUS   ARCH   NODE      POOL      NETWORK   AGE
web-1      running  arm64  node-1    default   -         3m
```

`vm list` is cursor-paginated (`--limit`, `--cursor`) and filters
server-side with `--pool`, `--node` (by agent-reported current
location, not pinned intent), and `--status`. Output: `--output`
(`-o`) accepts `table` (default), `json`, or `yaml`.

```bash
# Single VM by name.
otherix vm get web-1

# Round-trip a manifest you can re-apply with `otherix create -f`.
otherix vm get web-1 -o yaml
```

`vm get` defaults to a multi-line key/value view; `-o json` echoes the
raw server envelope; `-o yaml` projects a declarative manifest. (Some
create-time-only fields do not round-trip through `-o yaml` - keep your
source manifest as the record of what you applied.)

## Lifecycle

Mutating operations split into **sync** (the call returns the refreshed
VM directly) and **async** (the call returns a task id; pass `--wait`
to block).

| Command | Mode | Effect |
|---|---|---|
| `vm start <name>` | async | Boot a stopped VM. Sets `desired_phase=running`. Idempotent. |
| `vm stop <name>` | async | Graceful ACPI shutdown (QMP `system_powerdown`). On timeout the task fails with `stop_timeout`. |
| `vm stop <name> --force` | async | CLI-level dispatch to `poweroff` (hard shutdown, guest not notified). |
| `vm poweroff <name>` | async | Hard power-off (QMP `quit`, then SIGKILL). Sets `desired_phase=stopped`. |
| `vm reboot <name>` | async | Graceful stop+start cycle; the QEMU PID changes. |
| `vm reset <name>` | sync | QMP `system_reset` - the reset button. Runtime identity preserved. |
| `vm pause <name>` | sync | QMP `stop` - vCPUs freeze, memory stays resident. |
| `vm resume <name>` | sync | QMP `cont` - continue a paused VM. |
| `vm delete <name>` | async | Tear down and soft-delete. Prompts for confirmation unless `--force`. |

Sync commands (`pause`, `resume`, `reset`) accept `--output`
(`text`/`json`) and return the refreshed VM view; re-issuing an op the
VM is already in (e.g. pausing a paused VM) returns `409 conflict`.

Async commands (`start`, `stop`, `poweroff`, `reboot`, `delete`)
accept `--wait` and `--wait-timeout` (default `10m`).

`vm delete` is async only when there is an agent-side teardown to run.
A `pending` (unscheduled) VM is deleted synchronously CP-side: the
command prints `deleted vm=<name>` and returns with no task id to poll.

```bash
otherix vm stop web-1 --wait
otherix vm stop web-1 --force          # hard poweroff
otherix vm reboot web-1 --wait
otherix vm delete web-1 --wait --force
```

## The async task model

Async operations return `202 Accepted` with a task id rather than
blocking. The client polls `GET /v1/tasks/{id}` until the status
reaches `success`, `failed`, or `cancelled`.

The CLI does this for you with `--wait`: it prints one dot per poll to
stderr (so stdout stays parseable) and exits non-zero on a failed task,
surfacing the task error as `task <id> failed: <code>: <message>`.
Without `--wait` the command prints `<op> task=<id> status=<status>`
and returns immediately - poll the task yourself, or re-run the command
with `--wait`.

## See also

- [Console and logs](console-and-logs.md)
- [Join a node](join-a-node.md)
- [Declarative manifests](declarative-manifests.md)
- [Storage pools](storage-pools.md)
- [Networks](networks.md)
