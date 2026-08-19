# Ingress gateways

How to provision and manage the **ingress gateways** that carry inbound
connections to overlay VMs. This is the operator counterpart to the
[Ingress and load balancing](../concepts/ingress.md) concept, which explains the
broker model from a client's point of view. Read that first if you only need to
*use* ingress (`otherix ssh`, `otherix forward`, `otherix lb`); read this page
when you need to decide *where the gateway capacity lives*.

## What a gateway is

An ingress gateway is a data-plane forwarder that splices a brokered connection
through to a guest VM on a cross-node **overlay** network, with the control plane
out of the data path. A live session survives a live migration of the guest.

**A gateway is a node role, not a separate appliance or a dedicated node type.**
The same `otherix-agent` binary serves ingress. A node carries the gateway role
in one of two shapes:

- **Co-located** - the gateway role runs on a hypervisor node, alongside its VMs.
  The node keeps hosting VMs; its roles read `[hypervisor, gateway]`. This is the
  simplest way to add ingress capacity: no extra host.
- **Standalone** - a host with no KVM and no storage pools runs the agent in
  gateway-only mode. It hosts no VMs (its role reads `[gateway]`) and exists
  purely to forward ingress. Use this when you want ingress capacity separated
  from your hypervisors, or on a host that sits closer to your clients.

You do not run a separate daemon for either shape, and you never edit a node's
roles by hand - you assign the role through the CLI.

## When you need a gateway

