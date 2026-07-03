# Otherix - Architecture

A self-hosted control plane for running QEMU virtual machines across a fleet of Linux
hypervisor nodes. This document is a high-level orientation for engineers with a
devops/SRE background: what the moving parts are, how they talk, and where state lives.

---

## What it is

Otherix manages the full lifecycle of VMs (create, start/stop, console, delete) on bare
hypervisors. A VM is created directly from a disk-image URL - there is no template or image
registry. Users own VMs; nodes, networks, and storage pools are shared infrastructure managed
by administrators. The system is operated through a kubectl-style CLI (`otherix`) that speaks a
REST API, including declarative multi-document YAML manifests (`otherix create -f`).

It is deliberately small in surface: no multi-tenancy, no external database, no Postgres/Redis,
no libvirt. Just three Go binaries and embedded etcd.

---

## The three processes

```
  operator ── otherix (CLI) ──REST──▶ otherix-api ──embeds──▶ etcd
                                          │  (single stateful store, in-process)
                                          │ mTLS over a cluster CA
                                          ▼
                                     otherix-agent  (one per hypervisor node)
                                          │
                                          ▼
                                     QEMU + Linux networking
```

**`otherix-api`** - the control plane. A single self-contained process that:
- serves the public REST API (JWT or `otx_` API-token auth);
- **embeds an etcd member in-process** - the only stateful component;
- runs the async job dispatcher and periodic maintenance loops in the same process (there is no
  separate scheduler or reconciler daemon);
- talks to agents over mutual TLS.
For HA, multiple `otherix-api` replicas self-cluster into one etcd Raft cluster and share work by
claiming jobs off the etcd-backed queue.

**`otherix-agent`** - the per-node daemon (Linux only). Owns the QEMU processes, the node's local
image cache, and the host networking data plane (bridges, tap devices, VXLAN overlay, WireGuard
mesh, nftables NAT). It keeps its own state on the local filesystem and **never touches the control
plane's etcd** - all coordination flows through a periodic heartbeat.

**`otherix`** - the CLI. Cobra-based, kubectl/gh ergonomics, multi-cluster config under
`~/.otherix/config`.

> Build note: `make build` produces `otherix-api`, `otherix-agent`, and `otherix`. The control plane
> is one process; "scheduler" and "reconciler" are in-process loops, not separate binaries.

---

## How state is split

There are two sources of truth, and keeping them separate is the core design choice:

| | Desired state | Observed state |
|---|---|---|
| **What** | What the user asked for: VM cpu/memory/disks, networks, pools | What is actually running: qemu pid, phase, metrics, cached images |
| **Where** | Control-plane etcd (`internal/etcdstore`) | On the agent's local disk |
| **How it moves** | Pushed down in each heartbeat response | Reported up in each heartbeat request |

The control plane writes *desired* state and the agent's reconcilers converge toward it,
reporting *observed* state back. A VM's API view shows both: top-level fields are what you want;
a derived `status` reflects what the agent last reported. This is the same desired/observed loop
Kubernetes uses, on a 30-second heartbeat.

---

## Storage: embedded etcd

