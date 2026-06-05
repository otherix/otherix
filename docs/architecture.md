# Otherix — Architecture

**Status:** api-server, agent, and CLI are built and exercised
end-to-end through the `tests/apie2e/` suite. Scheduler and reconciler
loops run in-process inside `otherix-api`, alongside the embedded etcd
member and the worker job runtime. The control plane is wired through
the join-token bootstrap protocol against the embedded etcd store.

This document is the high-level orientation. The control-plane store
lives in `internal/etcdstore/`; the embedded etcd member is started by
`internal/etcd/`; the pgx-free row / params / result types shared by
handlers and the store live in `internal/store/`.

---

## What Otherix is

A self-hosted VM orchestration control plane that manages QEMU virtual machines across a fleet of hypervisor nodes. VMs are created directly from an image URL (no template entity). Users own VMs and snapshots (tracked via per-resource `owner_id`); nodes, networks, storage pools, and firmwares are shared infrastructure managed by administrators. The scheduler enforces placement policies. Live migration moves running VMs between nodes peer-to-peer without involving the control plane in the data path.

**Non-goals (deliberately):** multi-tenancy / SaaS, custom RBAC, OAuth/OIDC, NFS/Ceph storage, OVS/VXLAN networking, OCI registry images. All deferred to future iterations. (Multi-tenancy was scaffolded and then removed in 2026-05-03.)

---

## Components

```
┌──────────────────────────────────────────────────────────┐
│                  Control Plane (otherix-api)             │
│                                                          │
│  ┌──────────┐  ┌───────────┐  ┌────────────┐  ┌───────┐  │
│  │ REST API │  │ scheduler │  │ reconciler │  │worker │  │
│  │ + mTLS   │  │ (in-proc) │  │  loops     │  │ jobs  │  │
│  └────┬─────┘  └─────┬─────┘  └─────┬──────┘  └───┬───┘  │
│       │              │              │             │      │
│       └──────────────┴──────┬───────┴─────────────┘      │
│                             │                            │
│                    ┌────────┴────────┐                   │
│                    │  embedded etcd  │                   │
│                    │   (in-process)  │                   │
│                    └─────────────────┘                   │
└──────────────────────────┬───────────────────────────────┘
                           │  REST + mTLS
                           │  (CP ↔ agent)
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   ┌────▼────┐        ┌────▼────┐        ┌────▼────┐
   │  agent  │        │  agent  │        │  agent  │
   │  (node) │        │  (node) │        │  (node) │
   └─────────┘        └─────────┘        └─────────┘
        ▲                  ▲                  ▲
        └──── peer-to-peer QMP migrate ───────┘
```

**`otherix-api`** - the public REST API. Embeds the etcd member and
hosts the scheduler, reconciliation loops, and the worker job runtime
in-process (no separate worker / scheduler / reconciler tiers, no
external DB). Reads/writes desired state through `internal/etcdstore`.
Designed for HA - replicas self-cluster as a single etcd cluster and
share work by claiming jobs off the etcd-backed queue.

**Scheduler (in-process)** — picks a node for each new VM and decides
when to evacuate/rebalance. Reads node capacity, VM
architecture, and labels. Writes nothing user-facing; emits work by
enqueuing jobs on the etcd queue.

**Reconciler (in-process)** — closes the loop between desired state
(`vms`, `vm_disks`, `vm_nics`, storage pools, …) and observed state
(`vm_runtime`, per-resource status columns). When desired generation
advances past `observed_generation`, the matching loop queues agent
work and bumps `observed_generation` once the agent confirms.

**`otherix-agent`** — runs on each hypervisor node. Talks QEMU
directly via QMP (no libvirt). Reports node capabilities through
heartbeat, applies VM lifecycle commands, performs live migrations
peer-to-peer. Authenticated to the CP via mTLS client certificate
issued through the `join_tokens` bootstrap protocol.

**`otherix`** — operator CLI. Not a cluster component; installed
wherever an operator runs commands. Talks to the CP over HTTPS with
a bearer token (JWT or `otx_*` API token).

**Deployment:** standalone binaries. The control plane runs on a
dedicated host or alongside an agent for single-node installations;
agents install on each KVM/QEMU host alongside `qemu-system-*`. The
control plane embeds etcd in-process, so it has no external stateful
dependency to bring up.

---

## Data flow patterns

### Desired → Observed (reconciliation)

