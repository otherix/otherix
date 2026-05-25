-- name: CreateTemplate :one
-- Templates are always created with visibility='private' (variant C of
-- the create-permission model — see CLAUDE.md "Authorization (RBAC)"
-- and ROADMAP). The literal is hard-coded in this INSERT rather than
-- accepted as a parameter so a future call site cannot opt out by
-- mistake; visibility moves only through SetTemplateVisibility, which
-- carries the dedicated `template:set_visibility` permission. Name
-- uniqueness is enforced by uq_templates_name (global, not per-owner)
-- and surfaces as 23505 → 409 in the handler with a neutral
-- "template with this name already exists" message.
insert into templates (
    id,
    owner_id,
    name,
    description,
    visibility,
    architecture,
    os_family,
    os_variant,
    image_url,
    image_checksum_sha256,
    image_format,
    image_size_bytes,
    firmware_type,
    firmware_id,
    default_cpu_cores,
    default_memory_mib,
    default_disk_gib,
    cloud_init_supported,
    cloud_init_user_data,
    qemu_guest_agent_expected,
    metadata
)
values (
    @id,
    @owner_id,
    @name,
    @description,
    'private',
    @architecture,
    @os_family,
    @os_variant,
    @image_url,
    @image_checksum_sha256,
    @image_format,
    sqlc.narg('image_size_bytes')::bigint,
    @firmware_type,
    sqlc.narg('firmware_id')::uuid,
    @default_cpu_cores,
    @default_memory_mib,
    @default_disk_gib,
    @cloud_init_supported,
    sqlc.narg('cloud_init_user_data')::text,
    @qemu_guest_agent_expected,
    @metadata
)
returning *;

-- name: GetTemplate :one
-- Soft-deleted rows are invisible to every read path; a Get that
-- targets a deleted template returns pgx.ErrNoRows and the handler
-- surfaces 404. The partial index uq_templates_name (where deleted_at
-- is null) means a freed name is reusable immediately after delete.
select *
from templates
where id = @id
  and deleted_at is null;

-- name: GetTemplateByName :one
-- Backs ResolveTemplate in the name-based resolver. Case-insensitive
-- on lower(name) to match the uq_templates_name functional
-- index — every /v1/templates/{id} read and every POST body that
-- references a template by name routes through here when the
-- identifier is not a UUID. Visibility is NOT filtered at the SQL
-- layer: callers run their own RBAC / visibility checks after the
-- resolve (e.g. checkTemplateUseAccess) so the handler can return 404
-- not_found for both "missing" and "invisible to you" cases without
-- the resolver leaking the distinction.
select *
from templates
where lower(name) = lower(@name)
  and deleted_at is null;

-- name: ListTemplates :many
-- Visibility-aware listing for templates. The clamp is encoded as two
-- parameters:
--
--   - caller_can_see_any (bool) — true for admin/operator. When true,
--     the visibility / ownership clause short-circuits and every active
--     template (subject to the optional query filters) is returned.
--   - caller_id (uuid, nullable) — for developer it is the caller's id
--     (own private + public are visible); for viewer it is NULL (only
--     public are visible).
--
-- Optional query filters (?owner_id, ?visibility, ?architecture,
-- ?os_family) compose with the clamp via AND. A non-admin caller that
-- supplies ?owner_id pointing at another user therefore sees only that
-- user's public templates — their private set never leaks. Cursor
-- pagination.
--
-- The WHERE-shape is intentionally aligned with idx_templates_visibility
-- (visibility, owner_id) — visibility is the leading filter on
-- non-admin callers, which lets the index do the heavy lifting.
select *
from templates
where deleted_at is null
  and (
       @caller_can_see_any::boolean
    or visibility = 'public'
    or (sqlc.narg('caller_id')::uuid is not null and owner_id = sqlc.narg('caller_id')::uuid)
  )
  and (sqlc.narg('owner_id_filter')::uuid is null    or owner_id     = sqlc.narg('owner_id_filter')::uuid)
  and (sqlc.narg('visibility_filter')::text is null  or visibility   = sqlc.narg('visibility_filter')::text)
  and (sqlc.narg('architecture_filter')::cpu_arch is null or architecture = sqlc.narg('architecture_filter')::cpu_arch)
  and (sqlc.narg('os_family_filter')::os_family is null   or os_family    = sqlc.narg('os_family_filter')::os_family)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: UpdateTemplate :one
-- Caller merges the patch with the current row before issuing this
-- query (same pattern as UpdateNetwork / UpdateFirmware). The columns
-- below are exactly the 11 mutable fields surfaced by TemplateUpdate
-- in api/openapi/control-plane.yaml. Identity / image-source fields
-- (architecture, os_family, owner_id, image_url, image_checksum_sha256,
-- image_format, image_size_bytes) are intentionally absent — they are
-- API-immutable. visibility is also absent: it moves only through
-- SetTemplateVisibility.
update templates
set name                      = @name,
    description               = @description,
    os_variant                = @os_variant,
    firmware_type             = @firmware_type,
    firmware_id               = sqlc.narg('firmware_id')::uuid,
    default_cpu_cores         = @default_cpu_cores,
    default_memory_mib        = @default_memory_mib,
    default_disk_gib          = @default_disk_gib,
    cloud_init_supported      = @cloud_init_supported,
    cloud_init_user_data      = sqlc.narg('cloud_init_user_data')::text,
    qemu_guest_agent_expected = @qemu_guest_agent_expected,
    metadata                  = @metadata
where id = @id
  and deleted_at is null
returning *;

