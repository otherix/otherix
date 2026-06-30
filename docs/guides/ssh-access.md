# SSH access to VMs

Otherix can put an SSH session straight onto a guest without exposing the
guest's IP or opening an inbound port on the node. The SSH bytes tunnel through
the control plane over a WebSocket relay to the owning agent, which pipes them
to the guest's `sshd`. There are two audiences:

- an **operator** with a configured `otherix` CLI, who runs `otherix ssh <vm>`;
- an **external** person with no Otherix account, who installs the thin
  `otherix-ssh` connector, imports a one-time bundle, and runs
  `ssh <vm>.<suffix>`.

## How it works

The cluster holds an **SSH user certificate authority**. Every SSH-ingress VM is
provisioned at create time to trust that CA (`TrustedUserCAKeys`). When someone
connects, the control plane mints a short-lived SSH user certificate, and the
SSH bytes are spliced over the relay
(`GET /v1/vms/{vm}/ssh-stream`, a WebSocket) to the agent that owns the VM,
which connects to the guest's `sshd` on the guest network.

The **guest `sshd` is the sole login authority.** Otherix never keeps a
server-side login allow-list: the certificate names a login (principal), and
whether that login is accepted is entirely the guest's policy (its users,
`AllowUsers`, `authorized_principals`, and so on). Otherix gates *reach* (who
may open a relay to which VM), not *login*.

## Prerequisites

- **SSH ingress enabled for the cluster.** An admin turns on the cluster-wide
  SSH-ingress switch; the control plane generates the cluster SSH user-CA on
  first use. A cluster **DNS suffix** is set so externals can address VMs as
  `<vm>.<suffix>` (the connector defaults the suffix to `otherix`).
- **The VM is created with SSH ingress.** Opting a VM in provisions its
  cloud-init so the guest trusts the cluster SSH user-CA. A VM created without
  SSH ingress behaves as a normal VM.
- **Guest image requirements.** The guest needs **OpenSSH 8.2 or newer** (for
  the `sshd_config.d` Include drop-in the provisioning writes) and a cloud-init
  that honors `write_files`. Stock Ubuntu, Debian, and RHEL-family cloud images
  qualify. The provisioning adds only the CA trust drop-in
  (`/etc/ssh/sshd_config.d/60-otherix-ca.conf` pointing `TrustedUserCAKeys` at
  the installed CA public key) and restarts `ssh`/`sshd`; it never creates
  users and never weakens the guest's existing login policy.

## Operator: SSH into a VM

With a configured cluster profile, one command opens a session:

```bash
otherix ssh web01
# log in as a specific guest user:
otherix ssh web01 --login deploy
```

`otherix ssh` resolves your cluster credential, mints a short-lived guest
certificate, and execs your system `ssh` client with a `ProxyCommand` that
tunnels through the relay. There is nothing to expose on the node and no guest
IP to reach directly. (`otherix ssh proxy <vm> <port>` is the `ProxyCommand`
primitive `ssh` invokes under the hood; you do not run it by hand.)

## Operator: grant access to an external

Hand SSH access to someone who has no Otherix account by minting a **grant
bundle**:

```bash
otherix ssh-grant create alice-web \
  --vm web01,web02 \
  --login deploy \
  --ttl 168h \
  --user "Alice Smith"
```

The command prints a single paste-able **bundle** carrying the control-plane
URL, the TLS trust the external needs to reach the same control plane you do,
the one-time grant token, and the granted `vm:login` set. Send the bundle over
a secure channel. The grant token is shown **exactly once**; it is a bearer
secret.

Manage grants over their lifetime:

```bash
otherix ssh-grant list
otherix ssh-grant get alice-web
otherix ssh-grant add-vm alice-web db01=postgres   # widen scope
otherix ssh-grant remove-vm alice-web web02        # narrow scope
otherix ssh-grant revoke alice-web                 # cut off access
```

Per-VM logins can be inlined as `name=login` in `--vm`; `--login` sets the
default for entries without one. `--ttl` bounds the grant lifetime (omit it for
a grant that never expires).

## External: connect to a granted VM

The external installs the thin `otherix-ssh` connector, imports the bundle
once, and uses plain `ssh` from then on. No Otherix account, no management CLI.

### 1. Install the connector

```bash
curl -fsSL https://get.otherix.dev/ssh | sh
```

The installer detects the OS and architecture, downloads the matching
`otherix-ssh` release artifact, **verifies its SHA256 against the release
`SHA256SUMS` before installing** (it refuses to install an unverified or
mismatched binary), and drops `otherix-ssh` onto `PATH`
(`/usr/local/bin` when writable, otherwise `~/.local/bin`; no `sudo` needed).
Pin a version with `OTHERIX_VERSION=vX.Y.Z` or choose the directory with
`OTHERIX_BINDIR`.

