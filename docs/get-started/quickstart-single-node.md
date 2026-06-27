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
- A CLI cluster profile named `local` (in `/root/.otherix/config`).
- A managed bridge network `default` with NAT egress and DHCP, set as the
  cluster default network.
- A demo VM named `demo` attached to that network.

## Growing to HA

A single node is a one-member cluster with a routable peer URL, so you can add
members later with no reconfiguration. See the
[high-availability guide](../operations/high-availability.md).