User issues `PATCH /v1/vms/X { cpu_cores: 4 }` to api-server. The handler:
1. Updates the VM row and increments `vms.generation` in the same etcd `Txn` (because `cpu_cores` is a whitelisted, generation-bumping field). The bump is applied application-side in `internal/etcdstore`, not by a DB trigger.
2. Returns 200 to the user.

Asynchronously, reconciler observes `vms.generation > vm_runtime.observed_generation` for VM X. It enqueues a "reconcile VM" job on the etcd queue. A worker:
1. Reads desired state, computes the diff vs observed.
2. Issues an agent RPC ("resize VM X cpu to 4").
3. Agent applies via QMP, reports back.
4. Worker updates `vm_runtime.observed_generation = vms.generation`.

If the user changes `description` instead, `generation` does NOT bump (cosmetic-only fields are excluded from the bump whitelist) and reconciler does no work.

### Live migration

Live migration is a planned first-class resource. What exists today is the migration row model, active-migration tracking (`activeMigrationsOnNode`, consumed by node force-delete), and cancellation of active migrations when a node is force-deleted; the create endpoint and the phase-driving worker are not yet implemented. The intended flow:

User issues `POST /v1/vms/X/migrate { target_node: Y }`. The handler:
1. Writes the migration row with `phase='pending'` plus a per-VM active-migration guard key, committed together in a single etcd `Txn` whose compare-and-set on the guard's `CreateRevision` rejects a second active migration for VM X - the etcd analogue of the former partial unique index.
2. Returns 202 with the migration ID.

A worker then drives the migration through `preparing -> running -> completing -> succeeded|failed|cancelled`. The agents on source and target talk QMP `migrate` directly to each other (peer-to-peer); the worker just polls progress and updates the migration's `bytes_transferred`, `progress_percent`, etc.

### Audit

A dedicated audit-log subsystem is backlog and not yet shipped. The audit
records that do exist today are written inline alongside the operation that
produces them: token-redemption consumption rows (`join_token_consumptions`
and the cluster-join equivalent) are committed in the same etcd `Txn` as the
cert issuance / node upsert they record, so the audit and its operation
land together or not at all. There is no partitioned audit table; retention
of finalized records is handled by the periodic Scheduler sweeps (see
"Background jobs and maintenance" below), not by detaching monthly partitions.

---

## Store design - the big picture

Embedded etcd is the **only** stateful service. No Postgres, no Redis, no
Kafka, no separate queue process. The async job queue lives in the same etcd
keyspace, drained by the in-process worker runtime. There is no SQL and no
migrations: the store (`internal/etcdstore`) enforces all structure
application-side over an etcd key-schema, and `internal/store` is a pgx-free
type layer (row / params / result structs + sentinels) shared by handlers and
the store.

### Key-schema and resource families

Each resource is a JSON value under a primary key `/otherix/<resource>/<id>`.
Uniqueness constraints are guard keys (an extra key whose presence blocks a
conflicting write); lookups by a non-primary attribute go through secondary
index keys (e.g. `/otherix/index/vm_runtime/node/<node_id>/<vm_id>`). Jobs are
sequence-keyed under `/otherix/jobs/<seq>`. The resource families:

| Family | Resources | Soft-delete? |
|---|---|---|
| Identity | `users`, `api_tokens`, `agent_certs`, `join_tokens`, `refresh_tokens`, `ca_certs` | partial |
| Infrastructure | `nodes`, `firmwares`, `storage_pools`, `networks` | partial |
| VM domain (desired) | `vms`, `vm_disks`, `vm_nics`, `snapshots` | yes |
| VM domain (runtime) | `vm_runtime` | hard delete |
| Operations | `migrations` (live), `idempotency_keys`, `tasks` | hard delete |
| Queue | jobs under `/otherix/jobs/<seq>`, sequence counter `/otherix/seq/jobs` | hard delete |

### Conventions enforced everywhere

- **IDs:** UUIDv7 - sortable by creation time, generated application-side.
- **Timestamps:** RFC 3339 UTC. Standard triplet `created_at`, `updated_at`, `deleted_at` on soft-deletable resources; `updated_at` is stamped by the store on each mutation.
- **Soft delete:** `deleted_at` set on the row. Application-level cascade. Uniqueness guard keys are released on soft-delete so the value (e.g. an email, a node name) becomes reusable immediately.
- **Atomicity:** multi-key writes (primary row + guards + secondary indexes + any related rows) commit through a single etcd `Txn` with compare-and-set on `ModRevision`/`CreateRevision`. Sweeps that exceed etcd's default 128-op/txn limit are split into chunks by `commitInChunks`.
- **Resource ownership:** user-owned resources (`vms`, `snapshots`) carry `owner_id` referencing a user; deleting a user that still owns resources is refused. Infrastructure resources (`nodes`, `firmwares`, `storage_pools`, `networks`) have no owner - they're administered for the whole installation. Multi-tenancy was removed in 2026-05-03.

