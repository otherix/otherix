# Snapshots

This guide covers the day-to-day commands for capturing, listing, recreating,
and replicating VM snapshots. For the model behind them - content addressing,
durability, reclamation, self-healing - see
[Snapshots and the artifact store](../concepts/snapshots.md).

A snapshot is **disk-only and crash-consistent**: it copies the VM's disks, not
its RAM. It is captured as a VM sub-resource action and then managed as a
top-level object.

## Capture a snapshot

Snapshots are taken with `vm snapshot`, a sub-resource action on the VM:

```
otherix vm snapshot web-1
otherix vm snapshot web-1 --name golden --description "pre-upgrade baseline"
```

The capture runs asynchronously. Add `--wait` to block until it finishes:

```
otherix vm snapshot web-1 --name golden --wait
```

`--wait-timeout` bounds only how long the CLI blocks; the snapshot keeps
producing server-side regardless. When `--name` is omitted it defaults to
`snap<unix_seconds>` and the chosen name is printed.

## List, inspect, and delete

Snapshots are listed and inspected through the top-level `snapshot` group (they
are *created* only through `vm snapshot`):

```
otherix snapshot list
otherix snapshot list --vm web-1
otherix snapshot list --status ready
otherix snapshot get <id>
otherix snapshot delete <id>
```

`snapshot get` shows the disks, the capture state, the holder nodes, and the
durability status. Deleting a snapshot is asynchronous; when the last snapshot
referencing a blob is gone, that blob is reclaimed automatically (see
[reclamation](../concepts/snapshots.md#automatic-reclamation-no-orphaned-blobs)).

## Recreate a VM from a snapshot

A snapshot is independent of its source VM, so you can recreate from it at any
time, even after the original VM is gone:

```
otherix vm create web-2 --from-snapshot <id>
```

The architecture, format, and firmware come from the snapshot manifest, so you
do not pass `--arch` or image flags. The recreate can run on any node; if the
target node does not already hold the snapshot's disk blob, the cluster pulls it
from a node that does before booting.

### Round-trip through a manifest

`vm get -o yaml` emits a spec carrying `sourceSnapshotID`, which `vm create -f`
applies - so a recreate is reproducible from a file:

```
otherix vm get web-2 -o yaml > web-2.yaml
otherix vm create -f web-2.yaml
```

## Make snapshots survive a node loss

By default a snapshot blob exists only on the node that produced it - one copy.
To replicate it across nodes, place it in an **artifact pool** that declares a
replication factor.

Create a pool and snapshot into it:

```
otherix artifact-pool create artifacts --replication-factor 2
otherix vm snapshot web-1 --artifact-pool artifacts
```

The control plane then keeps the snapshot's blobs on two distinct live nodes and,
if a holder is lost, re-replicates onto a survivor automatically.

| Flag | Values | Meaning |
|---|---|---|
| `--replication-factor` | `1` (default), an integer `>= 1`, or `all` | How many distinct live nodes hold each blob; `all` means every member node. |
| `--members` | `all` (default) or a comma-separated node-name list | Which nodes may back the pool. |

Inspect and change pools:

```
otherix artifact-pool list
otherix artifact-pool get artifacts
otherix artifact-pool update artifacts --replication-factor 3
```

Raising the replication factor re-replicates existing snapshots immediately - you
do not re-take them. Lowering it trims the surplus copies back down on the next
reconcile.

### A cluster default artifact pool

So that `vm snapshot` works without naming a pool each time, set a cluster-wide
default (admin):

```
otherix cluster set-default-artifact-pool artifacts
otherix cluster get-default-artifact-pool
```

With a default set, a `vm snapshot` that omits `--artifact-pool` is tagged into
the default pool and inherits its replication factor.

## Delete an artifact pool

A pool that still has snapshots tagged into it cannot be deleted - the delete is
refused with a conflict until those snapshots are gone (this is deliberate;
deleting the pool must not silently orphan durability policy):

```
otherix artifact-pool delete artifacts
```
