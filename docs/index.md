# Otherix

Open-source, self-hosted control plane for managing virtual machines on
KVM/QEMU clusters - deployed in your own datacentre or homelab.

Otherix runs VMs on a fleet of bare-metal nodes, each controlled by an Otherix
agent that talks to QEMU directly (no libvirt). The control plane keeps the
cluster's desired state in an embedded etcd member; agents report observed state
through a heartbeat. The split mirrors the Kubernetes pattern: a declarative API,
generation / observed-generation bookkeeping, and reconciliation loops - applied
to long-lived, stateful VMs instead of stateless containers.

<div class="grid cards" markdown>

- :material-rocket-launch: **[Quickstart](get-started/quickstart-single-node.md)**
  Bring up a single-node cluster and boot your first VM.

- :material-sitemap: **[Architecture](architecture.md)**
  How the control plane, agents, etcd, and data plane fit together.

- :material-book-open-variant: **[Guides](guides/create-and-manage-vms.md)**
  Task-focused how-tos: VMs, networks, pools, nodes, RBAC.

- :material-console: **[Reference](reference/cli.md)**
  CLI, REST API, configuration, RBAC matrix, error codes.

</div>

## What makes it different

VMs are not cattle. Disks persist, identities are stable, and the design follows
the workload:

- **Created from an image URL** - no template entity, no registry. The agent
  materializes the image into a content-addressed node cache on first use, and a
  cached image is pulled peer-to-peer between nodes rather than re-downloaded -
  see [Images](concepts/images.md).
- **Snapshots are standalone, replicated artifacts** - content-addressed,
  disk-only, independent of the source VM. Place them in an artifact pool to keep
  N copies across nodes; the cluster re-replicates after a node loss, reclaims
  orphaned blobs automatically, and self-heals corruption - see
  [Snapshots](concepts/snapshots.md).
- **Live migration is peer-to-peer** between agents, with the control plane out of
  the data path. The guest, your open console and `logs -f` session, and the
  network all follow the VM across the move - see [Live migration](guides/live-migration.md).
- **Storage pools, networks, and firmwares are explicit cluster resources**, not
  abstractions over a cloud provider.
- **One stateful service.** Embedded etcd is the only datastore - no Postgres, no
  Redis, no external queue.

## Who this is for

- **Operators / SREs** standing up and running a cluster - start with
  [Installation](get-started/install.md) and [Operations](operations/high-availability.md).
- **Users** creating and managing VMs through the `otherix` CLI - start with the
  [Quickstart](get-started/quickstart-single-node.md) and the [Guides](guides/create-and-manage-vms.md).

!!! note "Documentation in progress"
    This site is being built out. Pages marked *work in progress* are stubs that
    will be filled in; the [Architecture](architecture.md),
    [RBAC matrix](rbac.md), and [Scheduler configuration](scheduler-configuration.md)
    pages already carry full content.
