# Installation

This page covers what you need to run Otherix, how to obtain the three
artifacts, and how to configure and start the control plane. To go from
a fresh host to a running VM in one sitting, follow the
[Quickstart](quickstart.md) instead - it walks the same steps in order
with copy-paste commands.

## Requirements

Otherix has two cluster components plus an operator CLI:

| Artifact | Role | Platform |
|---|---|---|
| `otherix-api` | Control plane. Embeds an etcd member, the in-process scheduler, and the worker dispatcher. | Plain Go - runs on any Linux host (and natively on macOS for development). |
| `otherix-agent` | Node agent. Talks to QEMU directly and reports state over mTLS. | Linux only. Needs KVM + `qemu-system-<arch>`. |
| `otherix` | Operator CLI. | Any platform. |

Architectures: `amd64` and `arm64`.

The agent needs a Linux host with hardware virtualization:

- `/dev/kvm` present and accessible (verify with `ls -l /dev/kvm`).
- `qemu-system-x86_64` or `qemu-system-aarch64` installed.
- For `arm64` guests, a UEFI firmware blob (Debian/Ubuntu:
  `qemu-efi-aarch64`, default path `/usr/share/AAVMF/AAVMF_CODE.fd`).

The control plane has no external dependencies. There is no Postgres,
Redis, or message queue to operate - the api-server embeds its own etcd
member as the only datastore.

!!! note "Developing on macOS"
    The agent is Linux only. On a Mac, run the control plane natively
    and the agent inside a Lima VM. See
    [macOS development](../macos-development.md).

## Getting the artifacts

### Install a released build

The fastest path on a Linux host is the install script. It downloads the
requested artifact from the latest GitHub release, verifies it against the
published `SHA256SUMS` (fail-closed), and installs it.

```bash
# Operator CLI (installs to /usr/local/bin/otherix)
curl -fsSL get.otherix.dev | OTHERIX_COMPONENT=cli sh

# Control plane (.deb; run as root)
curl -fsSL get.otherix.dev | OTHERIX_COMPONENT=api sudo -E sh

# Node agent (.deb; run as root on a KVM host)
curl -fsSL get.otherix.dev | OTHERIX_COMPONENT=agent sudo -E sh
```

The endpoint serves the script as plain text - read it before piping to a
shell. It honours three environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `OTHERIX_COMPONENT` | `api` | `api`, `agent`, or `cli`. |
| `OTHERIX_VERSION` | `latest` | A release tag such as `v1.2.3`. |
| `OTHERIX_REPO` | `otherix/otherix` | Source `owner/repo` for releases. |

On macOS the script installs the CLI via Homebrew
(`brew install otherix/tap/otherix`); the daemons are Linux only.

### Build from source

```bash
make build              # otherix-api, otherix-agent, otherix into ./bin/
make build-api          # control plane only
make build-cli          # CLI only
```

Cross-compile the daemons for a Linux node from another host:

```bash
make build-linux-amd64  # -> bin/linux-amd64/otherix-{api,agent}
make build-linux-arm64  # -> bin/linux-arm64/otherix-{api,agent}
```

### Container images

Control-plane and agent images are published to GitHub Container
Registry (`ghcr.io`). The control-plane image is distroless; the agent
image is Alpine-based and intended for development and CI only (a
production agent runs as a host binary alongside `qemu-system-*`, not in
a container - it needs `/dev/kvm` and host networking).

## Upgrade

Upgrading installs the new release over the old one. State is preserved (etcd
data, certs, and config under `/var/lib/otherix` and `/etc/otherix`); nothing is
re-provisioned. The daemon `.deb` upgrades restart their services.

`upgrade.sh` upgrades whatever is **already installed** on the host - api, agent,
and/or the CLI - touching only what is present, so the same command is correct on
a control-plane host, a hypervisor node, an operator workstation, or a single-node
all-in-one:

```bash
curl -fsSL https://get.otherix.dev/upgrade.sh | sudo sh
```

Pin a version with `OTHERIX_VERSION=vX.Y.Z` (default: latest). It refuses if no
Otherix component is installed - bootstrap a host with the install script (or the
[single-node quickstart](quickstart-single-node.md)) first.

To upgrade a single component instead, re-run the install script for it - the same
`OTHERIX_COMPONENT` form as a fresh install upgrades in place:

```bash
curl -fsSL get.otherix.dev | OTHERIX_COMPONENT=api sudo -E sh
```

### Ordering (multi-replica and mixed fleets)