On macOS or Linuxbrew, install from the tap instead (see
[Homebrew](#homebrew-macos-and-linuxbrew) below):

```bash
brew install otherix/tap/otherix-ssh
```

### 2. Import the bundle

```bash
otherix-ssh add ./alice-web.bundle
# or read it from stdin / paste it directly:
otherix-ssh add -
```

`add` stores the control-plane coordinates, TLS trust, and grant token to a
`0600` state file under `~/.otherix/ssh`, writes a managed wildcard
`ssh_config` fragment, and wires it into `~/.ssh/config` with an `Include`
line (added once, never clobbering existing content). Re-running `add` is
idempotent.

### 3. SSH in

```bash
ssh web01.otherix
```

The wildcard `Host *.<suffix>` rule routes the connection through
`otherix-ssh proxy`, which rebuilds the connection from the stored state and
splices it to the relay. Use the suffix the operator told you (the connector
defaults to `otherix`; override at import time with `otherix-ssh add --suffix`).

## Homebrew (macOS and Linuxbrew)

`otherix-ssh` is published to the **`otherix/homebrew-tap`** tap, so it installs
the same way on macOS and Linuxbrew:

```bash
brew install otherix/tap/otherix-ssh
```

The formula lives in the separate `otherix/homebrew-tap` repository at
`Formula/otherix-ssh.rb` and is regenerated and pushed by the release pipeline
on each tagged release (it is not hand-edited). For reference, it has the shape:

```ruby
class OtherixSsh < Formula
  desc "Otherix external SSH connector for granted VMs"
  homepage "https://github.com/otherix/otherix"
  license "Apache-2.0"
  # url / sha256 / version are filled in per release per OS and arch.

  def install
    bin.install "otherix-ssh"
  end
end
```

## Running behind a reverse proxy (HA)

In a high-availability deployment the control plane sits behind a load balancer
or reverse proxy. The SSH relay is a **long-lived WebSocket**, so the proxy in
front of it must be configured for that:

- **Allow the WebSocket upgrade** on the relay path. The relay request is a
  `GET /v1/vms/{vm}/ssh-stream` carrying `Connection: upgrade` and
  `Upgrade: websocket`; the proxy must pass those headers through rather than
  buffering the response.
- **Use a generous idle timeout.** An interactive SSH session can sit idle for
  minutes between keystrokes. A default 60-second proxy read/idle timeout will
  tear the session down mid-use. Raise the idle timeout well above the keepalive
  interval (for example, `nginx` `proxy_read_timeout`/`proxy_send_timeout` set to
  an hour or more).
- **Keepalive.** The relay already sends an application-level WebSocket ping
  about every **15 seconds** and drops a half-open peer that misses a pong, so
  the path stays warm and dead connections are reaped promptly. The proxy idle
  timeout only needs to exceed that interval comfortably; the keepalive does the
  liveness work.

### TLS trust model

The grant bundle carries the **cluster CA / fingerprint**, so the connector
pins the same TLS trust the operator does. How that interacts with the proxy
depends on where TLS terminates:

- **TLS passthrough (recommended).** The proxy forwards the encrypted stream to
  the control plane unchanged; the connector validates the control plane's own
  certificate, which chains to the cluster CA in the bundle. Nothing extra to
  configure on the trust side.
- **TLS terminating at the proxy.** The connector then validates the *proxy's*
  leaf certificate, which must be trusted by the material in the bundle. If the
  proxy presents a certificate the bundle does not pin, connections fail; mint
  and re-distribute bundles after any front-end certificate change.

An `nginx` location for the relay path:

```nginx
location /v1/ {
    proxy_pass            https://otherix_cp;
    proxy_http_version    1.1;
    proxy_set_header      Upgrade $http_upgrade;
    proxy_set_header      Connection $connection_upgrade;  # "upgrade" for ws, "" otherwise
    proxy_set_header      Host $host;
    proxy_read_timeout    3600s;
    proxy_send_timeout    3600s;
}
```

## Security notes

- The **grant token is a bearer secret**: it is persisted `0600` by the
  connector and never logged. Treat the bundle like a credential, send it over a
  secure channel, and `otherix ssh-grant revoke` it when access should end.
- The **guest `sshd` is the only login authority.** Removing or locking a guest
  account, or tightening the guest's `sshd` policy, denies access regardless of
  any outstanding grant. A grant controls reach to the relay, not the guest's
  acceptance of a login.
