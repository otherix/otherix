# Backups

All control-plane state - users, nodes, VMs, storage pools, networks, the job
queue, the cluster CA row - lives in the embedded etcd. Backing up Otherix means
snapshotting etcd. The api-server ships a built-in periodic snapshot worker;
restore is a manual operator procedure (see below).

## Periodic snapshots

The backup worker is configured under `workers.backup` in `api.yaml`:

| Key | Default | Meaning |
| --- | --- | --- |
| `workers.backup.enabled` | `false` | Master switch. Off by default - a backup needs an operator-chosen destination |
| `workers.backup.interval` | `6h` | Time between snapshots |
| `workers.backup.dir` | (unset) | Directory snapshots are written to. **Required** when enabled |
| `workers.backup.retention` | `7` | Number of newest snapshots to keep; older ones are pruned |

```yaml
workers:
  backup:
    enabled: true
    interval: 6h
    dir: /var/backups/otherix-etcd
    retention: 7
```

The config fails fast at startup if `enabled: true` with an empty `dir` - the
worst failure mode is an operator believing backups run when they do not. A
`retention` or `interval` below zero is also rejected.

On each tick the worker streams a point-in-time snapshot of its own etcd member
to `dir`, then prunes to the newest `retention` files. The snapshot is the
member's backend state (a bbolt database with an etcd integrity footer),
restorable into a fresh data directory.

### Snapshot file naming and retention

Files are named:

```
otherix-<UTC-timestamp>.db        e.g. otherix-20260606T031500Z.db
```

The timestamp is sortable, so a lexical sort is chronological. Retention keeps the
newest `retention` files (by name) and deletes the rest; non-snapshot files in the
directory are never touched. Each write is atomic (written to `*.tmp`, fsynced,
renamed, then the parent directory is fsynced), so a crash mid-write never leaves a
truncated file at the canonical path.

A snapshot failure fails that tick (logged, retried next interval). A prune failure
is logged but does not fail the tick, since the fresh snapshot already succeeded.

!!! note "Where the snapshot is taken"
    The worker snapshots the **local** member. In an HA cluster, every replica
    with this worker enabled snapshots its own copy. Enabling it on one replica is
    enough for a recoverable backup; enabling it on a follower avoids perturbing
    the leader.

### Manual snapshot

To take an out-of-band snapshot you can run the periodic worker's settings against
any member, or use the upstream `etcdctl snapshot save` against a member's client
URL. The built-in worker is the supported path; `etcdctl` is available for ad-hoc
captures.

## Restore (manual procedure)

!!! warning "Restore is destructive and not automated"
    There is no automated restore command in Otherix. Restoring etcd from a
    snapshot **replaces** the live cluster state and is irreversible. Restore only
    to recover from data loss (lost quorum with no recoverable members, corrupted
    data dir), never as a routine operation. Take a fresh snapshot of the current
    (even degraded) state before you begin, so a mistaken restore can itself be
    undone.

The conceptual procedure follows the standard etcd snapshot-restore flow. Treat
it as operator runbook, not a turnkey command:

1. **Stop every `otherix-api` replica.** A restore must not race a live member.
2. **Restore the snapshot into a fresh member data directory** on each replica,
   using the upstream `etcdctl snapshot restore` (or `etcdutl`) tooling. The
   restore initializes a new member data dir from the snapshot; you provide the
   member name, peer URL, and initial-cluster matching your topology. For a
   single-node restore this is one member; for HA, restore a consistent
   member set with the same `--initial-cluster-token` you run under.
3. **Point each replica's `etcd.data_dir` at the restored directory** (or restore
   in place after moving the old dir aside).
4. **Restart the replicas.** They form a cluster from the restored state.

!!! warning "Keep the cluster CA consistent with etcd"
    The cluster CA lives in **two** places that must agree: the on-disk CA files
    (`cluster_ca.cert_file` / `key_file`) and the `ca_certs` row inside etcd.
    Restoring etcd from a snapshot brings back whatever CA was active when the
    snapshot was taken. If the on-disk CA no longer matches the restored etcd CA,
    the api-server refuses to start (CA divergence). When restoring, make sure the
    on-disk CA files correspond to the snapshot, or restore both together. See
    [Certificates](certificates.md) for the divergence rules and `make etcd-reset`,
    which wipes the dev data dir and on-disk CA together for a clean slate.

After a restore, verify the cluster with `GET /v1/cluster/members` and confirm
expected resources are present (`otherix node list`, `otherix vm list`). Nodes may
re-register and flip back to `ready` within a heartbeat cycle.
