-- name: CreateUser :one
insert into users (id, email, password_hash, display_name, role)
values (
    @id,
    @email,
    @password_hash,
    @display_name,
    @role
)
returning *;

-- name: GetUserByID :one
select *
from users
where id = @id
  and deleted_at is null;

-- name: GetUserByEmail :one
-- Email uniqueness is case-insensitive (uq_users_email on lower(email)).
select *
from users
where lower(email) = lower(@email)
  and deleted_at is null;

-- name: ListUsers :many
-- Cursor pagination: (created_at, id) ordered, nullable cursor
-- params indicate "first page". Caller passes limit_count = clamp(limit, 1, 200).
select *
from users
where deleted_at is null
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: UpdateUser :one
-- Caller merges patch with current values; this query updates the lot.
-- updated_at is bumped by the trg_set_updated_at trigger.
update users
set email         = @email,
    password_hash = @password_hash,
    display_name  = @display_name,
    role          = @role
where id = @id
  and deleted_at is null
returning *;

-- name: TouchUserLastLogin :exec
-- Records a successful login. Separate from UpdateUser because it's a hot
-- path (every login) and shouldn't go through the full row replacement.
update users
set last_login_at = now()
where id = @id
  and deleted_at is null;

-- name: SoftDeleteUser :exec
-- Sets deleted_at = now(); preserves the row for audit / FK integrity.
-- Hard delete is not exposed via the API (owned resources block
-- deletion entirely).
update users
set deleted_at = now()
where id = @id
  and deleted_at is null;

-- name: CountAdmins :one
-- Used by the bootstrap-admin path to skip seeding when an admin
-- already exists. Counts non-deleted rows so a soft-deleted admin
-- does not satisfy the precondition.
select count(*)::bigint as admin_count
from users
where role = 'admin'
  and deleted_at is null;

-- name: CountUserResources :one
-- Returns counts of resources owned by user, for the DELETE precheck
-- (cannot delete a user with owned VMs / templates / snapshots).
-- Counts exclude soft-deleted rows.
-- Aliased to silence sqlc 1.31.1 analyzer ambiguity warning. The three
-- subqueries are independent — column qualification is cosmetic.
select
    (select count(*)::bigint from vms       v where v.owner_id = @user_id and v.deleted_at is null) as vms,
    (select count(*)::bigint from templates t where t.owner_id = @user_id and t.deleted_at is null) as templates,
    (select count(*)::bigint from snapshots s where s.owner_id = @user_id and s.deleted_at is null) as snapshots;
