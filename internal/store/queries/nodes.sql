-- name: CreateNode :one
-- Used at agent join time. Capabilities and labels start empty; they are
-- filled by subsequent UpdateNodeHeartbeat calls once the agent connects.
insert into nodes (
    id,
    name,
    architecture,
    advertised_endpoint,
    migration_host,
    migration_port_range_start,
    migration_port_range_end,
    status
)
values (
    @id,
    @name,
    @architecture,
    @advertised_endpoint,
    @migration_host,
    @migration_port_range_start,
    @migration_port_range_end,
    @status
)
returning *;

-- name: GetNodeByID :one
select *
from nodes
where id = @id
  and deleted_at is null;

-- name: GetNodeEffectiveByID :one
-- Reads from the node_effective_availability view (00001_init.sql) so
-- callers see effective-availability columns alongside the raw
-- heartbeat columns. Used by the GET /v1/nodes/{name} handler to
-- render the full operator-facing projection. Soft-
-- deleted rows are filtered same as GetNodeByID.
select *
from node_effective_availability
where id = @id
  and deleted_at is null;

-- name: GetNodeByName :one
-- Case-insensitive on lower(name) to match the uq_nodes_name functional
-- index. Backs ResolveNode in the name-based resolver: every
-- /v1/nodes/{id} read and every node referenced by name (cordon /
-- uncordon, storage pool ?node= filter, etc.) routes through here when
-- the identifier is not a UUID.
select *
from nodes
where lower(name) = lower(@name)
  and deleted_at is null;

-- name: ListNodes :many
-- Optional architecture / status filters. Cursor pagination.
select *
from nodes
where deleted_at is null
  and (sqlc.narg('architecture')::cpu_arch   is null or architecture = sqlc.narg('architecture')::cpu_arch)
  and (sqlc.narg('status')::node_status      is null or status       = sqlc.narg('status')::node_status)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: ListNodesEffective :many
-- View-backed variant of ListNodes used by GET /v1/nodes. Returns the
-- same row shape as ListNodes plus cpu_cores_effective /
-- memory_effective_mib so the operator can compare raw heartbeat
-- availability vs the scheduler's view in а single response.
-- Filters / pagination semantics identical к ListNodes.
select *
from node_effective_availability
where deleted_at is null
  and (sqlc.narg('architecture')::cpu_arch   is null or architecture = sqlc.narg('architecture')::cpu_arch)
  and (sqlc.narg('status')::node_status      is null or status       = sqlc.narg('status')::node_status)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: UpdateNodeHeartbeat :exec
-- Called from the agent heartbeat path. Migration capability is included
-- because an agent can reconfigure its migration network without rejoining.
-- `architecture` is intentionally NOT mutated here — it is immutable
-- after registration, and the handler rejects mismatches with 409
-- before reaching this update. `labels` is also untouched: heartbeat
-- does not carry labels (operator-managed). `status` stays unchanged —
-- that is the reconciler's job.
--
-- system_disk_{total,available}_bytes write
-- raw root-filesystem capacity reported by the agent. Pressure detection
-- happens via UpdateNodeSystemDiskPressure inside the same transaction.
update nodes
set
    last_heartbeat_at           = now(),
    agent_version               = @agent_version,
    migration_host              = @migration_host,
    migration_port_range_start  = @migration_port_range_start,
    migration_port_range_end    = @migration_port_range_end,
    cpu_cores_total             = @cpu_cores_total,
    cpu_cores_available         = @cpu_cores_available,
    cpu_model                   = @cpu_model,
    cpu_flags                   = @cpu_flags,
    memory_total_mib            = @memory_total_mib,
    memory_available_mib        = @memory_available_mib,
    hugepages_2mib_total        = @hugepages_2mib_total,
    hugepages_1gib_total        = @hugepages_1gib_total,
    kernel_version              = @kernel_version,
    qemu_version                = @qemu_version,
    numa_topology               = @numa_topology,
    capabilities                = @capabilities,
    system_disk_total_bytes     = @system_disk_total_bytes,
    system_disk_available_bytes = @system_disk_available_bytes
where id = @id
  and deleted_at is null;

-- name: GetNodeForHeartbeat :one
-- Pre-flight read for the heartbeat receiver: fetches the immutable
-- and policy-relevant fields needed for the architecture / status
-- guards plus the current pressure state used by the transition logic.
-- Both memory and system_disk pressure live на the row; the receiver
-- reads both inside the same transaction as the metric writes.
-- Excludes soft-deleted rows (deleted_at IS NOT NULL → 404, as opposed
-- to status='gone' which surfaces as 409 node_gone).
select
    id,
    architecture,
    status,
    memory_pressure_since,
    memory_pressure_count,
    system_disk_pressure_since,
    system_disk_pressure_count
from nodes
where id = @id
  and deleted_at is null;

-- name: UpdateNodeMemoryPressure :exec
-- Persists the memory-pressure transition computed by the heartbeat
-- receiver. Memory_pressure_since is NULL
-- when the condition is not active; the count increments on every
-- below-threshold observation и resets к 0 on an at-or-above-threshold
-- observation. Caller runs this inside the same transaction as
-- UpdateNodeHeartbeat so the pressure state never drifts ahead of the
-- raw metrics that determined it.
update nodes
set
    memory_pressure_since = sqlc.narg('memory_pressure_since')::timestamptz,
    memory_pressure_count = @memory_pressure_count::int
