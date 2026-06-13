# Storage pools

A storage pool is a named place on a single node where that node's VM disks and
its cached disk images live. When you create a VM, its disk is materialized into
a pool on whichever node ends up running it. This page explains the model; for
the commands to list, create, and delete pools see the
[Storage pools guide](../guides/storage-pools.md).

## A pool name is a concept, not one object

The single most important thing about pools: **a pool name is multi-instance and
scoped to a node.** A pool called `default` on `node-a` and a pool called
`default` on `node-b` are two separate pools that happen to share a name. They
have their own UUIDs, their own directories, and their own free space.

This mirrors the Kubernetes split between a StorageClass and a PersistentVolume:

| Otherix | Kubernetes analogue | What it is |
|---|---|---|
| Pool **name** (e.g. `default`) | StorageClass | A cluster-wide concept - "a pool called X exists on these nodes" |
| Pool **instance** (a UUID) | PersistentVolume | One concrete pool on one node, with a path and capacity |

The API reflects both shapes. Asking for a pool **by name** returns an
aggregated view across every node that has it; asking **by UUID** returns the one
flat per-instance pool. So pool names are deliberately *never* globally unique -
designing around a single global pool object is the one mental model to avoid.

Because a pool lives on a node, a VM that wants a particular pool can only run
where that pool exists. If you pin a VM to a node that does not have the
requested pool, the VM stays `pending` with the reason `pool_not_on_node` rather
than failing outright - the scheduler is telling you the placement and the
storage disagree.

## The cluster default pool

So that `otherix vm create` works without forcing you to name a pool every time,
the control plane keeps a cluster-wide default pool name in its settings
(`default_pool_name`, shipped as `default`). As each node becomes ready the
control plane auto-provisions a pool of that name on it, under the first allowed
path prefix. A VM created with no `--pool` lands in the default pool on its
chosen node.

Setting the default pool name to an empty string opts out of auto-provisioning -
then you create and name every pool yourself.

## A pool has desired and observed state

Pools follow the same desired/observed control loop as VMs (see
[Desired vs observed state](desired-vs-observed.md)). The pool row you create is
*desired* state: a name, a node, a type, and a path. The agent on that node
reports *observed* state back over the heartbeat - how much space is actually
free, whether the pool is `ready`, and what disk images it currently has cached.

Storage pools were the first resource built on this per-resource reconciliation
skeleton, so the pattern - desired row in, observed status out, retried until it
converges - is most visible here.

## The image cache lives in the pool

Otherix has [no template entity](../index.md): a VM is built directly from an
image URL. The downloaded image is cached **inside the pool**,
keyed by the image's basename. The first VM on a node that references a given
image materializes it into that node's pool with `IfNotPresent` semantics;
subsequent VMs on the same node and pool reuse the cached copy instead of
downloading again.

That cache is observed state, not something you manage directly. It shows up in
the per-pool image inventory the agent reports, which you can see on
`otherix pool get`. Deleting a pool that still holds cached images is fine - the
cache is not a blocker; only attached VM disks are.

## Where a pool can live

A pool's `type` describes its backend. Today there is exactly one type,
`local_dir`: a directory on the node's filesystem. Other backends (network or
block storage) are a future extension, not present yet.

Where that directory may live is bounded by the control plane, not the operator.
A path allowlist (`allowed_path_prefixes`, defaulting to
`/var/lib/otherix/pools/`) gates pool creation: a pool path that does not sit
under an allowed prefix is rejected. This keeps pools inside the conventional
runtime-state location and out of arbitrary host paths.

## See also

- [Storage pools guide](../guides/storage-pools.md) - listing, creating,
  inspecting, and deleting pools, and managing the default pool.
- [Desired vs observed state](desired-vs-observed.md) - the control loop pools
  share with VMs.
