# Storage pools

A storage pool is a directory on a node where Otherix keeps VM disks and the
agent-owned image cache. Pools are managed through the `otherix pool` command
group and are **admin-only** to create or delete (`storage_pool:manage`); every
authenticated role can read them.

## Pools are per-node

A pool **name** is a cluster-wide concept, but each name maps to one or more
per-node instance rows: the same name can exist on multiple nodes. Two mental
models from Kubernetes apply:

- A pool **name** is like a StorageClass - the cluster-wide handle a VM
  references.
- A pool **instance** (one row on one node) is like a PersistentVolume - the
  concrete directory on a specific host.

This shapes the whole CLI surface below: `list` shows one row per instance,
`get <name>` aggregates instances under a name, and `create` / `delete` operate
on a single `(name, node)` instance per call.

## Listing pools

`otherix pool list` is a cursor-paginated list with **one row per per-node
instance**, so a name that lives on three nodes appears three times:

```bash
otherix pool list
# NAME     NODE    TYPE       PATH                          AVAILABLE  STATUS  DEFAULT  AGE
# default  node-a  local_dir  /var/lib/otherix/pools/default    80.1GiB    ready   yes      2d
# default  node-b  local_dir  /var/lib/otherix/pools/default    79.4GiB    ready   yes      2d
```

The `DEFAULT` column reflects the cluster default pool (see below). Filter with
`--node <name|uuid>` or `--type local_dir`; add `--show-ids` to include instance
UUIDs, or `-o json` / `-o yaml`.

## Inspecting one pool: name vs UUID

`otherix pool get` is dual-shape and the CLI picks the form from the argument:

- **By name** -> the aggregated view (every per-node instance plus the
  cluster-default flag):

  ```bash
  otherix pool get default
  ```

- **By UUID** -> the flat single-instance view for one node, including the
  agent-reported runtime detail (available bytes, disk pressure, reconciliation
  status, and the image cache):

  ```bash
  otherix pool get 8b1d... # an instance UUID
  ```

Both forms support `-o json` and `-o yaml` (the YAML form projects an
apply-ready manifest, see [Declarative manifests](declarative-manifests.md)).

## Creating a pool

`otherix pool create` registers **one `(name, node)` instance per invocation**.
There is no batch flag; to put the same name on several nodes, run the command
once per node (or use a [manifest](declarative-manifests.md) with `nodeList`).

```bash
otherix pool create default \
  --node node-a \
  --path /var/lib/otherix/pools/default
```

| Flag | Required | Meaning |
| --- | --- | --- |
| `--node` | yes | Owning node **name** (a UUID literal is rejected by the server). |
| `--path` | yes | Absolute filesystem path on that node. |
| `--type` | no | Pool type; only `local_dir` is supported today (the default). |
| `--wait` | no | Poll until reconciliation reaches `ready` or `failed` (60s timeout). |

Creating the same `(name, node)` twice is an error - the command is not
idempotent. Pre-check with `otherix pool get <name>` if a seed script needs
re-run safety.

### The allowed-path gate

The Control Plane only accepts pool paths under an operator-configured
allowlist, defaulting to `/var/lib/otherix/pools/`. A `--path` outside every
configured prefix is rejected with `path_not_allowed`. The prefix match is
boundary-safe: `/var/lib/otherix/pools` does not match `/var/lib/otherix/pools-evil/`.
The path must be absolute (start with `/`) and at most 1024 bytes.

## The cluster default pool

A VM created without an explicit `--pool` resolves the **cluster default pool**
by name. Manage it with the `otherix cluster` commands (admin-only,
`cluster:manage` to change):

```bash
otherix cluster get-default-pool          # prints the current default, or a hint if unset
otherix cluster set-default-pool default  # set by pool NAME (not UUID)
otherix cluster unset-default-pool        # clears it (prompts unless --force)
```

`set-default-pool` takes a pool **name** only; the server validates that at
least one instance with that name exists (case-insensitive) and otherwise
returns `pool_not_found`. Once set, `otherix vm create` without `--pool` (and a
VM manifest without `spec.pool`) uses it. See
[Create and manage VMs](create-and-manage-vms.md) for the VM side.

## The agent-owned image cache

Otherix has no template entity. A VM is created directly from an image URL, and
the **agent** owns a per-pool image cache: on first use it materializes the
image under `{poolPath}/images/{basename}` (with a `.sha256` sidecar) using
Kubernetes `IfNotPresent` semantics. Subsequent VMs reusing the same image hit
the cache instead of re-downloading.

This cache is **observed runtime state** reported by the agent through
heartbeat, so it surfaces on the pool view rather than being something you
manage directly. `otherix pool get` lists cached images on each instance:

```bash
otherix pool get 8b1d...
# ...
# images:
#   - ubuntu-24.04-minimal-cloudimg-arm64.img (sha 7f3c1a9b0e2d, 612.0MiB)
```

The image cache is **not** a delete blocker - you do not need to clear it before
deleting a pool.

## Deleting a pool

`otherix pool delete` removes a single instance. The argument can be a UUID
(targets one row directly) or a name; for a name that exists on multiple nodes,
disambiguate with `--node`:

```bash
otherix pool delete default --node node-a
otherix pool delete <instance-uuid>
otherix pool delete pool-dev --force      # skip the confirmation prompt
```

A bare name that maps to multiple instances is ambiguous; the command then
lists the per-node instances and the two ways to retry (by `--node` or by UUID).

Deletion is refused with `409 conflict` when the pool still backs VM disks. The
failure output lists the blocking `vm_disks` count and the names of the VMs
holding a disk on the pool. There is **no force-delete** for pools by design:
remove or migrate the dependent VMs first, then retry.
