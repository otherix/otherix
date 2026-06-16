# Networking

How VMs are wired together on a node and across the cluster, from an operator's
point of view. For the step-by-step CLI workflow see the
[Networks guide](../guides/networks.md); for where this sits in the wider system
see the [Architecture](../architecture.md) overview.

A VM NIC attaches to a *network*. There are two network types today:

- **`bridge`** - a plain Linux bridge on a single node. VM tap devices are wired
  into it, and traffic stays local to that node (plus whatever the bridge is
  itself uplinked to).
- **`overlay`** - a cross-node layer-2 network. VMs on different hypervisors share
  one broadcast domain, carried by a VXLAN tunnel that rides an encrypted
  WireGuard underlay mesh.

!!! note "Deferred types"
    VLAN and isolated networks are deferred extensions of the network-type enum,
    not separate mechanisms. `nat` is an egress mode on bridge networks, not a
    network type.

## Bridge networks

For a bridge network the agent programs a Linux bridge on the host and attaches
each VM's tap device to it. This is the simplest case: everything is local
netlink, no tunnelling. Egress to the outside world is handled by the NAT mode
below.

## Overlay networks

An overlay network gives VMs on different nodes a single L2 segment. Each overlay
network has:

- a **VNI** (VXLAN network identifier) that isolates it from other overlays, and
- a **subnet** that VM addresses are drawn from.

VM frames are encapsulated in VXLAN and tunnelled between nodes. Crucially, the
control plane is authoritative for forwarding:

- **The control plane computes the forwarding database (FDB)** - the map of
  "which remote node hosts which MAC" - and distributes it to each agent on the
  heartbeat. **VXLAN address learning is turned off**; agents never learn MACs
  from the wire, they only apply the FDB the control plane hands them.
- **A WireGuard mesh carries the overlay between nodes.** The control plane
  recomputes the full peer set on every heartbeat and the agent simply applies
  it - there is no agent-to-agent peering negotiation.

This keeps the data plane deterministic: the source of truth for who-talks-to-whom
lives in one place, and an agent restart re-converges from the next heartbeat.

### MTUs

The overlay stacks two layers of encapsulation, so inner MTUs are sized down from
the physical underlay to avoid fragmentation:

| Layer | MTU | Why |
|---|---|---|
| Physical underlay | 1500 | standard Ethernet |
| WireGuard (`otwg0`) | 1440 | 1500 minus 60 bytes of WireGuard encapsulation (20 IP + 8 UDP + 32 WG) |
| Overlay inner (VM-facing) | 1390 | 1440 minus 50 bytes of VXLAN encapsulation |

If the cluster's underlay MTU is tuned away from 1500, the WireGuard and overlay
MTUs are derived from it the same way.

## Egress

Bridge networks can request managed egress with the **`nat`** mode, which installs
an nftables masquerade rule for the network's subnet. VMs then reach external
networks through the host's address.

## Live migration: the network follows the VM

When a VM live-migrates to another node it keeps its MAC and IP, and the fabric is
updated so traffic redirects to the new location promptly - without a
flood-and-learn cycle. How that happens differs by network type:

- **Overlay.** VXLAN address learning is off and the control plane owns the FDB, so
  a migration is just an FDB update. The CP recomputes "this MAC now sits behind the
  target node's VTEP" and **fast-pushes** it: it nudges the overlay peers (and the
  target) to re-pull their FDB immediately, rather than waiting for the next
  heartbeat tick. The MAC re-points between VTEPs deterministically - the data plane
  is *told* where the VM went, never left to flood-and-learn it.
- **Bridge.** As soon as the guest resumes on the target, that node's agent
  announces the VM to the segment - a QEMU self-announce (RARP, MAC-only) plus a
  **gratuitous ARP** for each IPv4 NIC - so the physical switches relearn the port
  immediately, instead of waiting out a MAC-aging timeout.

See the [Live migration guide](../guides/live-migration.md) for the operator-facing
view of a move.

## Host device names

So you can recognise Otherix-managed interfaces on a node, the agent uses
deterministic names:

| Device | Name | Notes |
|---|---|---|
| Bridge (overlay) | `otb<vni>` | e.g. `otb1000` for VNI 1000 |
| VXLAN VTEP | `otvx<vni>` | the tunnel endpoint, e.g. `otvx1000` |
| WireGuard interface | `otwg0` | one interface carries all overlay peers |
| VM tap | `ot<12hex>` | `ot` + the first 12 hex digits of the NIC id |

All Otherix-managed taps share the `ot` prefix, which is how the agent
enumerates the ones it owns.
