# Otherix — Architecture

**Status:** v1 schema landed (2026-05-02). Application components (api-server, scheduler, reconciler, agent) not yet built — their shape informs the schema but their code lives in subsequent work.

This document is the high-level orientation. For the canonical schema see `migrations/00001_init.sql`.

---

## What Otherix is

A self-hosted VM orchestration control plane that manages QEMU virtual machines across a fleet of hypervisor nodes. Users own VMs, templates, and snapshots (tracked via per-resource `owner_id`); nodes, networks, storage pools, and firmwares are shared infrastructure managed by administrators. The scheduler enforces placement policies. Live migration moves running VMs between nodes peer-to-peer without involving the control plane in the data path.

**Non-goals (deliberately):** multi-tenancy / SaaS, custom RBAC, OAuth/OIDC, NFS/Ceph storage, OVS/VXLAN networking, OCI registry images. All deferred to future iterations. (Multi-tenancy was scaffolded and then removed in 2026-05-03.)

---

## Components (planned)

```
┌──────────────────────────────────────────────────────────┐
│                     Control Plane                        │
│                                                          │
│   ┌──────────────┐   ┌───────────┐   ┌───────────────┐   │
│   │  api-server  │   │ scheduler │   │  reconciler   │   │
│   │  (+ river    │   │           │   │               │   │
│   │   workers)   │   │           │   │               │   │
│   └──────┬───────┘   └─────┬─────┘   └───────┬───────┘   │
│          │                 │                 │           │
│          └─────────────────┼─────────────────┘           │
│                            │                             │
│                    ┌───────┴────────┐                    │
│                    │   PostgreSQL   │                    │
│                    │   (single DB)  │                    │
│                    └────────────────┘                    │
└──────────────────────────┬───────────────────────────────┘
                           │  REST + mTLS
                           │  (CP → agent)
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   ┌────▼────┐        ┌────▼────┐        ┌────▼────┐
   │  agent  │        │  agent  │        │  agent  │
   │  (node) │        │  (node) │        │  (node) │
   └─────────┘        └─────────┘        └─────────┘
        ▲                  ▲                  ▲
        └──── peer-to-peer QMP migrate ───────┘
```

**`api-server`** — the public REST API. Runs `riverqueue/river` job workers in-process (no separate worker tier). Reads/writes desired state.

**`scheduler`** — picks a node for each new VM and decides when to evacuate/rebalance. Reads node capacity, template architecture, and labels. Writes nothing user-facing; emits work via river jobs.

**`reconciler`** — closes the loop between desired state (`vms`, `vm_disks`, `vm_nics`) and observed state (`vm_runtime`, etc.). When `desired.generation > observed_generation`, it queues work for the agent and updates `observed_generation` once the agent confirms.

**`agent`** — runs on each hypervisor node. Talks QEMU directly via QMP (no libvirt). Reports node capabilities, applies VM lifecycle commands, performs live migrations peer-to-peer. Authenticated to the CP via mTLS client certificate (issued through the `join_tokens` bootstrap flow).

**Deployment:** CP in Kubernetes via Helm; agents installed directly on hypervisor hosts.

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

`pressly/goose v3` (chosen over `golang-migrate v4` after the latter pulled ~180 indirect dependencies because its CLI vendors every supported database).

Single init file: `migrations/00001_init.sql` with `-- +goose Up` / `-- +goose Down` markers. Function bodies wrapped with `-- +goose StatementBegin` / `StatementEnd`. The whole file uses `-- +goose NO TRANSACTION` because river migration 4's `ALTER TYPE ADD VALUE` is referenced by migration 6's function (PostgreSQL forbids that within one transaction).

The Down block does `drop schema public cascade; create schema public; ... insert into goose_db_version (0, true)` — the only honest rollback for a single-file init migration. Works because we re-seed goose's bookkeeping table after the drop.

Future schema changes ship as new files (`00002_*.sql`, etc.). Goose computes deltas; we never modify `00001_init.sql` after launch.

### River queue sync

River's own SQL migrations are embedded **verbatim** from upstream (`riverqueue/river v0.35.1`) into `00001_init.sql` between clearly-marked delimiters. Following that block, an `INSERT INTO river_migration (line, version) SELECT 'main', v FROM generate_series(1, 6)` records all bundled versions as applied so `rivermigrate.Migrator` doesn't try to re-run them.

To upgrade river: bump go.mod, read upstream changelog and migration delta, add **only the delta** as a new goose file (e.g., `00002_river_v0.36.0.up.sql`). Don't modify the embedded block. The `TestRiverHasNoPendingMigrations` test verifies the embedded schema agrees with the rivermigrate API on every run.

---

## Testing strategy

`tests/migrations/` holds 46 integration tests that run against a fresh PostgreSQL 16 container started by testcontainers-go. One container per `go test` invocation (initialized via `TestMain` in `main_test.go`); all tests share it. Each test inserts rows with unique keys (slug suffixes derived from `uuid_generate_v7()`) so cross-test state pollution doesn't matter.

Coverage targets, in order of importance:
1. **Constraints fire on synthetic violations** — partial unique indexes, CHECKs, FK actions.
2. **Triggers behave** — `set_updated_at` advances on UPDATE; `bump_generation` fires on whitelisted fields, no-ops on cosmetic, handles NULL transitions, warns on unknown field names.
3. **River sync stays in lockstep with upstream** — `rivermigrate.Migrator` dry-run reports zero pending.
4. **Down→Up is idempotent** — separate isolated container exercises the full goose cycle.

Run via `make test-migrations` (requires Docker, build tag `integration`). Typical runtime ~2–4 seconds.

---

## What's next

The schema is v1-complete. Following work, in rough dependency order:

1. **River jobs framework wiring** — generic worker bootstrap, then domain-specific workers.
2. **Auto-partition job** — recurring river job creating next-month `audit_log` partitions ahead of time.
3. **Idempotency-key GC job** — hourly delete of expired rows.
4. **api-server REST handlers** — VM CRUD, template management, migration initiation.
5. **mTLS issuance flow** — agent join-token redemption → CSR → signed cert.
6. **agent** — QMP wrapper, image cache, peer-to-peer migrate.
7. **scheduler** — placement decisions, eviction policies.
8. **reconciler** — desired→observed convergence loop.
9. **Helm chart** — CP deployment to Kubernetes.
10. **Observability** — Prometheus metrics, structured logging, tracing.

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

- **Schema source of truth:** `migrations/00001_init.sql`
- **Test harness:** `internal/migrationtest/harness.go`
- **Local dev DB:** `make db-up` (Postgres 16-alpine on `127.0.0.1:5432`)
- **Run tests:** `make test-migrations` (Docker required)
