-- name: CreateTask :one
-- Inserts a fresh task row. The handler runs this in the same SQL
-- transaction as the river job enqueue; the river_job_id is stamped
-- onto the row via UpdateTaskRiverJobID after river.InsertTx returns.
-- result / error / progress / started_at / finished_at default to NULL.
insert into tasks (
    id,
    type,
    status,
    resource_type,
    resource_id,
    args,
    max_attempts,
    created_by
)
values (
    @id,
    @type,
    @status,
    @resource_type,
    @resource_id,
    @args,
    @max_attempts,
    @created_by
)
returning *;

-- name: UpdateTaskRiverJobID :exec
-- Stamps the river job id onto the task row inside the atomic enqueue
-- transaction (after river.InsertTx returns). The column carries a
-- weak reference (no FK) used only for ops
-- drill-down.
update tasks
set river_job_id = @river_job_id
where id = @id;

-- name: UpdateTaskAgentTaskID :exec
-- Stamps the agent-side task id on the CP task row. Called by an
-- insert-driven agent-saga worker after the agent's 202 response
-- (storage_image.import is the first consumer). Persisting the id
-- makes a CP-side restart resume polling the existing agent task
-- instead of issuing a duplicate POST (worker resumption seam).
-- storage_pool.scan does NOT populate this column — its task surface
-- stays nil.
update tasks
set agent_task_id = @agent_task_id
where id = @id;

-- name: GetTask :one
select *
from tasks
where id = @id;

-- name: ListTasksAny :many
-- Admin / operator scope: see all tasks. Filters compose with AND;
-- omitted filters pass NULL and are no-ops. Cursor pagination keyed on
-- (created_at, id) — DESC ordering means tuple comparison flows in
-- the < direction.
select *
from tasks
where (sqlc.narg('status_filter')::text is null or status::text = sqlc.narg('status_filter')::text)
  and (sqlc.narg('type_filter')::text is null or type = sqlc.narg('type_filter')::text)
  and (sqlc.narg('resource_type_filter')::text is null or resource_type = sqlc.narg('resource_type_filter')::text)
  and (sqlc.narg('resource_id_filter')::uuid is null or resource_id = sqlc.narg('resource_id_filter')::uuid)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) < (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at desc, id desc
limit @limit_count;

-- name: ListTasksOwn :many
-- Developer / viewer scope: only tasks created by the principal.
-- Same filter / cursor surface as ListTasksAny.
select *
from tasks
where created_by = @created_by
  and (sqlc.narg('status_filter')::text is null or status::text = sqlc.narg('status_filter')::text)
  and (sqlc.narg('type_filter')::text is null or type = sqlc.narg('type_filter')::text)
  and (sqlc.narg('resource_type_filter')::text is null or resource_type = sqlc.narg('resource_type_filter')::text)
  and (sqlc.narg('resource_id_filter')::uuid is null or resource_id = sqlc.narg('resource_id_filter')::uuid)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) < (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at desc, id desc
limit @limit_count;

-- name: UpdateTaskRunning :exec
-- Worker entry: pending → running. Increments the attempt counter and
-- stamps started_at on the first transition (idempotent across retries
-- thanks to COALESCE).
update tasks
set status     = 'running',
    started_at = coalesce(started_at, now()),
    attempts   = attempts + 1
where id = @id;

-- name: UpdateTaskFinalized :exec
-- Worker exit: terminal status (success / failed / cancelled), the
-- result / error JSONB payloads, and finished_at. The caller passes
-- result and error; both nullable JSONB columns and either may be NULL
-- depending on the terminal branch.
update tasks
set status      = @status,
    result      = @result,
    error       = @error,
    finished_at = now()
where id = @id;

-- name: CancelTaskIfPending :one
-- Cancel a task only while it is still pending. Returns the updated
-- row when status was pending; returns pgx.ErrNoRows for any other
-- state — the caller maps that to a 409 (running cancel is deferred
-- к future work; terminal cancel is idempotent-409).
update tasks
set status      = 'cancelled',
    finished_at = now()
where id = @id
  and status = 'pending'
returning *;

-- name: DeleteExpiredTasks :execrows
-- State-aware retention. The two cutoffs are
-- independent: success rows past @completed_cutoff and failed /
-- cancelled rows past @failed_cutoff are removed in one DELETE. Only
-- finalized rows (finished_at IS NOT NULL) are eligible — the partial
-- index idx_tasks_finished_at supports the scan.
delete from tasks
where finished_at is not null
  and (
    (status = 'success'                  and finished_at < @completed_cutoff::timestamptz)
    or
    (status in ('failed', 'cancelled')   and finished_at < @failed_cutoff::timestamptz)
  );
