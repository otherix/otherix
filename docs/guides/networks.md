# Networks

A network in Otherix is a cluster-wide definition that VM NICs attach to. There
are two kinds: a **bridge** (a Layer-2 bridge on the node) and an **overlay**
(a VXLAN segment spanning nodes). Networks are managed through the
`otherix network` command group. Every authenticated role can read networks
(`network:read`); only **admin** may create or delete them (`network:manage`).

For the underlying model - bridges, the VXLAN overlay, the WireGuard underlay,
and nftables egress - see [Networking concepts](../concepts/networking.md).

## Creating a bridge network

A bridge network needs a host bridge interface name:

```bash
otherix network create net-dev --type bridge --bridge-name br0
```

`--type` defaults to `bridge`, so it can be omitted. The flags for a bridge
network:

| Flag | Meaning |
| --- | --- |
| `--bridge-name` | Host bridge interface name. Required for `--type bridge`. 1..15 chars, `[A-Za-z][A-Za-z0-9_-]*`; the `otb*` / `otvx*` / `otwg*` prefixes are reserved. |
| `--managed` | The Control Plane manages the bridge lifecycle (required for NAT egress). |
| `--egress` | Managed egress mode: `none` (default) or `nat`. |
| `--subnet` | Egress subnet in CIDR form. Required for `--egress nat`. |
| `--gateway` | Gateway IP inside `--subnet`. Derived as the first usable host when omitted. |
| `--mtu` | Link MTU, 68..9216. The server applies 1500 when omitted. |
| `--vlan` | VLAN tag, 1..4094. Omitted leaves the network untagged. |

For NAT egress the `(type, managed, egress)` triple must be consistent:
`--egress nat` requires `--managed` and a `--subnet`. The server re-validates and
returns `400 validation_failed` for an invalid combination.

```bash
otherix network create net-nat \
  --type bridge \
  --bridge-name br-nat \
  --managed \
  --egress nat \
  --subnet 10.10.0.0/24
```

## Creating an overlay network

An overlay network is a VXLAN segment; the Control Plane derives the bridge name
from an allocated VNI. It requires `--subnet` and **forbids** the bridge-only
flags (`--bridge-name`, `--mtu`, `--vlan`, `--egress`, `--managed`, `--gateway`):

```bash
otherix network create my-overlay --type overlay --subnet 10.50.0.0/24
```

A subnet must be IPv4 with a prefix length between /8 and /30.

## Listing and inspecting

```bash
otherix network list                 # cursor-paginated table
otherix network list --type bridge   # filter by type
otherix network get net-dev          # single network + per-node status
otherix network get net-dev -o yaml  # apply-ready manifest projection
```

`network get` shows a `status` section listing each node that has reported a
reconciliation outcome (NODE, STATUS, ERROR), so you can see how each node
materialised the network. The `get` and `delete` routes accept a name or a UUID;
a name (which is globally unique) is resolved to its UUID client-side.

## Attaching a network to a VM

A VM attaches one NIC to a network at create time. Imperatively:

```bash
otherix vm create web \
  --image-url https://example.com/noble.img \
  --arch arm64 \
  --network net-dev
```

Or declaratively in a [manifest](declarative-manifests.md), via `spec.network`:

```yaml
apiVersion: otherix/v1
kind: VM
metadata:
  name: web
spec:
  imageURL: https://example.com/noble.img
  arch: arm64
  network: net-dev
```

The `--network` value is the network name or UUID. See
[Create and manage VMs](create-and-manage-vms.md) for the full VM workflow.

## Deleting a network

```bash
otherix network delete net-dev          # prompts when interactive
otherix network delete net-dev --force  # skip the confirmation prompt
otherix network delete <network-uuid>
```

Deletion is refused with `409 conflict` while any VM NIC still references the
network; the failure output lists the blocking `vm_nics` count. There is **no
force-delete** for networks by design: remove or migrate the dependent VMs
first, then retry.
