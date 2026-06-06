# Declarative manifests

Otherix supports a kubectl-apply-style workflow: describe your resources in
YAML, then create or delete the whole set with one command. Manifests are an
alternative to the imperative `otherix network create` / `pool create` /
`vm create` flags - the same Control Plane endpoints, driven from a file.

```bash
otherix create -f cluster.yaml          # create everything in the file
otherix delete -f cluster.yaml --force  # tear it back down
```

## Manifest shape

Every document carries an `apiVersion`, a `kind`, a `metadata.name`, and a
`spec`. The only accepted `apiVersion` is `otherix/v1`. The three supported
kinds are `Network`, `StoragePool`, and `VM` (case-sensitive).

```yaml
apiVersion: otherix/v1
kind: Network
metadata:
  name: demo-net
spec:
  type: bridge
  bridgeName: otdemo0
```

A single file may hold many documents separated by the YAML `---` marker. You
can also pass `-f` more than once, or read from stdin with `-f -`:

```bash
otherix create -f net.yaml -f pool.yaml -f vms.yaml
cat cluster.yaml | otherix create -f -
```

!!! note "Unknown fields are rejected"
    A typo in a spec field (for example `bridge_name` instead of `bridgeName`)
    fails the whole command at the CLI edge rather than being silently dropped.
    Field names are camelCase.

## Apply ordering

A VM references a network and a storage pool by name, so those must exist
first. `otherix create -f` always orders the documents
**Network -> StoragePool -> VM** regardless of their order in the file, so name
references resolve. `otherix delete -f` runs the reverse order
(**VM -> StoragePool -> Network**) so dependents go first.

Both commands are best-effort: each document is reported independently and the
command exits non-zero if any document failed. A network that still has VM NICs
attached is reported as blocked and skipped, never forced.

## A full example

This manifest declares a managed bridge network and a VM attached to it, with
inline cloud-init:

```yaml
apiVersion: otherix/v1
kind: Network
metadata:
  name: demo-net
spec:
  type: bridge
  managed: true
  bridgeName: otdemo0
  mtu: 1500
---
apiVersion: otherix/v1
kind: VM
metadata:
  name: demo-vm
spec:
  imageURL: https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-arm64.img
  arch: arm64
  network: demo-net
  vcpus: 2
  memoryMB: 2048
  diskGiB: 20
  # inline cloud-config, sent as user_data at create time:
  cloudInit: |
    #cloud-config
    package_update: true
    users:
      - name: otherix
        sudo: ALL=(ALL) NOPASSWD:ALL
```

Apply it, blocking until the VM create task and any pool reconciliation finish:

```bash
otherix create -f cluster.yaml --wait --wait-timeout 5m
```

`--wait` blocks on the asynchronous resources (VM create tasks and storage-pool
reconciliation); networks are created synchronously and need no wait.
`--wait-timeout` defaults to `5m`. Use `--dry-run` to print the resolved plan
without contacting the server:

```bash
otherix create -f cluster.yaml --dry-run
# would create Network/demo-net
# would create VM/demo-vm
```

### Spec fields by kind

| Kind | Required | Optional |
| --- | --- | --- |
| `Network` | `type` | `bridgeName`, `managed`, `egress`, `subnet`, `gateway`, `mtu`, `vlan` |
| `StoragePool` | `path`, one of `node` / `nodeList` | `type` |
| `VM` | `imageURL`, `arch` | `imageSHA256`, `firmware` / `firmwareID`, `format`, `diskGiB`, `vcpus`, `memoryMB`, `pool`, `network`, `node`, `cloudInit` / `cloudInitDisabled` |

Within a kind, a few fields are mutually exclusive: `node` and `nodeList`
(StoragePool); `firmware` and `firmwareID`, and `cloudInit` and
`cloudInitDisabled` (VM). When `vcpus` or `memoryMB` is omitted the CLI applies
the same defaults as `vm create` (2 vCPUs, 2048 MiB). See
[Networks](networks.md), [Storage pools](storage-pools.md), and
[Create and manage VMs](create-and-manage-vms.md) for the meaning of each field.

## StoragePool nodeList fan-out

A storage-pool name is a per-node concept (see [Storage pools](storage-pools.md)).
A single StoragePool document can fan out to one instance per node with
`spec.nodeList`:

```yaml
apiVersion: otherix/v1
kind: StoragePool
metadata:
  name: default
spec:
  type: local_dir
  path: /opt/otherix/pools/default
  nodeList:
    - node-a
    - node-b
    - node-c
```

This is expanded to one create (or delete) per listed node. Use `spec.node` for
a single instance. Duplicate node names in `nodeList` are de-duplicated.

## Round-trip with `get -o yaml`

Every resource type can project a live object back to an apply-ready manifest
with `get -o yaml`, so you can capture current state into a file:

```bash
otherix network get demo-net -o yaml > demo-net.yaml
otherix pool get default -o yaml
otherix vm get demo-vm -o yaml
```

The projection deliberately omits server-assigned fields (id, timestamps,
status, owner, reconciliation) so the output re-applies cleanly.

!!! warning "Some fields do not round-trip"
    The API view does not surface every create-time field, so a
    `get -o yaml | create -f` round-trip is not always lossless. Keep your
    source manifest as the record of what you applied.

    - **VM:** `cloudInit` (user_data), `cloudInitDisabled`, `firmware` /
      `firmwareID`, and `diskGiB` are consumed at create time and are not in
      the view, so the projection omits them and re-applying reverts those to
      server defaults. Only the first NIC is projected (the v1 VM schema
      attaches a single `network`); a multi-NIC VM loses the extras.
    - **Network:** bridge networks round-trip in full. An overlay network
      projects only `type` + `subnet` (the create API forbids the
      server-derived `bridgeName` / `mtu` / `vlan`), so re-applying allocates a
      fresh VNI rather than preserving the original.
    - **StoragePool:** round-trips except the operator-settable `config` blob.
      A multi-node pool projects as a single `nodeList` document when every
      instance shares a path, or as one document per instance when paths
      differ, so each node keeps its own path on re-apply.

    The `config` blob on both Network and StoragePool is not yet
    manifest-expressible (there is no `config` field in the v1 schema) and is
    dropped on projection.

## Deleting from a manifest

`otherix delete -f` reads the same files and removes the named resources in
reverse create order. It prompts for confirmation when stdin is a terminal;
`--force` skips the prompt and `--dry-run` prints the plan:

```bash
otherix delete -f cluster.yaml          # prompts when interactive
otherix delete -f cluster.yaml --force  # no prompt
otherix delete -f cluster.yaml --dry-run
```

Existing delete blockers (a network still in use, a pool with disks) are
reported and skipped - `delete -f` never force-deletes a resource that has
dependents.
