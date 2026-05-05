-- name: FilterExistingVMIDs :many
-- Pre-filter for the heartbeat receiver's VM-projection step. Returns
-- only the VM ids that exist in `vms` and are not soft-deleted, so
-- the handler can skip unknown vm_uuids with a WARN instead of taking
-- an FK violation on UpsertVMRuntime. One round-trip vs. N — the
-- heartbeat carries up to a node's worth of VMs.
select id
from vms
where id = any(@ids::uuid[])
  and deleted_at is null;

-- name: GetVMRuntime :one
-- Read the runtime snapshot for a VM, used by GET /v1/vms/{id} to
-- project a combined desired+observed status. Returns ErrNoRows when
-- the worker has not yet upserted (i.e. the VM is still в
-- desired_phase='running' but the agent hasn't acknowledged).
select *
from vm_runtime
where vm_id = @vm_id;

-- name: DeleteVMRuntime :exec
-- Removes the runtime snapshot. Called by the worker's vm.delete
-- cleanup phase after the agent confirms teardown.
delete from vm_runtime
where vm_id = @vm_id;

-- name: UpdateVMRuntimePhase :exec
-- Single-field phase patch used by the synchronous VM lifecycle
-- handlers (pause / resume / reset). The CP-side handler invokes
-- this after the agent confirms а QMP-driven transition so the
-- next projected `status` reflects the new state without waiting
-- for the heartbeat reconciler to catch up. last_observed_at is
-- stamped to keep the wire's observed-state freshness signal
-- monotonic.
update vm_runtime
set phase = @phase,
    last_observed_at = now()
where vm_id = @vm_id;

-- name: UpsertVMRuntime :exec
-- Heartbeat-driven projection of a per-VM runtime snapshot. Conflict
-- on the primary key (vm_id) resolves to UPDATE; the heartbeat is
-- the only writer of the agent-reported fields. observed_generation
-- is bumped when the agent reports a value; missing values pass
-- through nullable parameters. last_observed_at is stamped on every
-- upsert (separate from the trigger-managed updated_at, which fires
-- on UPDATE only).
insert into vm_runtime (
    vm_id,
    current_node_id,
    phase,
    observed_generation,
    qemu_pid,
    last_started_at,
    last_error_message,
    last_observed_at
)
values (
    @vm_id,
    @current_node_id,
    @phase,
    @observed_generation,
    sqlc.narg('qemu_pid')::int,
    sqlc.narg('last_started_at')::timestamptz,
    sqlc.narg('last_error_message')::text,
    now()
)
on conflict (vm_id) do update
set current_node_id     = excluded.current_node_id,
    phase               = excluded.phase,
    observed_generation = excluded.observed_generation,
    qemu_pid            = excluded.qemu_pid,
    last_started_at     = excluded.last_started_at,
    last_error_message  = excluded.last_error_message,
    last_observed_at    = excluded.last_observed_at;
