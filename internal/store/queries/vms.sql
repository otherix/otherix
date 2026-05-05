-- VMs queries — foundations for the CP-side vm.create / vm.delete
-- vertical slice. Schema-aligned: cpu_cores not "vcpus", memory_mib not
-- "memory_mb", no pool_id/node_id columns on vms (pool comes through
-- vm_disks; current node comes through vm_runtime). The handler boundary
-- translates the simplified API wire shape to and from these column
-- names.

-- name: CreateVM :one
-- Inserts a new vms row. Caller supplies the id explicitly (unified
-- UUID, CP mints / agent uses). desired_phase defaults to 'running'
-- so the operator's "create" intent maps к "VM should run" without a
-- separate state transition.
insert into vms (
    id,
    owner_id,
    name,
    description,
    template_id,
    architecture,
    cpu_cores,
    memory_mib,
    cpu_model,
    machine_type,
    firmware_id,
    pinned_node_id,
    user_data,
    cloud_init_disabled,
    labels
)
values (
    @id,
    @owner_id,
    @name,
    @description,
    @template_id,
    @architecture,
    @cpu_cores,
    @memory_mib,
    @cpu_model,
    @machine_type,
    sqlc.narg('firmware_id')::uuid,
    sqlc.narg('pinned_node_id')::uuid,
    sqlc.narg('user_data')::text,
    @cloud_init_disabled,
    @labels
)
returning *;

-- name: GetVMByID :one
select *
from vms
where id = @id
  and deleted_at is null;

-- name: GetVMByName :one
-- Used by the create handler для globally-unique-name precheck before
-- the insert (the unique partial index would catch it as a 23505 too,
-- but a friendly 409 with the existing id is preferable). Also backs
-- ResolveVM in the name-based resolver (internal/api/handlers/internal/
-- resolver) — every GET / DELETE на /v1/vms/{id} folds к this query when
-- the identifier is not a UUID. The lookup is case-insensitive to match
-- uq_vms_name on lower(name); the row preserves the operator's chosen
-- casing.
select *
from vms
where lower(name) = lower(@name)
  and deleted_at is null;

-- name: ListVMs :many
-- Cursor pagination. First page passes nulls; subsequent
-- pages pass the previous page's last (created_at, id) tuple. Ordering
-- (created_at desc, id desc) matches every other paginated endpoint.
--
-- Optional filters:
--
--   - pool_id_filter — only VMs whose vm_disks references the named
--     pool. Resolved upstream from the wire `?pool=` query param,
--     which accepts either a UUID literal or a pool name. Implemented
--     as EXISTS rather than a JOIN to keep the row count stable for
--     multi-disk VMs (the MVP is single-disk, but the query is
--     correct for whatever future disk-fanout decisions land on).
--
--   - node_id_filter — only VMs currently located on the named node
--     per vm_runtime.current_node_id (D6 — current location, not
--     pinned intent). 'creating' VMs without a runtime row never
--     match.
select *
from vms
where deleted_at is null
  and (
    sqlc.narg('pool_id_filter')::uuid is null
    or exists (
      select 1 from vm_disks
      where vm_disks.vm_id = vms.id
        and vm_disks.storage_pool_id = sqlc.narg('pool_id_filter')::uuid
        and vm_disks.deleted_at is null
    )
  )
  and (
    sqlc.narg('node_id_filter')::uuid is null
    or exists (
      select 1 from vm_runtime
      where vm_runtime.vm_id = vms.id
        and vm_runtime.current_node_id = sqlc.narg('node_id_filter')::uuid
    )
  )
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) < (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at desc, id desc
limit @limit_count;

-- name: ListVMsByOwner :many
-- Same shape as ListVMs but filtered by owner_id. RBAC scope=own path —
-- currently inactive (vm:read=any for every role) but kept for future
-- restricted-role flows.
select *
from vms
where owner_id = @owner_id
  and deleted_at is null
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) < (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at desc, id desc
limit @limit_count;

-- name: SoftDeleteVM :exec
-- Soft delete sets deleted_at; the unique partial index on (name) where
-- deleted_at is null releases the name immediately so a fresh create
-- with the same name can succeed. The vm_runtime row (if any) is
-- handled separately by DeleteVMRuntime в the worker's cleanup phase.
update vms
set deleted_at = now()
where id = @id
  and deleted_at is null;

-- name: UpdateVMDesiredPhase :exec
-- Used by the worker to flip desired_phase к 'deleted' as the user-visible
-- delete starts (before the agent acknowledges teardown). Clean read-side
-- semantics: desired_phase reflects user intent; vm_runtime.phase reflects
-- observed.
update vms
set desired_phase = @desired_phase,
    updated_at    = now()
where id = @id
  and deleted_at is null;

-- name: ListVMsForNodeDeclared :many
-- Per-node VM desired-state inventory для HeartbeatResponse.declared_vms.
-- Joins vms × vm_runtime on the runtime's current_node_id (D6 — current
-- location, not pinned intent). Soft-deleted vms are excluded; runtime
-- rows whose phase has reached 'gone' are excluded too (the VM is no
-- longer materialised on this node). Output is sorted lower(name) asc
-- so the agent's diff against observed state stays deterministic across
-- heartbeats.
select
    vms.name::text         as name,
    vms.desired_phase      as desired_phase,
    vms.generation::bigint as generation
from vms
join vm_runtime on vm_runtime.vm_id = vms.id
where vm_runtime.current_node_id = @node_id::uuid
  and vms.deleted_at is null
  and vm_runtime.phase != 'gone'
order by lower(vms.name) asc;

-- name: CountRunningVMsByNode :one
-- Counts VMs intended к run on the given node. Used by the placement
-- scheduler for Least-VM-count tie-breaking. Counted by
-- vms.pinned_node_id (intent) rather than vm_runtime.current_node_id
-- (observed) so a burst of concurrent creates targeting the same
-- least-loaded node spread out immediately; vm_runtime rows arrive
-- only after the agent reports them.
select count(*)::bigint
from vms
where pinned_node_id = @node_id
  and deleted_at is null
  and desired_phase != 'deleted';

