-- name: CreateApiToken :one
insert into api_tokens (
    id, user_id, name,
    token_hash, prefix,
    expires_at
)
values (
    @id, @user_id, @name,
    @token_hash, @prefix,
    @expires_at
)
returning *;

-- name: GetApiTokenByHash :one
-- Used by the authn middleware on every API-token request. Excludes
-- revoked and expired tokens at the SQL layer — a hit means the token
-- is presently usable. The caller still type-checks pgx.ErrNoRows and
-- maps it to ErrInvalidToken.
select *
from api_tokens
where token_hash = @token_hash
  and revoked_at is null
  and (expires_at is null or expires_at > now());

-- name: GetApiTokenByID :one
-- Includes revoked and expired rows; the caller decides what to do.
-- Used by the self-service DELETE handler to scope by id+owner.
select *
from api_tokens
where id = @id;

-- name: ListApiTokensByUser :many
-- Cursor pagination. include_revoked toggles whether revoked
-- rows are surfaced (default false at the API edge); expired-but-not-revoked
-- rows are always included so the UI can explain why a token stopped
-- working. Both /v1/users/me/api-tokens and /v1/users/{id}/api-tokens use
-- this query.
-- Caller passes limit_count = clamp(limit, 1, 200).
select *
from api_tokens
where user_id = @user_id
  and (@include_revoked::boolean or revoked_at is null)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: RevokeApiToken :exec
-- Idempotent: re-revoking a token is a no-op.
update api_tokens
set revoked_at = now()
where id = @id
  and revoked_at is null;

-- name: RevokeApiTokensForUser :exec
-- Bulk-revokes every still-active token belonging to a user. Used by
-- the user-delete handler so a soft-deleted user cannot continue to
-- authenticate via a long-lived API token.
update api_tokens
set revoked_at = now()
where user_id = @user_id
  and revoked_at is null;

-- name: TouchApiToken :exec
update api_tokens
set last_used_at = now()
where id = @id;