Upgrade the **control plane before the agents**, and never roll the control
plane back below the version your agents are running. Agents report their
capabilities to the control plane on every heartbeat, and newer agents may
report fields an older control plane does not recognise. A control plane that
predates a field its agents send rejects those heartbeats, and the affected
nodes are marked unreachable even though their VMs keep running. Rolling the
control plane forward (or restoring it to at least the agents' version) clears
the condition on the next heartbeat. Upgrading all control-plane replicas first,
then the agents, avoids it entirely.

## Filesystem layout

Otherix follows a fixed convention:

| Path | Contents |
|---|---|
| `/etc/otherix/` | Operator-provided config (`api.yaml`, `agent.yaml`). |
| `/var/lib/otherix/` | Runtime state: etcd data dir, cluster CA, generated certs, pools, VMs. |

The defaults below assume this layout. Override paths in config if your
deployment differs.

## Configuring the control plane

`otherix-api` reads a single YAML file (`--config`, default
`/etc/otherix/api.yaml`). Config also binds environment variables with
the `OTHERIX_` prefix and `__` as the nesting separator (for example
`OTHERIX_SERVER__LISTEN`). A full annotated reference ships at
`deploy/config/api.example.yaml`.

For a working single-node install you care about a handful of blocks:

- `server.listen` - the user-facing HTTP API address. Default
  `0.0.0.0:8080`.
- `etcd.data_dir` - where the embedded etcd member persists its data.
  Default `/var/lib/otherix/etcd`. This is your cluster state; back it up.
- `auth.jwt_secret` - HS256 signing key for access tokens. **At least 32
  bytes.** Generate with `openssl rand -hex 32` and replace the example
  value before any non-dev deploy.
- `cluster_ca` - on-disk location of the cluster CA (cert + key). The
  api-server generates it on first boot and reuses it on restart. It
  signs the per-node agent certs and the per-replica CP server cert.
- `cp_cert` - per-replica CP server cert lifecycle. The default
  (Mode C) auto-generates a fresh cert signed by the cluster CA on every
  boot. Add hostnames the agent will dial via `cp_cert.additional_sans`.
- `agent_server` - the second HTTPS listener dedicated to mTLS agent
  traffic (heartbeat, node join, console bridging). **Enable it** for a
  working cluster. There is no default listen address; when enabled, the
  listen address must be set or startup fails (the minimal yaml below
  sets `0.0.0.0:8443`).
- `agent_client` - the outbound CP-to-agent client that drives async
  work (VM create/delete, pool scans). **Enable it** for a working
  cluster.
- `workers.enabled` - the in-process job dispatcher and periodic
  scheduler. Default `true`. When `true`, `agent_client.enabled` MUST
  also be `true` or the api-server refuses to start (it would otherwise
  wedge every async task in `pending`).

!!! warning "workers and the agent client are coupled"
    `workers.enabled: true` requires `agent_client.enabled: true`. To
    run the api-server as an HTTP-only target with no provisioned mTLS
    (smoke testing the contract), set `workers.enabled: false` instead.

### Bootstrap admin

The first admin user is seeded from environment variables read on first
boot, before the HTTP server starts. Identity is a username (the user
logs in by it); email and display name are optional.

```bash
export OTHERIX_BOOTSTRAP_ADMIN_USERNAME=admin
export OTHERIX_BOOTSTRAP_ADMIN_PASSWORD='correct-horse-battery-staple'
```

With zero existing admin rows and both vars set, the api-server creates
the admin. Both set with an admin already present is a no-op. Setting
only one is fatal. If you leave both unset, generate a password hash and
seed the row yourself:

```bash
otherix-api --hash-password 'your-plaintext'   # prints an argon2id PHC string
```

### Minimal `api.yaml`

A single-node config that boots a working cluster (user API + agent
listener + workers):

```yaml
server:
  listen: "0.0.0.0:8080"

auth:
  # This placeholder is denylisted and rejected at every boot - replace
  # it before starting the server. At least 32 bytes.
  jwt_secret: "REPLACE-ME-with-openssl-rand-hex-32-output"
  jwt_access_ttl: 15m
  jwt_refresh_ttl: 720h

# The second HTTPS listener for mTLS agent traffic (heartbeat, join,
# console). Required for a working cluster.
agent_server:
  enabled: true
  listen: "0.0.0.0:8443"

# Outbound CP -> agent client. Required when workers.enabled is true.
# mTLS material is sourced automatically from the cluster CA at boot.
agent_client:
  enabled: true

# In-process job dispatcher + periodic scheduler (default on).
workers:
  enabled: true

cluster_ca:
  cert_file: "/var/lib/otherix/ca/cluster-ca.crt"
  key_file:  "/var/lib/otherix/ca/cluster-ca.key"

# Per-replica CP server cert. Auto-generated from the cluster CA by
# default; list every hostname/IP the agent will dial.
cp_cert:
  additional_sans:
    # - "cp.example.com"
    # - "10.0.0.100"

etcd:
  mode: "single"
  name: "otherix-0"
  data_dir: "/var/lib/otherix/etcd"

storage_pools:
  # The CP auto-provisions a "default" pool on every node as it becomes
  # ready, so `vm create` works without --pool. Set to "" to opt out and
  # manage pools manually.
  default_pool_name: "default"
```

Everything omitted falls back to documented defaults (see
`deploy/config/api.example.yaml` for the full set, including the
placement scheduler and node-pressure knobs).

!!! note "etcd peer URL is always HTTPS"
    Peer (Raft) mTLS is always on, even single-node, so a later grow to
    HA needs no transport switch. The api-server auto-generates the peer
    cert from the cluster CA on each boot. The default `peer_url: auto`
    resolves to this host's routable IPv4 at boot (falling back to
    loopback only when no route is found), so a later grow to HA needs no
    change here.

## Running the control plane

```bash
otherix-api --config /etc/otherix/api.yaml
```

The process embeds etcd, runs the bootstrap hooks (admin, cluster CA,
default-pool seed), generates its server cert, and serves the API. It
blocks until it receives SIGINT/SIGTERM, then shuts down gracefully
within `server.shutdown_grace`.

Other invocations:

```bash
otherix-api --version                  # print version and exit
otherix-api --hash-password 'plain'    # print an argon2id hash and exit
```

Health probes live outside `/v1/` for Kubernetes:

- `GET /healthz` - liveness (process up, no dependency calls).
- `GET /readyz` - readiness (checks the store; 503 when not ready).

## Single node vs. HA

The config above runs a single self-clustering api-server
(`etcd.mode: single`). For a multi-replica control plane that forms one
etcd cluster over peer mTLS, see
[High availability](../operations/high-availability.md).

## Next steps

- [Quickstart](quickstart.md) - boot a VM end to end.
- [Join a node](../guides/join-a-node.md) - enrol an agent with the
  join-token bootstrap protocol.
- [Architecture](../architecture.md) - how the pieces fit together.
