# Snapshots and the artifact store

A snapshot captures a VM's disk state at a point in time as a **standalone,
content-addressed artifact**. It is a first-class API object, not a pile of
`qemu-img` invocations: you can snapshot a VM, delete the VM, and weeks later
recreate a brand-new VM from that snapshot - or delete the snapshot - in any
order, because the snapshot does not depend on the VM it came from. This page
explains the model and the durability, replication, reclamation, and self-healing
that sit behind it. For the commands, see the
[Snapshots guide](../guides/snapshots.md).

## What a snapshot is

A snapshot is a **manifest** plus one or more **blobs**:

- the **manifest** records the VM spec at capture time (architecture, firmware,
  format) and, per disk, the disk's content digest;
- each **blob** is a full-copy, self-contained qcow2 of one disk, named by its
  sha256 content digest.

Snapshots are **disk-only and crash-consistent**: the guest's RAM is not
captured. A snapshot of a running VM is a point-in-time copy of its disks, the
same consistency you would get from a clean power-cut - filesystems with
journaling recover normally. There is no template entity; a snapshot and an
[image](images.md) are the same kind of thing under the hood - immutable,
digest-named disk content.

## Recreate a VM from a snapshot, anywhere

A snapshot is independent of its source VM, so the lifecycle is fully decoupled:

```
otherix vm snapshot web-1 --name golden        # capture
otherix vm delete web-1                         # the source VM is gone
otherix vm create web-2 --from-snapshot <id>    # recreate from the snapshot
```

`vm create --from-snapshot` takes the architecture, format, and firmware from the
manifest, clones the disk blobs in order, and boots a fresh VM. Because snapshot
blobs are peer-pullable (below), the recreate can run on **any** node - if the
target node does not hold the blob, it pulls it from a node that does before
materializing the disk.

The manifest also round-trips through the declarative workflow: `vm get -o yaml`
emits a spec carrying `sourceSnapshotID`, and `vm create -f` applies it, so a
recreate is reproducible from a file.

## The artifact store: content-addressed, peer-to-peer

Every node runs a local **artifact store** of immutable blobs keyed by content
digest. Capturing a snapshot lands its disk blobs in the producing node's store;
the node reports which blobs it holds to the control plane through its heartbeat.

When a blob is needed on a node that does not have it - a recreate on a different
node, or replication (below) - the control plane brokers a **peer pull**: the
consumer node fetches the blob directly from a holder node over mutual TLS,
verifying the content digest as it arrives. The control plane discovers a live
holder and mints a one-time token, but the bytes never pass through it. If no
live node holds the blob, the operation fails cleanly with a retryable
`blob_unavailable` rather than serving partial data.

## Artifact pools and replication

By default a snapshot blob lives only on the node that produced it (one copy). To
make a snapshot survive the loss of that node, you place it in an **artifact
pool** - a cluster-level policy object that sets a **replication factor**:

```
otherix artifact-pool create artifacts --replication-factor 2
otherix vm snapshot web-1 --artifact-pool artifacts
```

| Replication factor | Meaning |
|---|---|
| `1` (default) | One copy. Survives nothing - the blob is gone if its node is. |
| `N` (an integer >= 1) | The control plane keeps the blob on `N` distinct live nodes. |
| `all` | The blob is kept on every member node of the pool. |

An artifact pool is the snapshot analogue of a Kubernetes StorageClass: a
cluster-wide policy, not a place on a disk. The blob itself still lives in the
node's disk pool; the artifact pool only declares *how many copies* the cluster
should maintain and *which nodes* may back it (its membership - `all`, or a
named node list). A cluster-wide **default artifact pool** lets `vm snapshot`
work without naming a pool every time.

Replication is **active and continuous**. A control-plane loop continually
compares each blob's desired copy count against the live nodes that actually hold
it, and copies the blob (via the same peer-pull data path) onto additional nodes
until the target is met. Raising a pool's replication factor re-replicates its
existing snapshots immediately; you do not have to re-take them.

## Durability is observed, never assumed

Otherix never *claims* a snapshot is safe - it reports what is actually true on
disk right now. A snapshot's durability is computed from the number of **live**
nodes that currently report holding its blobs:

| Status | Meaning |
|---|---|
| `durable` | At least the desired number of live nodes hold the blob. |
| `replicating` | Copies are being made toward the target. |
| `degraded` | Fewer holders than desired and no further copy can be made right now. |

If a holder node is lost, the snapshot's durability drops and the replication
loop automatically copies a healthy replica onto a surviving node to restore the
target - no operator action required. This is the same desired-vs-observed
control loop the rest of Otherix uses (see
[Desired vs observed state](desired-vs-observed.md)).

## Automatic reclamation (no orphaned blobs)

Blobs are **reference-counted** by the snapshots that use them, and the control
plane reclaims storage automatically - you never sweep blobs by hand:

- **Orphaned blobs.** When the last snapshot that references a blob is deleted,
  the blob has zero references and is reclaimed from the nodes that hold it.
- **Over-replicated blobs.** If a pool's replication factor is lowered, the
  surplus copies beyond the new target are trimmed back down to it.

The collector is **fail-closed** - this is the deliberate design point, because
deleting a blob is irreversible:

- it never deletes a blob that any surviving snapshot still references;
- it never drops the last live copy of a referenced blob;
- it re-derives the reference count and live-holder count at the moment of
  deletion, and aborts if removing a copy would breach the durability target;
- on any uncertainty (a transient read error, incomplete inventory) it does
  nothing and leaves the copy, rather than risk destroying data.

In other words, reclamation errs toward keeping an extra copy, never toward
losing one.

## Self-healing against corruption

Observing that a node holds a blob by *filename* is not the same as that blob
still being *correct*. A background **scrubber** periodically re-hashes stored
blobs and compares them to their content digest. A copy that no longer matches
(bit-rot, a torn write) is removed; its disappearance lowers the observed holder
count, and the replication loop copies a healthy replica back from a good holder.
A single corrupt copy is therefore detected and replaced without operator
involvement, and a corrupt blob is never served to a recreate (the consumer
re-verifies the digest on pull as a second line of defense).

## Crash safety

The snapshot disk tier is hardened against power loss and abrupt agent death:

- blob writes are fsynced and made visible by atomic rename, so a power cut can
  never leave a valid-looking blob with garbage content counted as a durable
  copy;
- interrupted captures leave only scratch files, which are swept on agent boot
  and periodically - they never accumulate;
- a backup left dangling inside a running guest by an agent restart is reaped on
  the next capture;
- deleting a snapshot whose blob an in-flight recreate is still sourcing is
  refused with a conflict, so a delete cannot pull content out from under a
  running create.

The guiding principle throughout is the same as reclamation: irreversible actions
fail toward doing nothing.

## Security

Snapshots are owned resources: you only see and act on your own (cross-user
visibility returns "not found", never a permission leak). Snapshot names and blob
digests are strictly validated on both the control-plane edge and the agent
before they are ever used to build a filesystem path, and a recreate from another
user's snapshot is refused by an ownership check - a snapshot blob cannot be used
to read another tenant's data.