### Image cache (agent-owned)

A VM is created from an image URL rather than a control-plane template. The
image bytes are NOT a control-plane resource: each agent owns a per-pool,
basename-keyed cache under `{poolRoot}/images/{basename}` (with a
`{basename}.sha256` sidecar), materialized on first use with Kubernetes
`IfNotPresent` semantics. Without `--image-sha256` the cache reuses by name;
with `--image-sha256` it enforces the digest, re-downloading and atomically
overwriting on sidecar disagreement and failing `checksum_mismatch` if the URL
serves a different digest. The agent reports its cache inventory to the CP
through heartbeat, and the CP surfaces it under `storage_pool:read` (shown by
`otherix pool get <name>` as an `images:` list of name, sha, and size). There
is no separate image resource, table, or RBAC permission - `vm:create` is the
single gate for materializing any image URL.

### Reconciler pattern (Kubernetes-style)

Every desired-state resource has a `generation` bumped by the store on a whitelist of fields. The matching runtime resource carries `observed_generation`. Reconciler scans for `generation > observed_generation` and converges. Cosmetic-only field changes don't bump `generation` and don't trigger reconciler work.

`vm_runtime.conditions` carries Kubernetes-style condition arrays (`{type, status, reason, message, last_transition_time}`) so UI can show rich state without a combinatorial enum explosion.

### Key invariants enforced by the store

The store enforces these application-side, each through a guard key compared
inside the mutating `Txn` (the etcd analogue of a partial unique index or
CHECK constraint), rather than by the database:

- One active live-migration per VM (per-VM active-migration guard key, released on terminal phase) - planned, with the live-migration create path.
- Live-migration source and target are different.
- VM disk source kind is one of `image` or `blank`. There is no template entity and no `source_template_id`: a VM is self-describing, carrying `image_url`, `image_sha256`, `image_format`, `architecture`, and `firmware_id` on its own row. The image bytes are materialized by the agent's per-pool image cache on first use (see the storage section), not tracked as a control-plane resource.
- Globally unique MAC address among non-deleted NICs (MAC guard key).
- Exactly one default firmware per `(architecture, type)` (default-firmware guard key).
- Exactly one default storage pool per node (default-pool guard key).
- Network VLAN tag in 1..4094 or NULL (validated at the API edge).

---

## Structure and schema

There is no schema and no migration tooling: etcd carries opaque keys and
values, and the store (`internal/etcdstore`) is the single authority for
structure. It owns the key-schema described above (primary
`/otherix/<resource>/<id>` JSON values, uniqueness guard keys, secondary
indexes, and the `/otherix/jobs/<seq>` queue), and every constraint that a
relational schema would have expressed as a unique index, CHECK, or FK action
is instead enforced by a compare-and-set inside the relevant store method's
`Txn`. There is no goose, no `--migrate-action`, no sqlc, and no `pgx`.
`internal/store` carries only the row / params / result types and sentinel
errors shared by handlers and the store.

### Background jobs and maintenance

The async queue replaces what was previously an in-database job table. Jobs
are written under `/otherix/jobs/<seq>` (a monotonic sequence allocated by a
value-compare CAS loop on `/otherix/seq/jobs`, zero-padded so lexical key
order matches numeric order). Two in-process components consume them:

- **`internal/worker.Dispatcher`** - polls the queue oldest-first, claims a
  pending job (a CAS that loses cleanly if another replica won), routes it to
  the handler registered for its `Kind`, and governs the job's queue lifecycle
  (`pending → running → completed`, or requeue/fail under the kind's attempt
  budget). A bounded-concurrency pool caps in-flight work. This is the
  replacement for the former in-process worker pool.
- **`internal/worker.Scheduler`** - runs registered periodic functions on
  independent tickers (the replacement for the former periodic-job scheduler).
  It drives node-health reconciliation, the storage-pool scan trigger, and the
  retention sweeps. Per-state task retention (7d completed / 30d
  failed-or-cancelled) and failed-job retention (7d) are swept here;
  because the default `--max-txn-ops` is 128, retention sweeps commit in chunks
  below that limit.

