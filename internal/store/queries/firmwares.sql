-- name: CreateFirmware :one
-- Identity-and-flag columns only. The CP API treats firmwares as flat
-- metadata records — per-node availability lives in node_firmwares and
-- is populated by the future heartbeat receiver, not by this query.
insert into firmwares (
    id,
    name,
    architecture,
    type,
    version,
    secure_boot,
    is_default
)
values (
    @id,
    @name,
    @architecture,
    @type,
    sqlc.narg('version')::text,
    @secure_boot,
    @is_default
)
returning *;

-- name: GetFirmwareByID :one
-- firmwares has no soft-delete column; rows are removed via
-- DeleteFirmware (hard delete). FK references in vms (RESTRICT) and
-- templates (SET NULL) keep dependent state consistent at the DB level;
-- the API blocks the operation up-front via DELETE prechecks.
select *
from firmwares
where id = @id;

-- name: ListFirmwares :many
-- Optional ?architecture and ?type filters. Cursor pagination.
select *
from firmwares
where (sqlc.narg('architecture')::cpu_arch    is null or architecture = sqlc.narg('architecture')::cpu_arch)
  and (sqlc.narg('type')::firmware_type        is null or type         = sqlc.narg('type')::firmware_type)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: UpdateFirmware :one
-- Caller merges the patch with the current row before issuing this
-- query (the same pattern as UpdateNetwork / UpdateStoragePool). The
-- `architecture` and `type` columns are intentionally absent from the
-- SET list — they are API-immutable and changing them would invalidate
-- the (architecture, type) default-uniqueness invariant.
update firmwares
set name        = @name,
    version     = sqlc.narg('version')::text,
    secure_boot = @secure_boot,
    is_default  = @is_default
where id = @id
returning *;

-- name: DeleteFirmware :exec
-- Hard delete (firmwares has no soft-delete column). Callers run the
-- vms / templates count prechecks inside the same transaction so a
-- delete that would orphan a template firmware reference (FK is SET
-- NULL) or fail with a 23503 from vms.firmware_id (FK is RESTRICT) is
-- surfaced as a 409 conflict instead.
delete from firmwares
where id = @id;

-- name: CountVMsForFirmware :one
-- Precheck before DELETE /v1/firmwares/{id}: counts active vms that
-- still reference the firmware. Soft-deleted vms are excluded — a
-- removed VM's firmware pin is no longer load-bearing. The FK uses
-- ON DELETE RESTRICT, so this check also avoids the SQL-level error
-- path; the handler surfaces 409 with a typed payload instead.
select count(*)::bigint as vm_count
from vms
where firmware_id = @firmware_id::uuid
  and deleted_at is null;

-- name: CountTemplatesForFirmware :one
-- Precheck before DELETE /v1/firmwares/{id}: counts active templates
-- that pin the firmware. The FK uses ON DELETE SET NULL — at the DB
-- level the delete would succeed and silently null out the templates'
-- firmware_id. The CP-level policy is to require the operator to
-- explicitly clear the pin first, so we block the delete with a 409
-- whenever any active template still references the firmware.
select count(*)::bigint as template_count
from templates
where firmware_id = @firmware_id::uuid
  and deleted_at is null;

-- name: GetDefaultFirmwareForArchAndType :one
-- Used by the Create / Update precheck when is_default=true: returns
-- the existing default firmware for an (architecture, type) pair, or
-- pgx.ErrNoRows when none exists. The unique partial index
-- uq_firmwares_default also enforces the invariant at the DB level,
-- but the precheck gives a nicer 409 path with
-- details.existing_firmware_id and avoids racing with the constraint
-- at handler-write time.
select *
from firmwares
where architecture = @architecture
  and type = @type
  and is_default = true;

-- name: LookupFirmwareByCatalog :one
-- Backs the heartbeat receiver's firmware-projection step. Resolves a
-- (name, architecture, type) triple reported by the agent to a
-- firmware_id. ErrNoRows means the operator has not registered this
-- firmware in the catalogue; the handler then skips the entry with a
-- WARN log instead of auto-registering it.
select id
from firmwares
where name = @name
  and architecture = @architecture
  and type = @type;
