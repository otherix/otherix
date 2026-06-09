# Linux development

On a Linux host with KVM, `make local-dev-start` brings up the full two-node
Otherix dev cluster - control plane (embedded etcd) plus two agents - the same
topology macOS gets from the two Lima VMs, but using Linux network + mount
namespaces instead of VMs. No nested virtualization.

## Requirements

### Agent runtime (any host that runs `otherix-agent`)

What the agent needs to boot VMs and build the network fabric - dev or prod:

- **QEMU**: `qemu-system-x86_64` (amd64) or `qemu-system-aarch64` (arm64) to run
  VMs, plus `qemu-img` to inspect/resize disk images. These are the only two
  external binaries the agent execs (`internal/agent/qemu/{cmdline,img}.go`).
- **KVM**: `/dev/kvm` for hardware acceleration. Without it the agent falls back
  to TCG software emulation (`internal/agent/qemu/cmdline.go` `DetectAccelerator`)
  - functional but far too slow for real use, so dev treats KVM as required.
  Access needs membership in the `kvm` group (or root).
- **Kernel modules** (loadable): `wireguard`, `vxlan`, `tun`, `bridge`, and
  `nf_tables` + the nft NAT modules (`nft_masq`/`nft_chain_nat`). The agent
  builds the fabric via netlink in-process; it does **not** shell out to
  `ip`/`nft`/`wg`.
- **UEFI firmware (arm64 only)**: `/usr/share/AAVMF/AAVMF_CODE.fd` (package
  `qemu-efi-aarch64`), configurable via `qemu.aarch64_firmware_path`. amd64 needs
  no firmware (SeaBIOS is built into QEMU).
- **Capability**: `CAP_NET_ADMIN` (the netlink network fabric).

The agent does **not** require `wireguard-tools`, `nftables`, `iproute2`, or
`genisoimage`/`cloud-localds`: WireGuard is configured via `wgctrl`, nftables via
netlink, and cloud-init seed ISOs are built in pure Go (`go-diskfs`).

Install (Debian/Ubuntu):

```bash
sudo apt install --no-install-recommends -y qemu-system-x86 qemu-utils                    # amd64
sudo apt install --no-install-recommends -y qemu-system-arm qemu-utils qemu-efi-aarch64   # arm64
```

### Dev-topology extras (for `make local-dev-start` on this host)

Beyond the agent itself, the two-node netns wiring
(`dev/scripts/linux-multinode.sh`) needs, on the host:

- `iproute2` (`ip`) and `nftables` (`nft`) - build the bridge/netns/veth +
  host NAT for VM image pulls.
- `util-linux` >= 2.32 (`unshare --mount --propagation`) - the per-agent mount
  namespace.
- `sudo` (the netns + mount-namespace topology requires root).
- Go toolchain (to build the binaries).
- `wireguard-tools` (`wg`) - only for the `smoke-wireguard-mesh` and
  `smoke-networking` smokes, which inspect the kernel WireGuard handshake with
  `wg`. The agent itself does not need it (it uses the in-kernel API via
  `wgctrl`); install it for those smokes:
  `sudo apt install --no-install-recommends -y wireguard-tools`.

`make local-dev-start` runs a dependency preflight and fails with the exact
missing dependency if anything is absent.

## Topology

```
Host netns
  CP api-server   :8080 (CLI)   :8443 (agents)
  bridge otdev0   10.77.0.254/24 -- veth -> [otns1] agent node-1  10.77.0.1
                                 -- veth -> [otns2] agent node-2  10.77.0.2
  masquerade 10.77.0.0/24 -> default route   (VM image pull / egress)
```

Each agent runs inside its own network namespace (so `otwg0`, the `otherix-nat`
table, and VXLAN/bridge interfaces are isolated) and its own mount namespace (so
the cluster-default pool `/opt/otherix/pools/default` resolves to per-node
storage under `/opt/otherix/dev/nodeN/pools`). Per-node state lives under
`/opt/otherix/dev/node{1,2}/` (root-owned).

The CP reaches each agent over mTLS at its advertised netns IP
(`https://10.77.0.N:9443`), which is included in the agent server cert SAN: the CP
derives that SAN entry from the node's `advertised_endpoint` when it signs the
cert at join. No `/etc/hosts` mapping is needed.

## Bring up / tear down

```bash
make local-dev-start     # build + topology up + CP + bootstrap both agents
./bin/otherix node list  # node-1 and node-2, both ready
make local-dev-stop      # stop CP, tear down netns/bridge/NAT, wipe state + etcd
```

The flow uses `sudo` for the privileged steps (`dev/scripts/linux-multinode.sh`);
you may be prompted for your password.

## Smokes (after local-dev-start)

```bash
make smoke-wireguard-mesh   # cross-node WireGuard handshake
make smoke-overlay          # VXLAN overlay datapath across both nodes
make smoke-overlay-vm       # two real VMs, cross-node ping over the overlay
```

Live migration is not yet implemented and is therefore not part of the Linux dev
smoke.

## Smoke: overlay egress (manual, static-addressed VM)

Verifies overlay egress (per-node SNAT) end to end against real agents. The CP
gives overlay VMs internet access by putting a link-local anycast gateway
(`169.254.1.1`) on every node's `otb<vni>` bridge, enabling `ip_forward`, and
masquerading overlay traffic out the node's uplink; a per-node DNS forwarder on
`169.254.1.1:53` relays to the node's upstream resolver. The gateway address is
node-independent, so it survives live migration. Run after `make
local-dev-start`.

