-- name: CreateStoragePool :one
-- Identity-and-config columns only. capacity_bytes / available_bytes /
-- reported_at are agent-reported and never accepted on
-- the API edge — they will be populated through the future heartbeat
-- and scan paths, both of which use UpsertStoragePoolUsage (deferred,
-- not implemented yet).
insert into storage_pools (
    id,
    node_id,
    name,
    type,
    path,
    config
)
values (
    @id,
    @node_id,
    @name,
    @type,
    @path,
    @config
)
returning *;

-- name: GetStoragePoolByID :one
select *
from storage_pools
where id = @id
  and deleted_at is null;

-- name: ListStoragePoolsByName :many
-- Returns every instance of a pool name across the cluster. The
-- multi-instance model permits the same name on multiple nodes
-- simultaneously; bare-name resolution is therefore an aggregation, not a
-- single-row lookup. Case-folded match honours uq_storage_pools_name's
-- functional component.
select *
from storage_pools
where lower(name) = lower(@name)
  and deleted_at is null
order by node_id;

-- name: ListEligiblePoolsByName :many
-- Joined query used by the scheduler at VM placement time: enumerates
-- pool instances for the given name on nodes that are ready and not
-- cordoned. Returns the pool's effective-capacity row plus the node's
-- effective-availability row in a single round-trip.
--
-- Sources pool columns from pool_effective_capacity so the scheduler
-- reads `available_bytes_effective` — pool availability с pending-VM-
-- disk accounting. Node columns come от node_effective_availability so
-- `cpu_cores_effective` / `memory_effective_mib` are available with
-- pending-VM accounting too. Both views are supersets of their
-- underlying tables, so the eligibility filters
-- (status = 'ready', cordoned_at IS NULL, deleted_at IS NULL) hold
-- without change. Disk-aware Sub-iteration C consumes both effective
-- columns in `filterByResources` / `scoreResourceAware`.
select sqlc.embed(p), sqlc.embed(n)
from pool_effective_capacity p
join node_effective_availability n on n.id = p.node_id
where lower(p.name) = lower(@name)
  and p.deleted_at is null
  and n.deleted_at is null
  and n.status = 'ready'
  and n.cordoned_at is null
  and n.memory_pressure_since is null
  and n.system_disk_pressure_since is null
  and p.disk_pressure_since is null
order by n.id;

-- name: ListMemoryPressuredCandidatesByName :many
-- Diagnostic query consumed by the scheduler on the failure path
-- (`len(eligible) == 0` for а pool that does exist somewhere). Returns
-- pool instances for the given name whose owning node would have been
-- eligible except for an active **memory** pressure condition.
-- Same node-status / cordoned / deleted_at filters as
-- ListEligiblePoolsByName, но the memory pressure filter is inverted
-- (memory_pressure_since IS NOT NULL).
select sqlc.embed(p), sqlc.embed(n)
from pool_effective_capacity p
join node_effective_availability n on n.id = p.node_id
where lower(p.name) = lower(@name)
  and p.deleted_at is null
  and n.deleted_at is null
  and n.status = 'ready'
  and n.cordoned_at is null
  and n.memory_pressure_since is not null
order by n.id;

-- name: ListSystemDiskPressuredCandidatesByName :many
-- Diagnostic-only sibling of ListMemoryPressuredCandidatesByName for
-- the system-disk pressure condition.
-- Returns pool instances on nodes flagged с system_disk_pressure_since
-- IS NOT NULL. Same eligibility predicates as the memory variant —
-- only the inverted pressure filter differs.
select sqlc.embed(p), sqlc.embed(n)
from pool_effective_capacity p
join node_effective_availability n on n.id = p.node_id
where lower(p.name) = lower(@name)
  and p.deleted_at is null
  and n.deleted_at is null
  and n.status = 'ready'
  and n.cordoned_at is null
  and n.system_disk_pressure_since is not null
order by n.id;

-- name: ListDiskPressuredPoolsByName :many
-- Diagnostic-only sibling for pool disk pressure. Returns pool
-- instances для the given name whose own disk_pressure flag is set —
-- independent of the owning node's pressure status. Pool-scoped
-- pressure is the only condition that excludes а pool independently
-- of its node.
select sqlc.embed(p), sqlc.embed(n)
from pool_effective_capacity p
join node_effective_availability n on n.id = p.node_id
where lower(p.name) = lower(@name)
  and p.deleted_at is null
  and n.deleted_at is null
  and n.status = 'ready'
  and n.cordoned_at is null
  and p.disk_pressure_since is not null
order by n.id;

-- name: ListStoragePools :many
-- Optional ?node_id and ?type filters. Cursor pagination.
-- Soft-deleted rows are excluded.
select *
from storage_pools
where deleted_at is null
  and (sqlc.narg('node_id')::uuid is null or node_id = sqlc.narg('node_id')::uuid)
  and (sqlc.narg('type')::text is null or type = sqlc.narg('type')::text)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: UpdateStoragePool :one
-- Caller merges the patch with the current row before issuing this
-- query (the same pattern as UpdateNetwork). The `node_id`, `type` and
-- `path` columns are intentionally absent from the SET list — they are
-- API-immutable. capacity_bytes / available_bytes / reported_at are
-- owned by the heartbeat / scan path and are never touched by this
-- handler-driven update.
update storage_pools
set name   = @name,
    config = @config
where id = @id
  and deleted_at is null
returning *;

