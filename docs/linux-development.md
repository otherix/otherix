# Linux development

On a Linux host with KVM, `make local-dev-start` brings up the full two-node
Otherix dev cluster - control plane (embedded etcd) plus two agents - the same
topology macOS gets from the two Lima VMs, but using Linux network + mount
namespaces instead of VMs. No nested virtualization.

## Prerequisites

- Linux host with `/dev/kvm` (bare metal or a KVM-enabled VM).
- Packages: `qemu-system-x86_64` or `qemu-system-aarch64`, `iproute2` (`ip`),
  `nftables` (`nft`), `util-linux` >= 2.32 (`unshare --mount --propagation`), the
  `wireguard` kernel module, and OVMF/AAVMF UEFI firmware.
- `sudo` (the network + mount namespace topology requires root).
- Go toolchain (to build the binaries).

`make local-dev-start` runs a dependency preflight and fails with the exact
missing package if anything is absent.

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
