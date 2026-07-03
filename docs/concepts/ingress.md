# Ingress and load balancing

How network traffic reaches a VM inside an Otherix cluster, and how it is
processed on the way in. This is the architectural anchor for the access
guides: [SSH access](../guides/ssh-access.md),
[Port forwarding](../guides/port-forwarding.md),
[Load balancers](../guides/load-balancers.md), and
[Grant external access](../guides/external-access.md) all link back here.
For how VMs get their addresses in the first place, see the
[Networking](networking.md) concept.

## The problem: private VMs you still need to reach

Otherix VMs live on private networks - a cross-node overlay or a host-local
bridge. Their addresses are drawn from a subnet the control plane owns (see
[Networking](networking.md)), and that subnet is not routable from the outside.
There is deliberately **no public IP per VM**, no routable VM network, and no
requirement for the cluster to own an autonomous system or advertise routes with
BGP.

That keeps the network small and safe, but you still need to reach a VM: SSH into
`web01`, connect a client to the Postgres port on `db01`, open a web app. The
naive answer - give each VM a public address and a firewall rule - is exactly
what Otherix avoids. Instead, **the control plane brokers access**. A client asks
the control plane for access to a specific `(VM, port)`, and the control plane
sets up a narrow, short-lived path to just that target.

The result: you address VMs **by name**, from anywhere you can reach the control
plane, with **least-privilege credentials that expire in minutes** - and no
inbound path to the guest ever has to exist on the public internet.

## How a connection reaches a VM: the broker model

Every form of ingress in Otherix - SSH, a forwarded port, a load-balanced
connection, an external grant - is built on the same broker handshake:

1. A client asks the control plane for access to a `(VM, port)`.
2. The control plane authenticates and authorizes the request, resolves the VM
   **by name** to its control-plane-allocated lease IP, and mints a
   **short-lived, narrowly-scoped credential**. It tells the client where to
   connect.
3. The client connects to a **single stable endpoint** - never to the VM
   directly - presenting the credential.
4. The bytes are spliced through to the guest on the requested port.

```
                     1. broker request (VM, port)
  client ───────────────────────────────────────▶  control plane
    │                                                (the broker)
    │            2. short-lived credential
    │◀───────────────────────────────────────────────────┘
    │
    │   3. connect to a single stable endpoint, present credential
    ▼
  gateway  or  CP relay  ──────────────────────────▶  guest VM
                              4. splice bytes           (private subnet)
```

The client never learns or dials the guest's private address. The dial target is
carried inside the verified credential, chosen by the control plane - not by
anything the client supplies. That single property is what makes the model safe
to expose.

## Two transports, chosen automatically

The last hop to the guest takes one of two forms. **Otherix picks between them
automatically** based on the VM's NIC type - you never select a transport by
hand.

### Gateway transport (overlay VMs)

For a VM on a cross-node **overlay** network, the last hop goes through an
**ingress gateway**, and the control plane is completely **out of the data
path**.

- A gateway is a **node role**, not a separate appliance. The agent serves
  ingress either **co-located** with a hypervisor (the same node keeps hosting
  VMs) or **standalone** on a KVM-less host. Each gateway-role node has a single
  advertised HTTPS ingress endpoint, and the control plane extends gateway
  coverage to every overlay that has VMs. See
  [Ingress gateways](../guides/ingress-gateways.md) for how to provision them.
- After brokering, the client dials that one gateway endpoint and presents the
  session credential. The gateway **verifies the credential offline** (it learns
  the signing key's public half over its heartbeat, so it needs no round-trip to
  the control plane), then splices raw bytes to the guest over the overlay.
- Because the gateway is on the overlay and the control plane owns the overlay's
  forwarding, a **live session survives a live migration of the guest**: the
  control plane keeps the gateway's overlay membership sticky while in-flight
  sessions drain, so an open connection is not dropped when the VM moves.

### Relay transport (bridge VMs)

For a VM on a host-local **bridge** network (no overlay NIC), there is no gateway
on the segment, so the **control plane relays the bytes itself**. The client
opens a WebSocket to the control plane; the control plane dials the owning
agent over mutual TLS and pumps raw bytes between the two connections. It
transports ciphertext only and never inspects the stream.

In this transport the control plane is briefly in the data path, and a session
does **not** follow the guest across a live migration in the current version.

### Which transport, at a glance

| | Gateway (overlay VMs) | Relay (bridge VMs) |
|---|---|---|
| **Data path** | Client ↔ gateway ↔ guest; CP out of the path | Client ↔ CP ↔ agent ↔ guest |
| **Chosen when** | VM has an overlay NIC | VM has only a bridge NIC |
| **Survives live migration** | Yes (sessions drain, membership sticky) | No (v1) |
| **CP role** | Broker only | Broker + byte relay (mTLS to agent) |

!!! note "You do not choose the transport"
    The transport follows the VM's NIC type. Put a VM on an overlay network if
    you want gateway ingress with migration-surviving sessions; a bridge VM gets
    the control-plane relay. See [Networking](networking.md) for the difference
    between the two network types.

