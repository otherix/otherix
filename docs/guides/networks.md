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
| `--bridge-name` | Host bridge interface name. Required for `--type bridge`. 1..15 chars, `[A-Za-z][A-Za-z0-9_-]*`; the `otvb*` / `otvx*` / `otwg*` prefixes are reserved. |
| `--managed` | The Control Plane manages the bridge lifecycle (required for NAT egress). |
| `--egress` | Managed egress mode: `none` (default) or `nat`. |
| `--subnet` | Subnet in CIDR form (IPv4, /8../30). Required for `--egress nat` and for `--dhcp`. |
| `--gateway` | Gateway IP inside `--subnet`. Derived as the first usable host when omitted. |
| `--dhcp` | Run the DHCPv4 responder for this network (requires `--managed` and `--subnet`). VMs get their IP/mask/resolver automatically. |
| `--dns` | Advertise the anycast resolver `169.254.1.1` (requires `--managed`). Defaults to the `--dhcp` value, so `--dhcp` alone enables both; pass `--dns=false` to run DHCP without advertising the resolver. |
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

### Managed bridge with DHCP and DNS

Add `--dhcp` to have the network address its VMs automatically - no static
cloud-init needed. The control plane allocates each VM an IP from `--subnet`, and
the per-node DHCP responder hands it out together with the anycast resolver
`169.254.1.1`. With `--egress nat` the VMs also get a default route and reach
external networks; this is the usual "VMs that just work" setup:

```bash
otherix network create net-app \
  --type bridge \
  --bridge-name br-app \
  --managed \
  --egress nat \
  --subnet 10.20.0.0/24 \
  --dhcp
```

`--dns` defaults to the `--dhcp` value, so the command above also advertises the
resolver. To hand out addresses but **not** the resolver, pass `--dns=false`.

To build an **isolated** network - VMs get an IP, mask and a working resolver but
**no route off the segment** - keep the default `--egress none`:

```bash
otherix network create net-isolated \
  --type bridge \
  --bridge-name br-iso \
  --managed \
  --subnet 10.30.0.0/24 \
  --dhcp
```

Addressing (`--dhcp` / `--dns`) is independent of egress: any combination is
valid. See [Networking concepts](../concepts/networking.md#automatic-addressing-dhcp-and-dns)
for how the anycast `169.254.1.1` gateway-and-resolver works and why it survives
live migration.

## Creating an overlay network

An overlay network is a VXLAN segment; the Control Plane derives the bridge name
from an allocated VNI. It requires `--subnet` and **forbids** the bridge-only
flags (`--bridge-name`, `--mtu`, `--vlan`, `--managed`, `--gateway`). `--egress
nat` is allowed for an overlay (per-node anycast-gateway SNAT):

```bash
otherix network create my-overlay --type overlay --subnet 10.50.0.0/24
```

A subnet must be IPv4 with a prefix length between /8 and /30.

`--dhcp` and `--dns` work the same way on an overlay as on a managed bridge (an
overlay is always managed, so no `--managed` is needed). A cross-node overlay
that addresses its VMs and gives them external egress:

```bash
otherix network create my-overlay \
  --type overlay \
  --subnet 10.50.0.0/24 \
  --egress nat \
  --dhcp
```

Every node hosting the overlay answers DHCP and DNS at the same anycast
`169.254.1.1`, so a VM that migrates between nodes keeps its addressing and
resolver unchanged.

## Declaring a network in a manifest

The same networks can be described declaratively and applied with
`otherix create -f` (see [Declarative manifests](declarative-manifests.md)). A
managed bridge with DHCP, DNS and NAT egress:

```yaml
apiVersion: otherix/v1
kind: Network
metadata:
  name: net-app
spec:
  type: bridge
  managed: true
  bridgeName: br-app
  egress: nat
  subnet: 10.20.0.0/24
  dhcp: true
```

An overlay with the same addressing (no `bridgeName` / `managed` - they are
server-derived for an overlay):

```yaml
apiVersion: otherix/v1
kind: Network
metadata:
  name: my-overlay
spec:
  type: overlay
  subnet: 10.50.0.0/24
  egress: nat
  dhcp: true
```

Both `dhcp` and `dns` are manifest fields. `dns` is optional and defaults to the
`dhcp` value (the same rule for bridge and overlay), so `dhcp: true` alone
advertises the resolver. Set `dns: false` to hand out addresses without it, or
`dns: true` with `dhcp: false` for a resolver-only network:

```yaml
apiVersion: otherix/v1
kind: Network
metadata:
  name: net-app-noresolver
spec:
  type: bridge
  managed: true
  bridgeName: br-app
  egress: nat
  subnet: 10.20.0.0/24
  dhcp: true
  dns: false
```

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