etcd runs inside `otherix-api` (no network hop for the control plane's own reads/writes). It is the
single stateful service - there is no SQL database and no schema migrations; structure is enforced
in application code (`internal/etcdstore`).

- **Keys** are laid out as `/otherix/<resource>/<id>` (JSON values), with `uniq/...` keys for
  uniqueness (e.g. VM name, user email) and `index/...` keys for list ordering.
- **Atomicity** comes from etcd transactions: a row plus its uniqueness guards plus its async job all
  commit in one compare-and-set transaction. Large cleanup sweeps are chunked under etcd's
  per-transaction op limit.
- **HA** is etcd Raft. Replicas join as learners and are auto-promoted once caught up; peer (Raft)
  traffic is mutual-TLS, chained to the cluster CA.
- **Backups** are periodic etcd snapshots written to a configurable directory with retention.

Cluster-wide settings (default pool, overlay supernet, VNI range, underlay MTU) live in a single
etcd document, seeded once at boot, rather than being duplicated across each replica's config file.

---

## Async operations and the job queue

Anything that touches a node or takes more than a moment is asynchronous:

1. The API validates the request, writes a **task** plus a **job** to etcd in one transaction, and
   returns `202 Accepted` with a task id.
2. The in-process **dispatcher** claims the job (one replica wins via a compare-and-set), calls the
   owning agent, and polls it to completion.
3. The client polls `GET /v1/tasks/{id}` (or uses `--wait`) until the task reaches
   `success` / `failed` / `cancelled`.

Sync `200` is reserved for genuinely fast operations (pause/resume, console-token issuance). VM
create/delete, start/stop/poweroff/reboot, and pool scans are all async.

The queue is etcd-backed (claim-by-revision, per-job attempt budget). Job redelivery is safe: a task
carries the agent's task id, so a control-plane restart resumes polling instead of re-running the
operation, and a committed-terminal task is never reopened.

Periodic loops handle node liveness (promote healthy / mark unreachable / mark gone by heartbeat
freshness), expired-row cleanup, storage-pool scan triggers, and etcd backups.

---

## Security model

- **Users** authenticate with passwords (argon2id) to get a short-lived JWT (15 min) plus a rotating
  refresh token; or with long-lived `otx_` API tokens. Refresh tokens carry theft detection - reusing
  a revoked one burns the whole token family.
- **RBAC** has four fixed roles (`admin`, `operator`, `developer`, `viewer`) and a static
  permission matrix with `own`/`any` scopes. Cross-user resources return `404`, never `403`, so
  existence is never leaked.
- **Agents** authenticate to the control plane with **mutual TLS**. A per-cluster CA is generated on
  first boot; each node enrolls by submitting a CSR with a one-time join token, and its identity is
  its certificate CN (`node-<name>`). The control-plane API verifies agents by certificate
  fingerprint.
- **Bootstrap** is trust-on-first-use: an enrolling agent fetches the cluster CA over a pinned
  fingerprint, then all subsequent traffic is verified against that CA.

---

## Networking (agent data plane)

Each node's agent programs the host network directly via netlink/nftables (no libvirt, no OVS):

- a **Linux bridge** per managed network, with **tap** devices wired into QEMU;
- a **VXLAN overlay** for cross-node VM traffic, with a controller-authoritative forwarding database
  (the control plane computes the FDB; agents do not learn);
- a **WireGuard mesh** carrying the overlay between nodes (the control plane recomputes the full peer
  set each heartbeat; agents just apply it);
- **nftables masquerade** for VM egress to the internet.

MTUs are sized for the encapsulation stack (1500 underlay, 1440 WireGuard, 1390 overlay).

**Overlay egress (anycast gateway + DNS).** VMs on a private overlay reach the internet through
per-node SNAT - the model Kubernetes CNIs use: each node masquerades its own overlay traffic out its
uplink, so there is no central egress gateway to bottleneck or fail. The subtlety is live migration: a
VM has to keep working after it moves to another node, so the
overlay is designed up front so everything it depends on is made
*anycast* - identical on every node. Its **default gateway** is a fixed link-local address
(`169.254.1.1`, with a deterministic per-overlay MAC) present on every node's overlay bridge, so the
local node always answers ARP and routes the VM's egress no matter where it runs (being link-local,
it also costs no address from the tenant's subnet). **DNS** is served at that same address by a small
per-node forwarder that relays to whatever upstream resolver the node itself uses. The guiding rule:
a VM is only ever handed *anycast* addresses (gateway, DNS), never a node-specific one - so nothing it
caches breaks when it migrates. (The node also installs a route for the overlay subnet back to the
bridge, so return traffic finds the VM; egress is opt-in per network.)

---

## Ingress and VM access

VMs live on private overlay or bridge subnets with no public IP. A client reaches
a VM **by name** by asking the control plane to broker access to a `(VM, port)`:
the CP mints a short-lived, narrowly-scoped credential and hands back where to
connect. Overlay VMs are reached through an **ingress gateway** - a **node role** (an L4
forwarder that joins the overlay), served either co-located with a hypervisor or
standalone on a KVM-less host; the client dials the gateway's advertised endpoint
and the CP stays out of the data path, and the connection **survives a live
migration** of the guest. Bridge VMs are
reached through a CP **relay** (a WebSocket the CP splices to the owning agent over
mTLS). On top of this, a logical **load balancer** fronts a label-selected pool of
VMs with active L4 health checks, brokering each connection to one healthy backend.
This is what powers `otherix ssh`, `otherix forward`, `otherix lb`, and scoped
external `ingress-grant`s - all without a public IP per VM, an AS, or a bastion.
See [Ingress and load balancing](concepts/ingress.md).

---

## VM lifecycle on a node

QEMU is driven directly (no libvirt), controlled over a QMP socket. The agent:
- materializes the disk image from a per-pool, basename-keyed cache with `IfNotPresent` semantics and
  optional SHA-256 enforcement;
- builds a NoCloud cloud-init seed ISO when user-data is supplied;
- launches `qemu-system-{x86_64,aarch64}` (KVM when `/dev/kvm` is present, TCG otherwise);
- exposes the serial console over a WebSocket bridge for `otherix vm console`.

Lifecycle operations are guarded per-VM so concurrent requests can't race, and graceful stop never
force-kills - destructive actions fail toward inaction.

---

## Repository map

| Path | What lives there |
|---|---|
| `cmd/{api,agent,cli}` | The three binary entry points |
| `internal/api` | REST server, router, middleware, handlers, response envelope |
| `internal/etcd` | Embedded etcd runtime, clustering, backups |
| `internal/etcdstore` | The control-plane store over etcd (key schema, transactions) |
| `internal/store` | Shared row/params/result types and error sentinels |
| `internal/auth` | Passwords, JWT, tokens, RBAC, CSR signing |
| `internal/worker` | Async dispatcher + periodic scheduler |
| `internal/agent` | Node runtime: VM manager, QEMU, netfabric, heartbeat, reconcilers |
| `internal/agentapi` | Generated CP-to-agent API (from `api/openapi/agent.yaml`) |
| `internal/config` | koanf-based config (env prefix `OTHERIX_`) |
| `api/openapi` | The two API contracts (control-plane + agent) |
| `deploy/`, `dev/` | Container images, example configs, Lima dev tooling, smoke tests |

---

## Operational notes

- **Config** is YAML plus environment overrides (`OTHERIX_` prefix, `__` for nesting). The control
  plane needs a data directory for etcd and a place for its CA and certs; the agent is configured by
  the one-shot `otherix-agent bootstrap` command and then runs `otherix-agent serve`.
- **Images**: control plane is distroless; the agent image is for dev/CI only (running real VMs needs
  host KVM and privileges).
- **Tests**: unit tests run with `make test`; etcd-backed integration tests (store + API end-to-end,
  all embedding etcd in-process, no Docker) with `make test-etcd`; the Linux data-plane suite (real
  bridges/taps/VXLAN/WireGuard in network namespaces) with `make test-netfabric`.
- **Architectures**: amd64 and arm64. The agent is Linux only; macOS is supported as a development
  platform via Lima.
- **Upgrade ordering (CP before agents)**: the heartbeat receiver decodes with
  `DisallowUnknownFields`, so the heartbeat wire contract is neither forward- nor
  backward-compatible across a field add or removal. When a release changes the heartbeat shape,
  upgrade the control plane first, then the agents. During the window a not-yet-upgraded agent may
  be rejected (`400`) and demoted `ready -> unreachable -> gone`; this is non-destructive (running
  VMs are preserved, `vm_runtime` is never orphaned by the demotion) and self-heals once the agent
  is upgraded. Upgrading agents first can break heartbeats against the old CP.
