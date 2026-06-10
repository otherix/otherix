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

Resource names are unique within each architecture directory and the two
scenarios use different subnets, so you can apply, keep, and delete either one
independently without disturbing the other.

Every VM carries a `userData` cloud-config that creates a passwordful sudo
user **`otherix` / `otherix`** for serial-console login. This is a demo
credential - do not reuse it anywhere real.

## Why is there no "bridge + DHCP" example?

DHCP is an **overlay-only** feature in otherix. `dhcp: true` is rejected on a
bridge network (`dhcp=true requires type=overlay`). The per-node DHCP
responder is built for the overlay egress model: it hands out a link-local
**anycast** gateway (`169.254.1.1`, via DHCP option 121) because overlay
egress is per-node SNAT, and CP-IPAM allocates cluster-unique addresses
because an overlay is a single L2 segment spanned across nodes by VXLAN.

A managed bridge is different: its gateway is an ordinary in-subnet address
(`.1`), it has no cross-node L2 fabric (each node materialises its own
bridge), and it is typically attached to existing L2 infrastructure where a
DHCP server may already live. So a VM on a bridge configures its address
statically (via `networkConfig`), or relies on an external DHCP server on
that segment - otherix does not serve DHCP there.

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
