# Grant external access

Sometimes someone **without** an Otherix account needs to reach a specific VM
port: a contractor who needs SSH to one box, a partner who needs a database
port for an integration. You do not want to create a user, hand out a role, or
expose the VM on a public IP for that. An **ingress-grant** is the answer: a
scoped, optionally time-boxed, optionally source-IP-pinned credential to a named
set of `(VM, port)` targets, and nothing else.

The grant is delivered as a single shareable **bundle**. The recipient installs
a thin connector (`otherix-ssh`) and connects to the VM by name - no Otherix
account, no VPN, no inbound path to the guest. Access always flows through the
control plane broker, exactly like the first-party [ingress](../concepts/ingress.md)
path.

Creating and managing grants requires the `vm:ingress-grant` permission.

## Create a grant

The operator creates a grant with `otherix ingress-grant create`, naming the
VMs and ports the third party is allowed to reach:

```bash
otherix ingress-grant create acme-contractor \
  --vm web01:22 \
  --vm db01:5432,8080 \
  --login deploy \
  --ttl 168h \
  --source-ip 203.0.113.10/32
```

The flags:

| Flag | Meaning |
| --- | --- |
| `--vm host:port[,port...]` | A VM and the ports it exposes to the grant. Repeatable - one VM per flag; list multiple ports comma-separated. Port `22` is SSH. |
| `--login` | Guest login the grant connects as, applied to **every** VM in the grant. Defaults to `root`. |
| `--ttl` | Relative lifetime, e.g. `168h` (7 days) or `720h` (30 days). Omit for a grant that never expires. |
| `--source-ip` | Pin the caller's source IP or CIDR (`203.0.113.10/32`, `198.51.100.0/24`). A connection from any other address is refused (fail-closed). |
| `--user` | A free-form label recording who the grant is for. Not an Otherix user. |

The grant above lets the holder SSH to `web01` and reach ports `5432` and `8080`
on `db01`, as the `deploy` login, for seven days, and only from
`203.0.113.10`. A grant reaches the **exact** `(VM, port)` set you list and
nothing else.

`create` prints a **bundle once**, at creation time. Copy it now - it is shown
only this once and cannot be retrieved later. If you lose it, `revoke` the grant
and create a new one.

## The bundle

The bundle is a one-time credential that packs everything the recipient needs
into a single string:

- the control plane server URL,
- the TLS trust needed to reach it,
- the grant token, and
- the `(vm, ports, login)` set the grant covers.

Its single-line form is `otx_ingressbundle_<...>`. You hand this one string to
the third party.

!!! warning "The bundle is a live credential"
    Anyone holding the bundle can reach the VMs and ports it names, as the
    pinned login, until it expires or you revoke it. Treat it like a password:
    share it over a secure channel (an encrypted message, a secrets manager),
    never a public paste or a plaintext email.

## Recipient side: the `otherix-ssh` connector

The recipient needs no Otherix account and no `otherix` CLI - only the thin
`otherix-ssh` connector. Install it:

```bash
curl -fsSL https://get.otherix.dev/ssh | sh
```

Then register the bundle once. `otherix-ssh add` accepts the bundle as a file
path or pasted inline:

```bash
otherix-ssh add ./acme-bundle.txt          # from a file
otherix-ssh add otx_ingressbundle_...       # or paste the single-line form
```

`add` stores the control plane URL, the TLS trust, and the grant token, and
writes an `ssh_config` fragment that routes `*.otherix.local` through
`otherix-ssh proxy`. After that, the recipient reaches a granted VM by name with
plain `ssh`:

```bash
ssh web01.otherix.local
```

The connection is brokered through the control plane to `web01`'s port 22. The
login is fixed by the grant server-side (the `--login` you set at create time),
so the recipient does not choose it.

!!! tip "Overriding the host suffix"
    The connector uses the host suffix `otherix.local` by default (so
    `web01` becomes `web01.otherix.local`). Pass `--suffix` to `otherix-ssh add`
    to use a different suffix if `otherix.local` collides with something on the
    recipient's machine.

## Recipient side: the raw token

If the recipient already has the `otherix` CLI, they can skip the connector and
use the bare grant token directly. Export it as `OTHERIX_API_TOKEN` and use the
normal ingress commands - the token's scope limits them to the granted targets:

```bash
export OTHERIX_API_TOKEN=<grant-token>

otherix ssh web01                       # SSH to a granted VM
otherix forward db01 5432 -L 127.0.0.1:5432   # forward a granted port
```

See [SSH access](ssh-access.md) and [Port forwarding](port-forwarding.md) for
the full command surface those two share.

## Manage grants

Once a grant exists, adjust its scope, inspect it, or shut it off:

```bash
otherix ingress-grant add-vm acme-contractor cache01:6379       # widen: add a target
otherix ingress-grant remove-vm acme-contractor db01            # narrow: drop a VM
otherix ingress-grant list                                      # all grants
otherix ingress-grant get acme-contractor                       # one grant's scope + status
otherix ingress-grant revoke acme-contractor                    # kill switch - disable now
otherix ingress-grant delete acme-contractor                    # remove the record
```

Each command takes the grant's name or its id, and supports `-o text|json|yaml`.

`revoke` is the kill switch: it disables the grant immediately, so every bundle
and raw token issued from it stops working at once. Use it the moment access is
no longer needed or a bundle may have leaked. `delete` removes the grant record
entirely.

## Scoping and security

An ingress-grant is a deliberately narrow tool. Keep it narrow:

- **Grant the minimum ports.** List only the `(VM, port)` targets the third
  party actually needs. A grant can reach nothing outside that set.
- **Prefer a short `--ttl`.** A grant that expires on its own is one less thing
  to remember to revoke. Reach for `168h`/`720h` over an open-ended grant.
- **Pin `--source-ip` when you can.** If the third party connects from a known
  address or range, pin it. An off-network connection is then refused
  fail-closed, even with a valid bundle.
- **Revoke when done.** When the engagement ends, `revoke` the grant. Do not
  leave a live credential outstanding.