---

## Testing strategy

Two test tiers, the integration tier build-tag gated. Neither needs Docker -
both embed etcd in-process.

**Unit tests** (`make test`) - plain `go test ./...` with the
`test_fast_argon` tag swapping OWASP Argon defaults for RFC 9106
minimums in the test binary. The `integration`-gated suites are skipped.

**Store / API integration** (`make test-etcd`, build tag `-tags=integration`)
- covers `internal/etcdstore` (the full store surface over an embedded etcd
member started per test) and `tests/apie2e` (the api-server e2e suite: the
real chi router over etcdstore + `httptest`, driving auth rotation /
theft-detection, user + infra CRUD, RBAC, idempotency, and health). Real-agent
CP↔agent coverage is the Lima smoke (`make local-dev-start`), not a mock tier.

Coverage targets for the store suite specifically:
1. **Guards fire on synthetic violations** - uniqueness guard keys (node name,
   email, MAC, default firmware / pool) reject conflicting writes; the
   active-migration guard rejects a second active migration.
2. **Generation bump behaves** - `updated_at` advances on mutation;
   `generation` bumps on whitelisted fields, no-ops on cosmetic, handles
   NULL transitions.
3. **Atomicity holds** - multi-key writes (row + guards + indexes) land or roll
   back together; delete projections stay under the 128-op txn budget.
4. **Queue lifecycle** - claim/complete/retry transitions and at-least-once
   redelivery resumption behave as specified.

Both tiers run in CI without Docker.

---

## Operations: overlay MTU and VNI range

The cluster overlay's `underlay_mtu` and `vni_range` are
bootstrap-immutable. Each is seeded into the `cluster_settings`
singleton first-writer-wins from the api-server's local config; there
is no runtime mutator. Subsequent replicas read the seeded etcd value
and ignore their own local config, so the first api-server to boot wins.

- `underlay_mtu` is the physical underlay MTU. The derived overlay
  inner MTU is `underlay_mtu - 110` (the WireGuard + VXLAN encapsulation
  overhead), and it must stay at or above 1280, the IPv6 minimum link
  MTU (RFC 8200). That floor is why the smallest seedable underlay is
  1390 (110 + 1280); the api-server validates this at config load.
- `vni_range` bounds VNI allocation and is likewise bootstrap-immutable.

To renumber the underlay MTU (or the VNI range): delete the
`cluster_settings` singleton key in etcd and re-seed, then recreate the
overlay networks. Existing overlay rows keep their stamped MTU - it is
snapshotted at create time and does not follow a later reseed.

---

## Operations: recovering a gone node

A node that has transitioned to `gone` cannot be recovered by simply
re-bootstrapping the agent under the same name. The node-name guard and
the WireGuard public-key guard persist as long as the old node row
exists, so a re-bootstrap under the same name fails with a name
conflict. To reuse the name, the operator must first delete the old row
with `DELETE /v1/nodes/{id}` (which clears both guards), then run the
agent bootstrap again. Picking a fresh node name avoids the conflict
entirely but leaves the dead row behind for later cleanup.

---

## Operations: overlay FDB freshness during a gone window

Overlay forwarding-database (FDB) freshness is gated on CP liveness.
The VTEPs run with `nolearning`, so there is no agent-side TTL or aging
fallback to expire entries on their own. While the CP is down across a
node's `gone` transition, a stale unicast FDB entry pointing at the
dead VTEP persists on peer nodes and is not aged out locally. This is
accepted and bounded by CP recovery: on its next heartbeat round the CP
re-projects the FDB without the gone node, and peers converge to the
corrected set.

---

## Operations: orphaned qemu after a partitioned delete

Deleting a VM whose owning node is `unreachable` (a network
partition) can leak a qemu process on that node if the node later
returns. The delete worker (`runDelete` in
`internal/api/handlers/vms/run.go`) attempts a best-effort agent
teardown first and only projects the delete directly when that agent
call fails, so the common case (the node is reachable at delete time)
reaps qemu cleanly. The leak window is narrow: the node is
`unreachable` at delete time, the agent teardown does not land, and the
node later heals with the qemu still running. The agent's VM reconciler
does not prune VMs the CP has stopped declaring (it reports them and
waits), so the returned node keeps the orphaned qemu alive.