-- name: UpdateTemplateImageChecksumIfNull :exec
-- Back-propagates the agent-computed image_checksum_sha256 onto a
-- template row that was registered without one (compute mode).
-- The `image_checksum_sha256 is null` guard in the WHERE clause makes
-- this idempotent and race-safe:
--
--   - First successful compute-mode import for the template lands the
--     hash; subsequent imports (to other pools, OR re-imports to the same
--     pool) become no-ops because the column is no longer NULL.
--   - Two concurrent compute-mode imports to different pools that happen
--     to compute different bytes (corrupted download) cannot both win:
--     whichever transaction commits first wins; the second's UPDATE
--     affects 0 rows and is silently absorbed.
--
-- The :exec return shape is acceptable because the worker treats a
-- failed update as a warning, not a terminal-fail signal — the
-- storage_images row is already authoritative for the (template, pool)
-- combination.
update templates
set image_checksum_sha256 = @image_checksum_sha256
where id = @id
  and image_checksum_sha256 is null
  and deleted_at is null;

-- name: SetTemplateVisibility :one
-- Updates only `visibility` (and `updated_at` via the existing trigger).
-- The handler runs a SELECT-then-conditional-UPDATE: when the requested
-- visibility equals the current visibility the UPDATE is skipped
-- entirely, so updated_at remains a meaningful change marker. Owner
-- never moves — publishing a private template does NOT transfer
-- ownership (see docs/rbac.md).
update templates
set visibility = @visibility::text
where id = @id
  and deleted_at is null
returning *;

-- name: SoftDeleteTemplate :exec
-- Soft-delete only. Three FKs reference templates:
--
--   - vms.template_id           ON DELETE SET NULL
--   - vm_disks.source_template_id  ON DELETE RESTRICT
--   - node_image_cache.template_id ON DELETE CASCADE
--
-- Soft-delete bypasses all three (the row physically remains), so the
-- API-level CountActiveVMsForTemplate precheck is the SOLE enforcement
-- of "don't remove a template that's still in use". The partial unique
-- index uq_templates_name on (name) where deleted_at is null frees the
-- name for re-use the moment delete completes.
update templates
set deleted_at = now()
where id = @id
  and deleted_at is null;

-- name: CloneTemplate :one
-- INSERT-SELECT clone. The new row is owned by the caller, named by
-- the caller, and starts in visibility='private' (Variant C — same
-- invariant as CreateTemplate). Description falls through to the
-- source's description when the caller omits it. Image-source bundle
-- (url + checksum + format + size), sizing, capabilities, metadata,
-- firmware pin, os_variant, and architecture / os_family are copied
-- verbatim — clone is a metadata copy and does not re-fetch the image.
--
-- Returns 0 rows when source is missing or soft-deleted (handler maps
-- to 404). uq_templates_name surfaces as 23505 → 409 with a neutral
-- "template with this name already exists" message.
insert into templates (
    id,
    owner_id,
    name,
    description,
    visibility,
    architecture,
    os_family,
    os_variant,
    image_url,
    image_checksum_sha256,
    image_format,
    image_size_bytes,
    firmware_type,
    firmware_id,
    default_cpu_cores,
    default_memory_mib,
    default_disk_gib,
    cloud_init_supported,
    cloud_init_user_data,
    qemu_guest_agent_expected,
    metadata
)
select
    @new_id::uuid,
    @new_owner_id::uuid,
    @new_name::text,
    coalesce(sqlc.narg('new_description')::text, description),
    'private',
    architecture,
    os_family,
    os_variant,
    image_url,
    image_checksum_sha256,
    image_format,
    image_size_bytes,
    firmware_type,
    firmware_id,
    default_cpu_cores,
    default_memory_mib,
    default_disk_gib,
    cloud_init_supported,
    cloud_init_user_data,
    qemu_guest_agent_expected,
    metadata
from templates
where id = @source_id::uuid
  and deleted_at is null
returning *;

-- name: CountActiveVMsForTemplate :one
-- Precheck before DELETE /v1/templates/{id}: counts active vms that
-- still reference the template. Soft-deleted vms are excluded — a
-- removed VM has no live consumption of the template's image rec.
--
-- The vms.template_id FK is ON DELETE SET NULL by design — orphaned
-- VMs continue to run from the cached image blob, since the template
-- was only the recipe used to provision the disk, not a runtime
-- dependency. Hence the API-level policy: deleting a template should
-- be an explicit operator decision, not a silent reference-nulling.
-- This counter is the sole enforcement of that policy (soft-delete
-- bypasses the FK action; vm_disks.source_template_id RESTRICT and
-- node_image_cache CASCADE only fire on a hard delete that this
-- handler never issues).
select count(*)::bigint as vm_count
from vms
where template_id = @template_id::uuid
  and deleted_at is null;

-- name: IncrementTemplateDerivedVMCount :exec
-- Bumps templates.derived_vm_count by one. Called from VMCreateWorker's
-- success projection inside the same InTx as UpdateTaskFinalized. The
-- column carries a check (derived_vm_count >= 0) at the schema level;
-- the increment cannot underflow.
update templates
set derived_vm_count = derived_vm_count + 1
where id = @id
  and deleted_at is null;

-- name: DecrementTemplateDerivedVMCount :exec
-- Drops templates.derived_vm_count by one. Called from VMDeleteWorker's
-- success projection inside the same InTx as the soft-delete chain.
-- The decrement is a guarded UPDATE — `where deleted_at is null and
-- derived_vm_count > 0` — so a stale call against a soft-deleted
-- template OR a counter that has already drifted to zero is a silent
-- no-op rather than a 23514 check-constraint violation.
update templates
set derived_vm_count = derived_vm_count - 1
where id = @id
  and deleted_at is null
  and derived_vm_count > 0;
