# Node maintenance: cordon, drain, uncordon

Otherix nodes have a maintenance lifecycle separate from their health. Three
operator actions manage it:

| Action | Effect | Sync/async |
| --- | --- | --- |
| `cordon` | Mark the node unschedulable. Running VMs stay; no new VM is placed on it. | sync |
| `drain` | Cordon, then live-migrate every VM off onto other eligible nodes, bounded by a timeout. | async (task) |
| `uncordon` | Return a cordoned node to normal scheduling. | sync |

Drain never destroys or stops a VM. A VM that cannot be moved is left running on
the node and reported. After a drain finishes - whether it emptied the node or
ran out of time - the node is left **cordoned**, so nothing new lands on it
until you decide what to do next.

All three actions require an operator (or admin) role.

## Cordon a node (stop new placements)

```console
$ otherix node cordon node-1
node-1: cordoned
```

Existing VMs keep running. The scheduler will not place new VMs - or pick the
node as a migration target - while it is cordoned. Cordon is idempotent:
cordoning an already-cordoned node is a no-op.

## Drain a node (evacuate for maintenance)

`drain` is asynchronous. The control plane cordons the node and enqueues a drain
task that live-migrates each VM to an eligible node, re-evaluating which VMs are
still on the node on every pass until it empties or the budget expires.

```console
$ otherix node drain node-1
drain node=node-1 task=b3f1c8a2-9d4e-4f1a-8c2b-1e5f6a7d8c90 status=pending
```

The command returns immediately with a task id you can poll:

```console
$ otherix task get b3f1c8a2-9d4e-4f1a-8c2b-1e5f6a7d8c90
```

Pass `--wait` to block until the drain reaches a terminal status and render the
outcome:

```console
$ otherix node drain node-1 --wait
drain node=node-1 task=b3f1c8a2-9d4e-4f1a-8c2b-1e5f6a7d8c90 status=pending
drain node-1: success
  migrated=3 remaining=0 in_flight=0
```

The result line reports `migrated` (VMs that left the node), `remaining` (VMs
still on it at finalize), and `in_flight` (the subset of remaining still
migrating - they finish on their own). On success all VMs left and the node ends
cordoned and empty, ready for maintenance or decommission.

`--wait` only bounds how long the CLI blocks, not the drain itself. With
`--wait-timeout` reached, the drain keeps running server-side and the CLI prints
how to resume watching.

Override the timeout for a slow or large node. Resolution order is the flag, then
the cluster default, then the built-in floor of 10 minutes:

```console
$ otherix node drain node-1 --timeout 30m --wait
```

## Scenario: some VMs cannot be moved

If the cluster has no room for a VM (full, no architecture match, or the VM is
not live-migratable), drain moves what it can and leaves the rest running. When
the timeout expires the node is left cordoned and the task reports what stayed:

```console
$ otherix node drain node-4 --timeout 10m --wait
drain node=node-4 task=c4a2d7e1-3f8b-4e9a-bb12-7d6e5f4a3b21 status=pending
drain node-4: failed
  migrated=1 remaining=1 in_flight=0
  reason: drain_timeout
  stuck VMs (no eligible target):
    - vm-b
  node left cordoned; add capacity and re-run drain, or migrate/stop the remaining VMs manually
```

The node is cordoned and `vm-b` is still running on it. Your options:

- add capacity or free a node, then re-run `otherix node drain node-4`;
- migrate `vm-b` somewhere specific by hand with
  `otherix vm migrate vm-b --node node-3`;
- if you are decommissioning the hardware, `otherix node delete node-4 --force`.

`node delete --force` orphans the VM runtime on the node without changing what
you asked for (the VM's desired state is preserved and it can be rescheduled),
whereas drain never stops a running VM.

## Cancel an in-progress drain

A drain is a task, so cancel it with `task cancel` using the task id from the
drain command:

```console
$ otherix node drain node-1
drain node=node-1 task=b3f1c8a2-9d4e-4f1a-8c2b-1e5f6a7d8c90 status=pending
$ otherix task cancel b3f1c8a2-9d4e-4f1a-8c2b-1e5f6a7d8c90
```

The drain stops starting new migrations; migrations already in flight finish on
their own, and the node is left cordoned.

## Return a node to service

```console
$ otherix node uncordon node-1
node-1: ready
```

Uncordon is idempotent: uncordoning a ready node is a no-op. It is rejected for a
node that is pending, draining, unreachable, or gone.

## How drain interacts with live migration

Drain uses the same live-migration data path as `otherix vm migrate` (see
[Live migration](../guides/live-migration.md)). Each evacuated VM is migrated
with reason `drain`, visible on the migration record. When a drain's timeout
expires, migrations already in progress are **not** cancelled - they finish under
their own timeouts. Drain only stops *starting* new ones.

Drain initiates a migration for a VM only when a target node is available, so a
VM with no eligible target is never enqueued - it is simply counted as remaining.

On a control plane running a single process, several VMs evacuated in the same
pass may land on one target node (this is correct - placement never
oversubscribes a node). A high-availability deployment spreads the work across
replicas. Operators who want strict spread across targets can drain serially or
migrate individual VMs by hand.

## Tuning

Two cluster settings tune drain behaviour:

| Setting | Default | Meaning |
| --- | --- | --- |
| `drain_timeout_seconds` | `600` | Overall budget for a drain before the node is left cordoned with whatever remains. Overridable per-drain with `--timeout`. |
| `drain_max_concurrent_migrations` | `2` | How many VMs a single drain migrates at once, to avoid saturating the network. |

The control plane also caps how many nodes may be draining simultaneously, set by
the `workers.max_concurrent_drains` configuration key (default `2`). This keeps a
fleet-wide drain from exhausting the shared worker pool. A drain request beyond
the cap is rejected with `409` and the message "too many drains in progress,
retry shortly" - wait for an in-progress drain to finish, then retry.