## The credential and security model

The safety of exposing broker-mediated access rests on the shape of the
credential and where the dial target comes from.

- **Overlay (gateway) credential.** A bearer token signed by a per-session
  certificate authority, with a **short TTL (about 5 minutes)** and **single-use
  per connection**. It is **bound to the specific target** - the VM, its NIC MAC,
  its guest IP, and the port - so it cannot be replayed against a different VM or
  a different port. The gateway verifies it **offline** and enforces an
  **anti-SSRF neighbor-MAC check**: a stale or reused credential cannot be
  steered to a target other than the one it names.
- **Bridge (relay) access.** Authorized by the control plane **per request**, and
  the control-plane-to-agent leg is **mutual TLS**. The client influences only the
  port; the guest is reached on its bridge at the lease-derived IP the agent
  holds for it.
- **The dial target is never client input.** In both transports the address that
  actually gets dialed comes only from the verified credential (or the
  control-plane-authorized request), never from anything the client sends. VMs
  are addressed by name and resolved to their control-plane-allocated lease IP.

!!! tip "Least privilege by construction"
    A broker credential grants exactly one VM, one port, for a few minutes, once.
    There is no long-lived key sitting on a VM, and nothing for an attacker to
    scan for - the inbound path exists only while a brokered session is open.

A concrete example, to ground the model:

```bash
otherix ssh web01            # broker an SSH session to web01:22, by name
otherix forward db01 5432    # broker a local port-forward to db01:5432
```

Each command performs the broker handshake for you and connects through the
right transport automatically. The command syntax lives in the guides; here the
point is only that a name plus a port is all you ever supply.

## Load balancing

A single VM is one target. To spread connections across a **pool** of VMs,
Otherix has a logical **load balancer**, addressed by name (`otherix lb`).

- A load balancer fronts a pool selected by **VM labels**, not a hand-maintained
  member list. You give it a `--selector` (for example `app=web`) and a target
  port, and it covers whatever VMs currently carry those labels. VMs get labels
  at create time (`otherix vm create ... --label app=web`).
- **Balancing is a broker-time choice.** Each connection is brokered
  independently to one healthy backend, so a long-lived local listener spreads
  successive connections across the pool. The client re-brokers per connection
  rather than pinning to one backend for its lifetime.
- **Active health checks** keep the pool honest. The owning agent actively probes
  each backend's health port; a backend that is **confirmed unhealthy is
  subtracted** from the pool so new connections skip it.
- **A still-warming backend is served** (fail toward serving) - a backend with no
  verdict yet counts as eligible, so a problem in the health pipeline can never
  darken a load balancer that is otherwise serving.
- **All-unhealthy refuses the connection.** When *every* backend is confirmed
  unhealthy there is no eligible target, and a connect attempt returns an error
  rather than dialing a known-bad VM.

Health and target status are visible on the CLI: `otherix lb list` shows an
aggregate `STATUS` (`healthy`, `degraded`, `unhealthy`, or `no_backends`) and a
`TARGETS` count of `<healthy>/<total>`, and `otherix lb get <name>` shows
per-backend health (including `unknown` for a still-warming backend) and the last
probe time.

```bash
otherix lb create web-lb --port 80 --selector app=web
```

A load balancer runs over the very same broker and transports described above -
it selects the backend, then brokers a connection to it exactly as a direct VM
access would. There is no separate data path to reason about. See the
[Load balancers guide](../guides/load-balancers.md) for the full workflow.

## What this gives you

Pulling the model together, brokered ingress buys you:

- **No public IP per VM** and **no routable VM network.** Guests stay on private
  subnets; nothing needs a public address.
- **No NAT rules or firewall holes to maintain**, and **no AS or BGP** to run.
  There is no per-VM inbound configuration to drift.
- **Reach VMs by name** from anywhere you can reach the control plane and the
  gateway endpoint - the same handshake for SSH, a database port, or a web app.
- **Short-lived, least-privilege credentials** scoped to one VM, one port, minted
  on demand and expiring in minutes.
- **Access that follows the VM across a live migration** on the overlay
  transport, so a move does not drop your session.
- **Load balancing over a labeled pool** with active health checks, without a
  separate load-balancer appliance in the data path.

External, account-less access for third parties is built on the same broker via a
scoped **ingress grant** - one bearer token for a fixed set of `(VM, port)`
targets - covered in [Grant external access](../guides/external-access.md).

## Where to go next

- [SSH access](../guides/ssh-access.md) - `otherix ssh` into a VM by name.
- [Port forwarding](../guides/port-forwarding.md) - `otherix forward` a guest
  port to a local listener.
- [Load balancers](../guides/load-balancers.md) - create and manage an
  `otherix lb`.
- [Grant external access](../guides/external-access.md) - scoped access for users
  without an Otherix account.
- [Ingress gateways](../guides/ingress-gateways.md) - provision the gateway
  capacity that carries overlay ingress.
- [Networking](networking.md) - overlay vs bridge networks, addressing, DHCP/DNS.
- [Architecture](../architecture.md) - where ingress sits in the wider system.