1. Create a managed overlay network with egress enabled (through the CLI):

   ```bash
   ./bin/otherix network create ov-egress \
     --type overlay --subnet 10.60.0.0/24 --egress nat
   ```

   (`--managed`, `--mtu`, and the bridge name are server-derived for overlay and
   are rejected if supplied; egress on overlay is always managed.)

2. Create a VM on the overlay with a **static** cloud-init network-config. The
   gateway and DNS are the link-local anycast address `169.254.1.1`, which is
   off-subnet, so the default route must be declared `on-link`. Example
   network-config (netplan v2 form) baked into the VM's cloud-init:

   ```yaml
   network:
     version: 2
     ethernets:
       eth0:
         addresses: [10.60.0.5/24]
         nameservers:
           addresses: [169.254.1.1]
         routes:
           - to: 169.254.1.1/32
             scope: link
           - to: default
             via: 169.254.1.1
             on-link: true
   ```

3. From the VM console (`./bin/otherix vm console <id>`), verify egress:

   ```bash
   ip route                          # default via 169.254.1.1
   ping -c2 8.8.8.8                  # ICMP egress (SNAT to the node IP)
   getent hosts archive.ubuntu.com   # DNS via the node forwarder
   curl -sI http://archive.ubuntu.com/ | head -1   # full egress path
   ```

4. Confirm the source IP is the node IP: on the node,
   `sudo ip netns exec otns1 nft list table ip otherix-nat` shows the
   `iifname "otb<vni>" oifname ... masquerade` rule; the external peer sees the
   node's address.

Pass criteria: ping + DNS + HTTP all succeed; the masquerade rule is present; no
`/etc/hosts` mapping and no external DHCP were involved.

### Multi-overlay anycast containment (extended smoke)

The gateway address `169.254.1.1` is anycast on the `otb<vni>` bridge of EVERY
egress overlay, each with a distinct per-VNI MAC. Bringing up two egress
overlays on one node verifies they do not interfere (the agent sets
`arp_ignore=1` / `arp_announce=2` per bridge to contain the shared address).

1. Create a second egress overlay:

   ```bash
   ./bin/otherix network create ov-egress-2 \
     --type overlay --subnet 10.61.0.0/24 --egress nat
   ```

2. Attach a VM to each overlay (`10.60.0.x` on `ov-egress`, `10.61.0.x` on
   `ov-egress-2`), both with the same static cloud-init gateway/DNS
   `169.254.1.1` (on-link), as in the single-overlay scenario.

3. From each VM independently verify egress and DNS:

   ```bash
   ping -c2 8.8.8.8
   getent hosts archive.ubuntu.com
   ```

4. On each node, confirm neither VM learned the other bridge's gateway MAC:

   ```bash
   # in the VM:
   ip neigh show 169.254.1.1     # MAC must match THIS overlay's otb<vni>, not the other
   ```

   And on the node, the per-bridge ARP sysctls are set:

   ```bash
   sudo ip netns exec otns1 cat /proc/sys/net/ipv4/conf/otb<vni>/arp_ignore   # 1
   sudo ip netns exec otns1 cat /proc/sys/net/ipv4/conf/otb<vni>/arp_announce # 2
   ```

Pass criteria: both VMs reach the internet and resolve DNS independently;
neither VM's `169.254.1.1` neighbor entry shows the other overlay's MAC.

## Overlay egress: known limitations (Slice 1)

- **DNS forwarder upstreams are read once at agent start.** A change to the
  node's `/etc/resolv.conf` (DHCP renew, systemd-resolved update) is not picked
  up until the agent restarts. The forwarder is UDP-only (no TCP fallback) and
  drops queries beyond an in-flight cap (a flood degrades to drops, not agent
  exhaustion).
- **`ip_forward` is enabled host-globally and not reverted.** On a bare-metal
  agent (host root netns) the first egress overlay sets `net.ipv4.ip_forward=1`
  for the whole host, and teardown does not reset it. There is no `forward`-chain
  default-deny yet, so overlay VMs can route to other host-reachable networks;
  restricting that is a planned follow-up.
- **No anti-spoof on the overlay L2 segment.** A compromised VM can ARP-spoof the
  gateway (`169.254.1.1`) and MITM co-overlay VMs' egress and DNS. This is
  inherent to the flat-L2 overlay until per-port filtering / IPAM lands.
- **Static addressing only.** VMs are addressed via cloud-init; CP-managed IPAM
  and a per-node DHCP responder (delivering the gateway via DHCP option 121)
  arrive in Slice 2.

## Manual topology control

```bash
make local-dev-up-linux     # just the netns topology (sudo)
make local-dev-down-linux   # tear it down + wipe state (sudo)
sudo dev/scripts/linux-multinode.sh restart   # rebuild loop: restart both agents
```

## Troubleshooting

- Agent logs: `sudo tail -f /opt/otherix/dev/node1/agent.log` (or `node2`).
- Inspect a node's namespace: `sudo ip netns exec otns1 ip addr`.
- Stale topology after a crash: `make local-dev-stop` tolerates partially-present
  objects; re-run then `make local-dev-start`.
- `unshare: unrecognized option '--propagation'` means util-linux is older than
  2.32; upgrade it (modern Ubuntu/Debian/Fedora ship a newer release).
- Image pull fails with `lookup ... on 127.0.0.53:53: connection refused`: DNS
  inside the netns. `up` writes `/etc/netns/otnsN/resolv.conf` from the host's
  real upstreams (or `1.1.1.1`), since the default `127.0.0.53` systemd-resolved
  stub is unreachable from a namespace. If your network blocks the seeded
  resolver, edit `/etc/netns/otns{1,2}/resolv.conf` to a reachable nameserver and
  `sudo dev/scripts/linux-multinode.sh restart`.
