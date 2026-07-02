# Port forwarding to VMs

`otherix forward` opens a local TCP listener on your machine and tunnels raw
bytes to a `(VM, port)` through the control plane. It lets you reach an
arbitrary TCP service on a VM that has **no public IP** - a database, a web or
admin UI, a metrics endpoint, an SSH port - straight from your laptop, without
any inbound path to the guest.

The VM is addressed by name and every connection is brokered with a fresh
short-lived credential. For the underlying broker and transport model see the
[Ingress concept](../concepts/ingress.md).

## Basic usage

```bash
otherix forward <vm-name> <port>
```

By default this opens a listener on `127.0.0.1` with an ephemeral port (the
CLI prints the chosen local address). Use `-L` / `--listen` to pin a specific
local address and port:

```bash
otherix forward db01 5432 -L 127.0.0.1:5432   # local 127.0.0.1:5432 -> db01:5432
```

Each accepted connection is brokered independently: the CLI mints a fresh
short-lived credential per connection and splices it to the guest's `<port>`.
The transport is chosen automatically by the VM's NIC type - an overlay VM goes
**direct through the gateway** (the control plane stays out of the data path)
while a bridge VM goes **through the control plane relay**. You do not choose;
it just works.

## Worked examples

### Postgres

```bash
otherix forward db01 5432 -L 127.0.0.1:5432
# in another terminal:
psql -h 127.0.0.1 -p 5432 -U app
```

### A web or admin UI

```bash
otherix forward web01 8080 -L 127.0.0.1:8080
# then open http://127.0.0.1:8080 in your browser
```

### Redis

```bash
otherix forward cache01 6379 -L 127.0.0.1:6379
# then:
redis-cli -p 6379
```

### SSH or scp on a raw port

`forward` is transport-agnostic, so you can point it at guest port 22 and get a
plain local port to hand to `ssh` or `scp`:

```bash
otherix forward web01 22 -L 127.0.0.1:2222
# then:
ssh -p 2222 user@127.0.0.1
scp -P 2222 file 127.0.0.1:/tmp/
```

!!! tip "Prefer `otherix ssh` for interactive login"
    For an interactive shell, [`otherix ssh`](ssh-access.md) is usually nicer:
    it mints a short-lived guest SSH certificate and wires up the
    `ProxyCommand` for you, so there is no local port to manage. Reach for
    `forward` when a tool needs a raw local TCP port (for example `scp`,
    `rsync` over a custom port, or an IDE remote plugin).

### Exposing on the LAN

By default the listener binds to loopback and is reachable only from your own
machine. Bind to `0.0.0.0` to expose it on every interface:

```bash
otherix forward web01 8080 -L 0.0.0.0:8080
```

!!! warning "A non-loopback bind is reachable by others"
    `-L 0.0.0.0:8080` makes the tunnel reachable by anyone who can reach your
    machine on that port - the forward carries your credentials, so they reach
    the VM as you. Prefer a loopback bind (`127.0.0.1:...`) unless you have a
    specific reason to share it.

## How `forward` relates to `ssh` and `lb`

All three open a local listener and reach VMs by name through the same broker;
they differ in what they carry and how they pick a target:

| Command | Target | Carries | Use for |
| --- | --- | --- | --- |
| `otherix forward <vm> <port>` | one VM, one port | raw TCP bytes | any TCP service on a specific VM |
| [`otherix ssh <vm>`](ssh-access.md) | one VM, port 22 | SSH with a short-lived cert + `ProxyCommand` | interactive login and file copy |
| [`otherix lb connect <name>`](load-balancers.md) | a pool of VMs | raw TCP, balanced per connection | a service fronted by a load balancer |

## Lifetime and cleanup

`otherix forward` runs in the **foreground** and keeps the local listener open
until you stop it with `Ctrl+C`. There is nothing to clean up afterwards.

Because each accepted connection is brokered separately with its own
credential, the short credential TTL does not limit how long the listener can
stay up: a listener you leave running all day keeps working, minting a new
credential for every new connection.

## Troubleshooting

If a connection through the forward is refused, the tunnel itself is usually
fine - the two common causes are:

- **The guest is not listening on that port.** Confirm the service is bound and
  listening inside the VM (for example with `otherix ssh <vm>` and `ss -ltnp`).
- **The VM is not reachable yet.** A freshly created VM may still be booting or
  waiting for its NIC to come up.

For the broker and transport model behind these behaviours, see the
[Ingress concept](../concepts/ingress.md).
