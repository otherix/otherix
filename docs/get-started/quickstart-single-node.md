# Quick start: single-node

Bring up a complete Otherix cluster and a running, SSH-able VM on one
Debian/Ubuntu host with two commands. No repo clone, no Go toolchain.

!!! note "Requirements"
    A Debian/Ubuntu host with hardware virtualization (`/dev/kvm` present),
    run as root. The host must reach GitHub releases and the Ubuntu cloud-image
    mirror.

## 1. Install and launch

```bash
curl -fsSL https://get.otherix.dev/quickstart.sh | sudo sh
```

This installs the control plane, a local hypervisor agent, and the CLI;
creates a default NAT network; and launches a demo VM. When it finishes it
prints how to reach the VM - its IP, the login user, a generated password,
and whether your SSH public key was installed:

```
  SSH in (NAT network 10.88.0.0/24, reachable from this host):
    ssh otherix@10.88.0.10
    (your SSH public key was installed)
    password for otherix: <generated>
```

If you have `~/.ssh/id_ed25519.pub` or `~/.ssh/id_rsa.pub`, the script installs
it into the VM so you can log in by key; the generated password is the fallback.

## 2. Log in

Over the network (the VM is on the host-local NAT bridge):

```bash
ssh otherix@<printed-ip>
```

Or attach to the serial console, which needs no network at all:

```bash
otherix vm console demo
```

From inside the VM, `ping 1.1.1.1` confirms NAT egress to the internet.

## Make your own VM

The script set a cluster default network, so `vm create` needs no `--network`:

```bash
otherix vm create web-1 \
  --image-url https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img \
  --arch amd64
```

See the [CLI reference](../reference/cli.md) for snapshots, resize, migration,
and more.

## What the script set up

- `otherix-api` and `otherix-agent` systemd services (state under
  `/var/lib/otherix/`, config under `/etc/otherix/`).
- A CLI cluster profile named `local` (in `/root/.otherix/config`, and copied to
  the invoking operator's `~/.otherix` so `otherix` works without `sudo`).
- A managed bridge network `default` with NAT egress and DHCP, set as the
  cluster default network.
- A demo VM named `demo` attached to that network.

## Upgrade

The quickstart is for the initial bring-up only - do not re-run it to upgrade.
It provisions the network and demo VM, and it refuses to run against a
different-version control plane already serving on the host. To move to a newer
release, re-install the components. This upgrades the binaries in place and
preserves all state (etcd data, certs, and config under `/var/lib/otherix` and
`/etc/otherix`):

```bash
curl -fsSL https://get.otherix.dev/install.sh | sudo OTHERIX_COMPONENT=api   sh
curl -fsSL https://get.otherix.dev/install.sh | sudo OTHERIX_COMPONENT=agent sh
curl -fsSL https://get.otherix.dev/install.sh | sudo OTHERIX_COMPONENT=cli   sh
```

Each command installs the latest release and the package upgrade restarts the
service. Pin a specific version with `OTHERIX_VERSION=vX.Y.Z`.

## Uninstall

To remove every component and wipe all state - the packages, the CLI,
`/var/lib/otherix`, `/etc/otherix`, the CLI config, the `otherix` user, and
managed bridges / nft rules:

```bash
curl -fsSL https://get.otherix.dev/uninstall.sh | sudo FORCE=true sh
```

`FORCE=true` is required for the piped form; a blind `curl | sudo sh` with no
confirmation refuses. For an interactive `yes` prompt, run it from a downloaded
file instead (`curl -fsSL https://get.otherix.dev/uninstall.sh -o uninstall.sh
&& sudo sh uninstall.sh`).

## Growing to HA

A single node is a one-member cluster with a routable peer URL, so you can add
members later with no reconfiguration. See the
[high-availability guide](../operations/high-availability.md).
