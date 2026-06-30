# Example manifests

Static, ready-to-apply `otherix/v1` manifests for common network + VM
scenarios. Each file is a multi-document manifest (a `Network` and the `VM`
that uses it, separated by `---`), so one `create -f` brings up the whole
scenario and one `delete -f` tears it down.

Pick the directory that matches your node's CPU architecture:

```
deploy/examples/
  amd64/   # x86-64 nodes
  arm64/   # ARM64 nodes
```

The two trees are identical apart from `spec.arch` and `spec.imageURL` (the
Ubuntu 24.04 "noble" minimal cloud image for that architecture). The dev node
arch equals the host arch, so use the directory matching your host.

## Scenarios

| File | Network | VM gets its IP via |
| --- | --- | --- |
| `bridge-nat-static.yaml` | managed **bridge**, NAT egress (`10.81.0.0/24`) | **static** address from `spec.networkConfig` (netplan v2) |
| `overlay-nat-dhcp.yaml` | **overlay** (VXLAN), NAT egress + DHCP (`10.82.0.0/24`) | **DHCP** (no `networkConfig` needed) |
| `bridge-dhcp-ssh.yaml` | managed **bridge**, NAT egress + DHCP (`10.83.0.0/24`) | **DHCP**, and reachable over **SSH ingress** (`otherix ssh`) |

Resource names are unique within each architecture directory and the two
scenarios use different subnets, so you can apply, keep, and delete either one
independently without disturbing the other.

Every VM carries a `userData` cloud-config that creates a passwordful sudo
user **`otherix` / `otherix`** for serial-console login. This is a demo
credential - do not reuse it anywhere real.

## Bridge networks and DHCP

A managed bridge can serve DHCP. Set `dhcp: true` on the `Network` and the
per-node DHCP responder hands each guest its IP, mask, DNS and default route -
the same responder that serves dhcp-enabled overlays - so a bridge VM needs no
`networkConfig` (see `bridge-dhcp-ssh.yaml`). `bridge-nat-static.yaml` instead
configures a static IP on purpose, to show the netplan path; either works on a
managed bridge.

DHCP is also the prerequisite for **SSH ingress**: because otherix reserves
the guest address via CP-IPAM on a managed-DHCP network, it knows where to
dial `otherix ssh <vm>`. A VM opts in with `spec.sshIngressEnabled: true`, and
the cluster switch must be on first (admin runs it once):

```bash
otherix cluster set-ssh-ingress --enabled --suffix ssh.otherix.local
```

With both in place, `otherix ssh ex-bridge-dhcp-vm --login ubuntu` opens a
shell - no console password, no static IP wiring.

## Usage

```bash
# Bring up a scenario (waits for the VM to reach running):
otherix create -f deploy/examples/amd64/bridge-nat-static.yaml --wait

# Log in on the serial console (user otherix / password otherix):
otherix vm console ex-bridge-vm

# Inspect:
otherix vm get ex-bridge-vm
otherix network get ex-bridge

# Tear it down (VM first, then the network - the same file, reverse order):
otherix delete -f deploy/examples/amd64/bridge-nat-static.yaml
```

Swap `amd64` for `arm64` and the resource names (`ex-overlay`, `ex-overlay-vm`)
for the overlay scenario.