The leak is detectable. The returned node heartbeats the orphaned VM,
and the CP logs `heartbeat references unknown vm; skipping` (in
`applyVMs`, `internal/api/handlers/heartbeat/handle.go`) for a VM it no
longer knows. An operator who sees that log can identify the node and
manually reap the qemu process.

Authoritative agent-side teardown (the agent destroying a VM it
infers the CP no longer wants) was deliberately NOT adopted. Acting on
inferred absence risks destroying a VM that is still wanted - for
example during a CP-side projection lag or a transient store read - a
strictly worse outcome than a recoverable, detectable leak. The leak is
accepted as a known limitation; the teardown is left to the operator,
who can confirm the VM is genuinely deleted before reaping.

---

## Operations: delete projection op budget (forward constraint)

`ProjectVMDeleteSuccess` (`internal/etcdstore/vms_project.go`) commits the
VM soft-delete, its disks, NICs, secondary indexes, the runtime row and its
by-node index, and the task finalize in a single etcd transaction. etcd's
default `--max-txn-ops` is 128. The current create model is single-root-disk
+ single-NIC, so a delete is ~17 ops, far under the limit. If multi-disk or
multi-NIC VMs are introduced (for example by wiring the already-specified
`vmDisks.create` / `vmNics.create` hotplug-attach endpoints), the attach path
MUST enforce a per-VM disk+NIC budget that keeps the delete projection under
128 ops, or `ProjectVMDeleteSuccess` must chunk the delete. Chunking trades
atomicity for a partial-delete failure mode (orphaned indexes), so a per-VM
cap at the attach edge is preferred. The regression test
`TestVMDeleteProjectionStaysUnderTxnBudget`
(`internal/etcdstore/vms_project_optest_test.go`) guards the current bound:
it fails if a single VM's delete projection ever exceeds 100 ops without a
guard.

---

## Operations: idempotency is at-least-once, not exactly-once

The idempotency middleware buffers a mutating response and flushes it to
the client only after `CompleteIdempotencyKey` commits. This closes the
duplicate-execution window for a control-plane CRASH between a committed
side effect and the completion write: the client never sees a 2xx, so its
retry replays as a first attempt.

It does NOT make mutating requests exactly-once. A handler's side effect
(for example writing a task or VM row) and the idempotency completion are
two separate etcd writes, not one transaction. If the side effect commits
but `CompleteIdempotencyKey` then returns a transient error, the response
is still flushed and the idempotency row stays `in_flight`; after the
2-minute lease the row is reclaimable, so a client retry with the same key
re-runs the handler and can produce a duplicate side effect (a second
task, a second VM). Treat every mutating endpoint as at-least-once: design
side effects to tolerate a rare duplicate, or check current state before
acting on a retry.

True exactly-once requires committing the handler's side effect and the
idempotency completion in a single etcd transaction. That is a deliberate
redesign, tracked as backlog, not a band-aid: returning a 5xx when the
completion write fails would not close the window (it only changes which
signal the client receives, and a 5xx invites the retry that produces the
duplicate).

---

## What's next

The store, api-server, agent, and CLI are wired end-to-end.
Larger upcoming themes:

- **Live migration** - store shape lands, agent-side QMP plumbing is the
  next concrete deliverable.
- **VM snapshots** - wired through the store and reconciliation
  framework; CLI / API surface to follow.
- **Audit-log subsystem** - a first-class audit record store with its own
  retention sweep; backlog.
- **Cert rotation** — cluster CA, per-replica CP certs, and per-node
  agent certs land via the bootstrap protocol but the rotation
  loops are still backlog.
- **Observability** — Prometheus metrics, structured logging
  conventions, tracing.

Store additions deferred until they're actually needed:
- `node_networks` link resource (when bridge homogeneity assumption breaks)
- Shared storage pools (NFS)
- Multi-tenancy (removed 2026-05-03; revisit if a SaaS use case appears)
- Per-user / per-team resource quotas
- Custom RBAC roles
- OAuth identities / web sessions
- VXLAN/overlay networking
- OCI registry as image source
- Transfer-of-ownership endpoint for user-owned resources

---

## Pointers

- **Store source of truth:** `internal/etcdstore/`
- **Shared row / params / result types:** `internal/store/`
- **Embedded etcd runtime:** `internal/etcd/`
- **API e2e harness:** `tests/apie2e/harness_test.go`
- **Local dev stack:** `make local-dev-start` (one-shot api-server with
  embedded etcd + Lima + agent + CLI config)
- **Run tests:** `make test-etcd` (no Docker; embeds etcd in-process)