where id = @id
  and deleted_at is null;

-- name: UpdateNodeSystemDiskPressure :exec
-- Persists the system-disk pressure transition.
-- Mirror of UpdateNodeMemoryPressure: NULL pressure_since когда condition
-- is not active; count increments on each below-threshold observation,
-- resets к 0 on the first at-or-above observation. Runs inside the same
-- transaction as UpdateNodeHeartbeat.
update nodes
set
    system_disk_pressure_since = sqlc.narg('system_disk_pressure_since')::timestamptz,
    system_disk_pressure_count = @system_disk_pressure_count::int
where id = @id
  and deleted_at is null;

-- name: UpdateNodeStatus :exec
-- Reconciler-driven status transitions. cordoned_at is bumped to now()
-- when the new status is 'cordoned' and was previously something else;
-- callers pass cordoned_at = NULL to clear it.
update nodes
set status      = @status,
    cordoned_at = sqlc.narg('cordoned_at')::timestamptz
where id = @id
  and deleted_at is null;

-- name: SoftDeleteNode :exec
update nodes
set deleted_at = now()
where id = @id
  and deleted_at is null;

-- name: CordonNode :one
-- Sets status to 'cordoned' and stamps cordoned_at. Caller is expected
-- to gate on valid transitions (idempotent no-op for already-cordoned,
-- 409 for gone/draining) before issuing this query.
update nodes
set status      = 'cordoned',
    cordoned_at = now()
where id = @id
  and deleted_at is null
returning *;

-- name: UncordonNode :one
-- Returns the node to 'ready' and clears cordoned_at. Caller is expected
-- to gate on valid transitions (idempotent no-op for already-ready, 409
-- for pending/unreachable/draining/gone) before issuing this query.
update nodes
set status      = 'ready',
    cordoned_at = null
where id = @id
  and deleted_at is null
returning *;

-- name: CountVMRuntimeOnNode :one
-- VM runtime rows currently bound to the node via vm_runtime.current_node_id.
-- Used as a precheck before DELETE /v1/nodes/{id} (without ?force=true).
select count(*)::bigint as runtime_count
from vm_runtime
where current_node_id = @node_id::uuid;

-- name: CountActiveMigrationsOnNode :one
-- Active migrations involving the node as either source or target.
-- "Active" matches the partial-index predicate on migrations:
-- phase not in ('completed', 'failed', 'cancelled').
select count(*)::bigint as active_count
from migrations
where phase not in ('completed', 'failed', 'cancelled')
  and (source_node_id = @node_id::uuid or target_node_id = @node_id::uuid);

-- name: OrphanVMRuntimeOnNode :execrows
-- Force-delete step. Marks every vm_runtime row currently on the node
-- as 'orphaned' and clears current_node_id. vms.desired_phase is
-- intentionally NOT touched (migration 00003): the user still wants
-- whatever they wanted before; only the observed runtime is affected.
update vm_runtime
set phase           = 'orphaned',
    current_node_id = null
where current_node_id = @node_id::uuid;

-- name: CancelActiveMigrationsOnNode :execrows
-- Force-delete step. Cancels every active migration with the node as
-- source or target. error_message names the cause for audit clarity.
update migrations
set phase         = 'cancelled',
    error_message = @reason::text,
    completed_at  = now()
where phase not in ('completed', 'failed', 'cancelled')
  and (source_node_id = @node_id::uuid or target_node_id = @node_id::uuid);

-- name: ListStaleNodes :many
-- Nodes whose last heartbeat is older than the supplied cutoff, or which
-- never heartbeated. Used by the reconciler to flip nodes to 'unreachable'.
-- Excludes terminal status 'gone' (post-mortem) and 'unreachable' (already
-- flagged). Pending/ready/cordoned/draining nodes ARE candidates for stale
-- detection. If node_status gains a new terminal status, update the NOT IN
-- list below.
select *
from nodes
where deleted_at is null
  and status not in ('gone', 'unreachable')
  and (last_heartbeat_at is null or last_heartbeat_at < @stale_before::timestamptz)
order by last_heartbeat_at nulls first, id;

-- name: MarkNodesUnreachable :many
-- Reconciler step that demotes stale 'ready' or 'pending' rows к
-- 'unreachable'. 'cordoned' and 'draining' are operator-pinned states
-- and stay put even when heartbeats lapse — the operator deliberately
-- isolated the node, so liveness is not the signal that flips it.
-- 'gone' and 'unreachable' are excluded as terminal / already-flagged.
-- Returns the affected rows so callers can log id + name + the stale
-- timestamp without an extra round-trip.
update nodes
set status = 'unreachable'
where deleted_at is null
  and status in ('ready', 'pending')
  and (last_heartbeat_at is null or last_heartbeat_at < @stale_before::timestamptz)
returning id, name, last_heartbeat_at;

-- name: PromoteHealthyNodes :many
-- Reconciler step that promotes rows к 'ready' once a fresh heartbeat
-- lands. Two source states qualify: 'pending' (first heartbeat after
-- registration) and 'unreachable' (recovery after a network blip).
-- 'cordoned' and 'draining' stay put — only the operator clears those.
-- Returns the affected rows so callers can log the transition.
update nodes
set status = 'ready'
where deleted_at is null
  and status in ('pending', 'unreachable')
  and last_heartbeat_at is not null
  and last_heartbeat_at >= @fresh_after::timestamptz
returning id, name, status;