-- name: SoftDeleteStoragePool :exec
update storage_pools
set deleted_at = now()
where id = @id
  and deleted_at is null;

-- name: CountVMDisksOnStoragePool :one
-- Precheck before DELETE /v1/storage-pools/{id}: counts active vm_disks
-- rows that still reference the pool. Soft-deleted disks are excluded
-- — a detached disk has no live attachment and does not block pool
-- deletion. The FK uses ON DELETE RESTRICT, so this check also avoids
-- the SQL-level error path; we surface 409 with a typed payload
-- instead.
select count(*)::bigint as disk_count
from vm_disks
where storage_pool_id = @storage_pool_id::uuid
  and deleted_at is null;

-- name: GetPoolEffectiveByID :one
-- View-backed read returning every storage_pools column plus the
-- `available_bytes_effective` synthesized by pool_effective_capacity.
-- Consumed by the GET-by-id handler so operator-facing responses
-- surface both raw и effective bytes.
select * from pool_effective_capacity
where id = @id
  and deleted_at is null;

-- name: ListPoolsEffective :many
-- View-backed parallel к ListStoragePools — same optional ?node_id /
-- ?type filters и (created_at, id) cursor pagination,
-- но every row carries `available_bytes_effective`. Soft-deleted rows
-- excluded by construction (view itself emits no rows when raw is
-- soft-deleted — see the WHERE clause on the view).
select * from pool_effective_capacity
where deleted_at is null
  and (sqlc.narg('node_id')::uuid is null or node_id = sqlc.narg('node_id')::uuid)
  and (sqlc.narg('type')::text is null or type = sqlc.narg('type')::text)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: ListPoolsEffectiveByName :many
-- View-backed parallel к ListStoragePoolsByName. Drives the
-- name-aggregated `GET /v1/storage-pools/{name}` projection so each
-- per-node instance carries effective bytes.
select * from pool_effective_capacity
where lower(name) = lower(@name)
  and deleted_at is null
order by node_id;

-- name: ListPoolsNeedingScan :many
-- Returns active storage_pools whose owning node is in a scannable
-- status, excluding pools that already have an in-flight scan task
-- (a `tasks` row of type `storage_pool.scan` in status pending or
-- running referencing the pool). Drives the `storage_pool.scan_trigger`
-- periodic worker: each tick fans out fresh scan tasks only к pools
-- that need а refresh, leaving in-flight scans untouched. Ordering on
-- `p.id` is stable across ticks so log lines correlate predictably.
select p.id, p.name, p.node_id
from storage_pools p
join nodes n on n.id = p.node_id
where p.deleted_at is null
  and n.deleted_at is null
  and n.status in ('pending', 'ready', 'cordoned')
  and not exists (
    select 1
    from tasks t
    where t.type = 'storage_pool.scan'
      and t.status in ('pending', 'running')
      and t.resource_id = p.id
  )
order by p.id;

-- name: UpsertStoragePoolUsage :exec
-- Scan-result projection. Agent-reported capacity /
-- availability merge onto the row; handler-driven CRUD никогда не
-- writes these columns. The scan worker pairs this с
-- GetPoolDiskPressureState + UpdatePoolDiskPressure inside the same
-- transaction so pool disk pressure stays
-- consistent с the metrics that determined it.
update storage_pools
set capacity_bytes  = @capacity_bytes,
    available_bytes = @available_bytes,
    reported_at     = now()
where id = @id
  and deleted_at is null;

-- name: GetPoolDiskPressureState :one
-- Pre-read for the scan worker's pool disk pressure transition.
-- Mirrors GetNodeForHeartbeat's pressure-state read on the node side.
-- Excludes soft-deleted rows — а scan that lands после the pool is
-- soft-deleted is а no-op overall.
select disk_pressure_since, disk_pressure_count
from storage_pools
where id = @id
  and deleted_at is null;

-- name: UpdatePoolDiskPressure :exec
-- Persists the pool disk pressure transition.
-- Caller runs this in the same transaction as UpsertStoragePoolUsage so
-- pressure state never drifts ahead of the scan that determined it.
update storage_pools
set
    disk_pressure_since = sqlc.narg('disk_pressure_since')::timestamptz,
    disk_pressure_count = @disk_pressure_count::int
where id = @id
  and deleted_at is null;

-- name: ListStoragePoolsByNode :many
-- Returns every active pool row owned by the given node, ordered for
-- stable heartbeat responses (operator-facing field order should not
-- jitter between heartbeats). Drives the heartbeat handler's
-- `declared_pools` response field — the agent reconciler
-- consumes this list as the desired-state cache.
select *
from storage_pools
where node_id = @node_id
  and deleted_at is null
order by lower(name);

-- name: UpdateStoragePoolReconciliation :exec
-- Applies the agent's reconciliation report for a single pool. Called
-- by the heartbeat handler once per reported pool per heartbeat. The
-- match key is (node_id, lower(name)) — а pool name maps к multiple
-- rows cluster-wide, but only one per node. Soft-deleted rows are
-- skipped silently (no rows affected): operator deleted the pool
-- mid-tick; the agent will drop it от observed state on the next
-- reconciliation cycle. error is set к the empty string когда the
-- agent reports success, NULL когда the status itself is not 'failed'.
update storage_pools
set
    reconciliation_status = @reconciliation_status::text,
    reconciliation_error  = sqlc.narg('reconciliation_error')::text,
    last_reconciled_at    = now()
where node_id = @node_id
  and lower(name) = lower(@name)
  and deleted_at is null;
