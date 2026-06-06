# Desired vs observed state

When you run `otherix vm get`, the output shows two parallel views of the same
VM. Understanding which is which explains why a freshly-created VM reads
`creating` for a few seconds, and how you tell whether the runtime has caught up
with a change you just made.

Otherix uses the same desired/observed control loop as Kubernetes: you write
*desired* state to the control plane, and each node's agent reports *observed*
state back. The two are reconciled on a periodic heartbeat. For the wider
picture see the [Architecture](../architecture.md) overview.

## Desired state - what you asked for

The **top-level** fields on a VM are desired state. They live in the control
plane's etcd store and only change when you change them (via `otherix vm create`,
`otherix vm set`, or a manifest). They are your intent:

| Field | Meaning |
|---|---|
| `desired_phase` | `running`, `stopped`, or `deleted` - the lifecycle state you want |
| `cpu_cores` (vCPUs) | requested virtual CPU count |
| `memory_mib` | requested memory |
| `image_url`, `image_sha256`, `image_format` | the disk image the VM is built from |
| `architecture`, `firmware_id` | target CPU architecture and firmware |

Patching any of these changes what you want. It does not, by itself, change what
is running - the agent has to converge to the new intent first.

Each VM also carries a `generation` counter. The control plane bumps it every
time desired state changes, so it acts as a version stamp for your intent.

## Observed state - what the agent reports

The nested **`status`** object is observed state: what the agent on the owning
node last reported about the actual QEMU process. It is never set by you. It
carries the runtime view - current node, conditions, and live metrics such as
CPU usage and memory in use.

The headline `status.phase` is *projected*, not stored. The control plane derives
the user-facing string from the observed runtime row each time you read the VM:

| Projected `status.phase` | Condition |
|---|---|
| `creating` | no runtime reported yet, or the agent's create task is still in flight |
| `running` | QEMU is up and running |
| `paused` | guest paused (QMP `stop`) |
| `stopped` | guest powered off |
| `error` | the agent reported a runtime error |
| `orphaned` | the owning node was force-removed; runtime is no longer tracked |
| `gone` | the VM has been deleted |

!!! note "Internal runtime fields are not exposed"
    On-node details (qemu pid, QMP socket path, raw VNC/SPICE ports, file paths)
    stay inside the agent. Console access goes through a dedicated token-issuing
    endpoint, not these fields.

## Telling whether the runtime caught up

`generation` (desired) and `status.observed_generation` (observed) together tell
you whether the agent has applied your latest change:

```text
generation == status.observed_generation   → runtime is in sync with your intent
generation >  status.observed_generation   → a change is still propagating
```

Because observed state arrives over the heartbeat (every ~30s by default),
`status` lags top-level fields briefly after any change. On each node the
reconcilers diff desired against observed (every ~10s, or immediately when a new
heartbeat payload lands) and issue the corrective QEMU operation - for example,
`desired_phase=running` with an observed `stopped` VM triggers a start. They
retry until observed converges to desired.

## Sync vs async operations

Whether a change shows up instantly or after a poll depends on how the operation
is carried out:

- **Sync (immediate, `200`)** - pause, resume, and reset. These are a single fast
  QMP call to the agent; `status` reflects them right away.
- **Async (`202` + task)** - create, delete, start, stop, poweroff, reboot, and
  resize. The API writes a task plus a job, returns a task id, and the work runs
  through the dispatcher and the agent reconcilers. Poll `GET /v1/tasks/{id}`
  (or pass `--wait`) to follow it, then re-read the VM to see the converged
  `status`.

In short: top-level fields change the moment you patch them; `status` follows
once the agent has done the work and reported back.
