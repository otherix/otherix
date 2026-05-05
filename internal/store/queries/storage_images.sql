-- name: CreateStorageImage :one
-- The storage_image.import worker calls this on terminal-success
-- to project the agent's import result into CP. ON CONFLICT
-- (template_id, pool_id) DO UPDATE: a re-import with
-- rotated content overwrites the row's checksum / size / format so
-- the row tracks the latest authoritative state. The handler's
-- atomic-enqueue tx creates the Task row first; the worker's success
-- projection commits this row in the same final tx as
-- UpdateTaskFinalized.
insert into storage_images (
    id,
    template_id,
    pool_id,
    checksum_sha256,
    size_bytes,
    format
)
values (
    @id,
    @template_id,
    @pool_id,
    @checksum_sha256,
    @size_bytes,
    @format
)
on conflict (template_id, pool_id) do update
    set checksum_sha256 = excluded.checksum_sha256,
        size_bytes      = excluded.size_bytes,
        format          = excluded.format
returning *;

-- name: GetStorageImageByID :one
select *
from storage_images
where id = @id;

-- name: GetStorageImageByTemplateAndPool :one
-- Composite-key lookup. Useful for handlers that need to project the
-- (template, pool) → image relation without scanning the pool list.
-- Returns pgx.ErrNoRows when the image has not been imported yet.
select *
from storage_images
where template_id = @template_id
  and pool_id     = @pool_id;

-- name: ListStorageImagesByPool :many
-- Backs the storageImages.list endpoint. Cursor pagination keyed on
-- (imported_at, id) DESC — the tasks-style direction (newest first).
select *
from storage_images
where pool_id = @pool_id
  and (
    sqlc.narg('cursor_imported_at')::timestamptz is null
    or (imported_at, id) < (
      sqlc.narg('cursor_imported_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by imported_at desc, id desc
limit @limit_count;

-- name: DeleteStorageImage :exec
delete from storage_images
where id = @id;

-- name: CountStorageImagesByTemplate :one
-- Precheck before DELETE /v1/templates/{id} (Step 6 extended-delete
-- refusal): if > 0, surface 409 template_in_use.
select count(*)::bigint as image_count
from storage_images
where template_id = @template_id::uuid;

-- name: CountStorageImagesByPool :one
-- Precheck before DELETE /v1/storage-pools/{id} (Step 6 extended-
-- delete refusal): if > 0, surface 409 pool_in_use.
select count(*)::bigint as image_count
from storage_images
where pool_id = @pool_id::uuid;

-- name: CountStorageImagesBySHA256InPool :one
-- Refcount gate for storage_image:delete handler (Step 9 sync
-- delete). Counts other rows in the same pool that share the
-- checksum, excluding the row about to be deleted. Caller invokes
-- agent.deleteImage only when this returns 0; otherwise the
-- on-disk file stays alive for the surviving sibling rows
-- (content-addressable deduplication).
select count(*)::bigint as referent_count
from storage_images
where pool_id         = @pool_id::uuid
  and checksum_sha256 = @checksum_sha256::text
  and id              != @exclude_id::uuid;
