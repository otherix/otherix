# Otherix — Architecture

**Status:** Initial schema landed 2026-05-02; api-server, agent, and
CLI are built and exercised end-to-end through the
`tests/integration/` suite. Scheduler and reconciler loops run
in-process inside `otherix-api`. The control plane is wired through
the join-token bootstrap protocol against a single Postgres.

This document is the high-level orientation. The canonical schema
lives in `internal/store/migrate/migrations/`; SQL queries live in
`internal/store/queries/`.

---

## What Otherix is

A self-hosted VM orchestration control plane that manages QEMU virtual machines across a fleet of hypervisor nodes. Users own VMs, templates, and snapshots (tracked via per-resource `owner_id`); nodes, networks, storage pools, and firmwares are shared infrastructure managed by administrators. The scheduler enforces placement policies. Live migration moves running VMs between nodes peer-to-peer without involving the control plane in the data path.

**Non-goals (deliberately):** multi-tenancy / SaaS, custom RBAC, OAuth/OIDC, NFS/Ceph storage, OVS/VXLAN networking, OCI registry images. All deferred to future iterations. (Multi-tenancy was scaffolded and then removed in 2026-05-03.)

---

## Components

```
┌──────────────────────────────────────────────────────────┐
│                  Control Plane (otherix-api)             │
│                                                          │
│  ┌──────────┐  ┌───────────┐  ┌────────────┐  ┌───────┐  │
│  │ REST API │  │ scheduler │  │ reconciler │  │ river │  │
│  │ + mTLS   │  │ (in-proc) │  │  loops     │  │ jobs  │  │
│  └────┬─────┘  └─────┬─────┘  └─────┬──────┘  └───┬───┘  │
│       │              │              │             │      │
│       └──────────────┴──────┬───────┴─────────────┘      │
│                             │                            │
│                    ┌────────┴────────┐                   │
│                    │   PostgreSQL    │                   │
│                    │   (single DB)   │                   │
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

**`otherix-api`** — the public REST API. Hosts the scheduler,
reconciliation loops, and `riverqueue/river` workers in-process (no
separate worker / scheduler / reconciler tiers). Reads/writes
desired state. Designed for HA — multiple replicas share work via
Postgres advisory locks.

**Scheduler (in-process)** — picks a node for each new VM and decides
when to evacuate/rebalance. Reads node capacity, template
architecture, and labels. Writes nothing user-facing; emits work via
river jobs.

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
agents install on each KVM/QEMU host alongside `qemu-system-*`. No
external dependencies beyond PostgreSQL.

---

## Data flow patterns

### Desired → Observed (reconciliation)

User issues `PATCH /v1/vms/X { cpu_cores: 4 }` to api-server. The handler:
1. Updates `vms.cpu_cores`. The `trg_bump_generation` trigger increments `vms.generation` (because `cpu_cores` is whitelisted).
2. Returns 200 to the user.

Asynchronously, reconciler observes `vms.generation > vm_runtime.observed_generation` for VM X. It queues a "reconcile VM" river job. A worker:
1. Reads desired state, computes the diff vs observed.
2. Issues an agent RPC ("resize VM X cpu to 4").
3. Agent applies via QMP, reports back.
4. Worker updates `vm_runtime.observed_generation = vms.generation`.

If the user changes `description` instead, `generation` does NOT bump (cosmetic-only fields are excluded from the trigger whitelist) and reconciler does no work.

### Live migration

User issues `POST /v1/vms/X/migrate { target_node: Y }`. The handler:
1. Inserts a row into `migrations` with `phase='pending'`. The partial unique index `uq_migrations_active_vm` rejects this insert if VM X already has an active migration — DB-level race protection.
2. Returns 202 with the migration ID.

A river worker drives the migration through `preparing → running → completing → succeeded|failed|cancelled`. The agents on source and target talk QMP `migrate` directly to each other (peer-to-peer); the worker just polls progress and updates `migrations.bytes_transferred`, `progress_percent`, etc.

### Audit

Every state-changing API call inserts into `audit_log` from application code (not via DB triggers — the trigger doesn't know the actor's user_id, request_id, IP). Audit rows have NO foreign keys, so they survive the deletion of the entities they reference. The table is RANGE-partitioned by month so old data can be detached/archived independently.

---

## Database design — the big picture

PostgreSQL 16 is the **only** stateful service. No Redis, no Kafka, no separate queue. River runs job storage on the same DB.

### Table families

| Family | Tables | Soft-delete? |
|---|---|---|
| Identity | `users`, `api_tokens`, `agent_certs`, `join_tokens` | partial |
| Infrastructure | `nodes`, `firmwares`, `node_firmwares`, `storage_pools`, `networks` | partial |
| Templates | `templates`, `node_image_cache` | templates only |
| VM domain (desired) | `vms`, `vm_disks`, `vm_nics`, `snapshots` | yes |
| VM domain (runtime) | `vm_runtime`, `vm_disk_runtime`, `vm_nic_runtime` | hard delete |
| Operations | `migrations` (live), `idempotency_keys`, `audit_log` | hard delete |
| River queue | `river_migration`, `river_job`, `river_leader`, `river_queue`, `river_client`, `river_client_queue` | hard delete |

21 own tables + 6 river tables + 4 audit_log partitions (3 monthly + 1 default).

### Conventions enforced everywhere

- **PKs:** `uuid primary key default uuid_generate_v7()` — sortable by creation time, no extension required (pure-SQL function lives in the migration).
- **Timestamps:** always `timestamptz`. Standard triplet `created_at`, `updated_at`, `deleted_at` on soft-deletable tables; `trg_set_updated_at` trigger advances `updated_at` automatically.
- **Soft delete:** `deleted_at timestamptz`. Application-level cascade (DB FK actions only fire on hard delete). Partial unique indexes use `WHERE deleted_at IS NULL` so values become reusable after soft-delete.
- **FK ON DELETE:** explicit on every FK. `cascade` for owned children, `restrict` for cross-domain links, `set null` where the reference is advisory.
- **Resource ownership:** user-owned tables (`vms`, `templates`, `snapshots`) carry `owner_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT`. Infrastructure tables (`nodes`, `firmwares`, `storage_pools`, `networks`) have no owner column — they're administered for the whole installation. Multi-tenancy was removed in 2026-05-03.

### Reconciler pattern (Kubernetes-style)

Every desired-state table has `generation bigint` bumped via `trg_bump_generation` on a whitelist of fields. The matching runtime table carries `observed_generation`. Reconciler scans for `generation > observed_generation` and converges. Cosmetic-only field changes don't bump `generation` and don't trigger reconciler work.

`vm_runtime.conditions jsonb` carries Kubernetes-style condition arrays (`{type, status, reason, message, last_transition_time}`) so UI can show rich state without a combinatorial enum explosion.

### Key invariants enforced at the DB level

- One active live-migration per VM (`uq_migrations_active_vm` partial unique on non-terminal phases).
- Live-migration source and target are different (`chk_migrations_distinct_nodes`).
- VM disk source kind is consistent (`chk_vm_disks_template_required`: `template` ↔ non-null `source_template_id`, `blank` ↔ null).
- Template hard-delete is RESTRICTed by referencing vm_disks (FK action), forcing operators to detach before deleting.
- Globally unique MAC address among non-deleted NICs.
- Exactly one default firmware per `(architecture, type)` (partial unique).
- Exactly one default storage pool per node (partial unique).
- Network VLAN tag in 1..4094 or NULL (CHECK).

### Audit log specifics

`audit_log` is RANGE-partitioned by `created_at` (monthly). The init migration seeds 3 monthly partitions plus an `audit_log_default` DEFAULT partition. The DEFAULT prevents writes from failing if the auto-partition job (a recurring river job, not yet shipped) hasn't pre-created the next month's partition. No foreign keys — audit must outlive the entities it logs.

---

## Migration tooling

`pressly/goose v3` used as a library (not the CLI) - migrations are
embedded into the `otherix-api` binary via `go:embed` from
`internal/store/migrate/migrations/` and applied via
`otherix-api --migrate-action=up|down|status`, wrapped by
`make migrate-up` / `migrate-down` / `migrate-status`.

The init migration `00001_init.sql` carries the full v1 schema with
`-- +goose Up` / `-- +goose Down` markers. Function bodies wrap
with `-- +goose StatementBegin` / `StatementEnd`. The file uses
`-- +goose NO TRANSACTION` because river migration 4's
`ALTER TYPE ADD VALUE` is referenced by migration 6's function
(PostgreSQL forbids that within one transaction).

Subsequent migrations land as new files (`00002_*.sql` or
`00002_*.go` for Go-bridge migrations). Goose computes deltas;
`00001_init.sql` is never modified after launch.

### River queue sync

River's own SQL migrations are embedded **verbatim** from upstream
(`riverqueue/river v0.35.1`) into `00001_init.sql` between
clearly-marked delimiters. Following that block, an
`INSERT INTO river_migration (line, version) SELECT 'main', v FROM generate_series(1, 6)`
records all bundled versions as applied so `rivermigrate.Migrator`
doesn't try to re-run them.

Subsequent river upgrades ship as goose Go-bridge migrations
(`00002_river_v6.go` is the current pattern). The
`TestRiverHasNoPendingMigrations` test verifies the embedded schema
agrees with the rivermigrate API on every run.

---

## Testing strategy

Three test tiers, all build-tag gated.

**Unit tests** (`make test`) - plain `go test ./...` with the
`test_fast_argon` tag swapping OWASP Argon defaults for RFC 9106
minimums in the test binary. No Docker.

**Migration / store / API integration** (`make test-migrations`) -
runs against a fresh PostgreSQL 16 container started by
testcontainers-go. One container per `go test` invocation
(initialised via `TestMain`); all tests share it via unique keys.
Covers `tests/migrations/`, `internal/store/...`,
`internal/api/...`, `internal/auth/...`, `internal/agent/...`.

**CP↔agent integration** (`make test-integration`) - exercises the
control plane against an in-process mock agent through the OpenAPI
contract. Covers VM lifecycle, console + logs, storage pool scan,
template + storage-image reconciliation, RBAC and idempotency
end-to-end.

Coverage targets for `tests/migrations/` specifically:
1. **Constraints fire on synthetic violations** — partial unique
   indexes, CHECKs, FK actions.
2. **Triggers behave** — `set_updated_at` advances on UPDATE;
   `bump_generation` fires on whitelisted fields, no-ops on
   cosmetic, handles NULL transitions, warns on unknown field
   names.
3. **River sync stays in lockstep with upstream** —
   `rivermigrate.Migrator` dry-run reports zero pending.
4. **Down→Up is idempotent** — separate isolated container exercises
   the full goose cycle.

All three tiers require Docker for the integration tiers and run in
parallel CI jobs.

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

## What's next

The schema, api-server, agent, and CLI are wired end-to-end.
Larger upcoming themes:

- **Live migration** — schema lands, agent-side QMP plumbing is the
  next concrete deliverable.
- **VM snapshots** — wired through the schema and reconciliation
  framework; CLI / API surface to follow.
- **Auto-partition job** — recurring river job creating next-month
  `audit_log` partitions ahead of time.
- **Cert rotation** — cluster CA, per-replica CP certs, and per-node
  agent certs land via the bootstrap protocol but the rotation
  loops are still backlog.
- **Observability** — Prometheus metrics, structured logging
  conventions, tracing.

Schema additions deferred until they're actually needed:
- `node_networks` link table (when bridge homogeneity assumption breaks)
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

- **Schema source of truth:** `internal/store/migrate/migrations/`
- **SQL queries (sqlc input):** `internal/store/queries/`
- **Test harness:** `internal/migrationtest/harness.go`
- **Local dev stack:** `make local-dev-start` (one-shot Postgres +
  CP + agent + CLI config); `make dev-up` for just Postgres
- **Run tests:** `make test-migrations` (Docker required)