Gateways carry ingress for VMs on **overlay** networks only. A VM on a host-local
**bridge** network is reached through the control-plane relay instead and needs
no gateway (see [Two transports](../concepts/ingress.md#two-transports-chosen-automatically)).

The control plane places gateway coverage on **every** gateway-role node for
**every** overlay that currently has at least one VM NIC. So:

- **Zero gateway-role nodes** means overlay VMs have no gateway to reach them -
  `otherix ssh`/`forward`/`lb` to an overlay VM returns `ingress_unavailable`.
  A cluster that hosts overlay VMs needs at least one gateway-role node.
- **Adding a gateway-role node** extends ingress coverage to every ingress-active
  overlay automatically. There is no per-overlay wiring to maintain.

!!! note "A single-node cluster already has a gateway path"
    On a one-box cluster, enable the gateway role on that same node
    (`otherix node gateway enable <node>`) and it serves ingress for its own
    overlay VMs, co-located. You do not need a second host.

## Enable the gateway role on an existing node

The quickest way to add ingress capacity is to enable the role on a node that has
already joined - typically a hypervisor. This is a co-located gateway.

```bash
otherix node gateway enable node-1
```

```text
node-1: gateway enabled
```

The command is idempotent (enabling an already-enabled node is a no-op `200`) and
requires the `node:manage` permission (admin/operator). The node keeps running
its VMs; only its role set changes. Verify:

```bash
otherix node list --role gateway
otherix node get node-1
```

```text
NAME    ROLES               ARCH   STATUS  CORDONED  AGE
node-1  hypervisor,gateway  arm64  ready   no        3d
```

The node's agent must also be configured to serve the ingress plane: its
`agent.yaml` needs a `gateway:` block with a `listen` address and an
`advertised_endpoint` (the same two settings `otherix-agent bootstrap --gateway`
writes for a standalone gateway, described below). The agent reports that
endpoint on every heartbeat, and the control plane treats it as the proof that
the plane is actually up - a node carrying the role bit but no such config is
skipped for ingress, for relaying, and for placement decisions that depend on it.
Restart the agent after editing the config.

With both halves in place the control plane places gateway memberships for
`node-1` on every ingress-active overlay within a heartbeat cycle; overlay VMs
become reachable through it with no further action.

To take the role away again, see [Remove or drain a gateway](#remove-or-drain-a-gateway).

### What the gateway role costs the host

A gateway forwards traffic on behalf of other machines, so enabling the role
widens the host's exposure in two ways worth deciding on deliberately.

**Host-global IPv4 forwarding.** The agent enables `net.ipv4.ip_forward` on a
gateway host and installs no `FORWARD` filter of its own, so the host will route
between any interfaces it holds, not only the overlay ones. Do not put a gateway
host on an untrusted management LAN, and if the host straddles networks you would
not want bridged, add your own `FORWARD` policy before enabling the role. The
setting does not survive a reboot on its own - the agent reapplies it on every
reconciliation pass, so a rebooted gateway forwards again once the agent is up.

**A relay gateway sees the guest traffic it relays.** When nodes cannot reach
each other directly (typically because they sit behind NAT), the control plane
relays their overlay traffic hub-and-spoke through a gateway. WireGuard is
point-to-point, so the gateway decrypts each spoke's traffic and re-encrypts it
toward the other: it can observe and inject the guest traffic of every pair it
relays for. Treat a relay gateway as inside the trust boundary for that traffic
and site it accordingly. Control-plane-to-agent traffic is not exposed - the
control plane runs its own end-to-end TLS through the gateway, which forwards
those bytes without terminating them - and live migration and blob transfer carry
their own TLS as well.

## Provision a standalone gateway host

A standalone gateway is a host that runs only the ingress plane - no KVM, no
qemu, no storage pools. Provision it much like a hypervisor node
([Join a node](join-a-node.md)), with three differences: mint a **gateway-kind**
join token, bootstrap with `--gateway`, and skip the KVM prerequisites.

### 1. Mint a gateway join token

On the control plane, as an admin:

```bash
otherix node join-token create --kind gateway --node-name gw-1 --ttl 10m
```

This prints the one-time token plaintext and the cluster CA fingerprint, exactly
like a node token. A gateway-kind token materializes the node with the gateway
role at redemption.

### 2. Install the agent (no KVM required)

Install the agent package on the gateway host. It ships the same binary and
systemd unit as a hypervisor node:

```bash
# run as root on the gateway host
curl -fsSL get.otherix.dev | OTHERIX_COMPONENT=agent sudo -E sh
```

Unlike a hypervisor node, a standalone gateway needs **no** `/dev/kvm`, no
`qemu-system-*`, and no UEFI firmware - it never boots a guest.

### 3. Bootstrap in gateway mode

Run `bootstrap` with `--gateway`. The distinguishing flag is
`--ingress-advertised-endpoint`: the HTTPS URL clients dial to reach the ingress
splicer (this is what the broker hands back to a connecting client). It is
required with `--gateway`.

```bash
otherix-agent bootstrap \
  --gateway \
  --token otx_join_AbC123... \
  --ca-fingerprint sha256:1a2b3c... \
  --cp-url https://cp.example:8443 \
  --node-name gw-1 \
  --advertised-endpoint https://10.0.0.31:9443 \
  --ingress-advertised-endpoint https://gw-1.example:9444
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--gateway` | `false` | Provision a gateway-only host. Requires `--ingress-advertised-endpoint`. |
| `--ingress-advertised-endpoint` | (required) | HTTPS URL clients dial to reach the ingress splicer. |
| `--ingress-listen` | `0.0.0.0:9444` | Ingress bind address, distinct from the mTLS control listener (`--listen`, default `0.0.0.0:9443`). |
| `--advertised-endpoint` | (required) | HTTPS URL the control plane uses to reach the agent's control listener (heartbeat). |

`--migration-host` is not needed - a gateway host has no VM migration ingress.
Bootstrap writes the cert material and a generated `agent.yaml` carrying a
`gateway:` block; it never overwrites an existing config (delete it first to
regenerate).

A standalone gateway uses its own WireGuard key
(`/var/lib/otherix/wg-gateway/private.key`) so a host that previously ran a
hypervisor agent never reuses that agent's overlay key.

### 4. Start the agent and verify

```bash
systemctl enable --now otherix-agent   # or: otherix-agent serve
```

```bash
otherix node list --role gateway
```

```text
NAME   ROLES    ARCH   STATUS  CORDONED  AGE
gw-1   gateway  arm64  ready   no        30s
```

The node appears `pending`, then `ready` once it reports its first heartbeat. It
hosts no VMs by design - `[gateway]` alone, never `hypervisor`.

## How gateway placement works

You never map gateways to overlays yourself. The control plane reconciles
coverage continuously:

- For every overlay that has at least one VM NIC ("ingress-active"), it places a
  **gateway membership** on **every** gateway-role node - a plain cross-product.
  Adding a gateway-role node or an overlay VM extends coverage automatically.
- There is **no redundancy target and no spread**. Two gateway-role nodes both
  cover every ingress-active overlay; the broker picks a ready one per
  connection. Running more than one is how you get redundancy and spread load -
  the model does not require a fixed count, and zero is a valid (if
  ingress-less) state.
- A membership consumes one address from the overlay's subnet, from the **same**
  pool as VM NICs. Gateways therefore compete with VMs for overlay address space.

!!! warning "Watch overlay subnet pressure"
    Because each gateway membership takes a VM-sized address on every
    ingress-active overlay, a small overlay subnet plus many gateway-role nodes
    can crowd out VM NICs. The control plane logs a `WARN` as memberships plus VM
    NICs approach an overlay's usable-host count. If you see it, widen the
    overlay subnet ([Networks](networks.md)) or reduce the number of gateway-role
    nodes on that overlay. Size overlay subnets with headroom for both VMs and
    the gateways covering them.

## Remove or drain a gateway

Take the gateway role away with `disable`:

```bash
otherix node gateway disable node-1
```

```text
node-1: gateway disabled
```

Removal is **sticky against live sessions**: the control plane keeps a gateway's
membership on an overlay as long as that node reports active sessions on it, and
reaps the membership only once the session count drains to zero. In-flight
connections are not cut when you disable the role; new connections stop being
brokered to that node. A co-located node keeps hosting its VMs after you disable
the gateway role - only ingress forwarding stops.

Deleting a node ([node maintenance](../operations/node-maintenance.md)) reaps its
gateway memberships as part of the delete, so you do not need to disable the role
first when decommissioning the whole node.

## Permissions

The whole gateway-role surface - `otherix node gateway enable|disable` and
minting a gateway-kind join token - is gated by `node:manage` (admin and
operator). See the [RBAC matrix](../rbac.md).

## See also

- [Ingress and load balancing](../concepts/ingress.md) - the broker model and the
  gateway/relay transports.
- [Join a node](join-a-node.md) - the hypervisor-node join flow this mirrors.
- [Networks](networks.md) - overlay vs bridge networks and subnet sizing.
- [Load balancers](load-balancers.md) - spread ingress across a labeled VM pool.
- [Node maintenance](../operations/node-maintenance.md) - cordon, drain, and
  delete nodes.
