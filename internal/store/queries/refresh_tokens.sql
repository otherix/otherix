-- name: CreateRefreshToken :one
insert into refresh_tokens (
    id, user_id, token_hash,
    family_id, parent_id,
    user_agent, ip_address,
    expires_at
)
values (
    @id, @user_id, @token_hash,
    @family_id, @parent_id,
    @user_agent, @ip_address,
    @expires_at
)
returning *;

-- name: GetRefreshTokenByHash :one
-- Returns the row regardless of revocation state. The caller inspects
-- revoked_at to detect replay (sentinel for token theft) and expires_at
-- to detect expiry; both are normal control-flow signals, not errors.
select *
from refresh_tokens
where token_hash = @token_hash;

-- name: RevokeRefreshToken :exec
-- Idempotent: re-revoking a token is a no-op.
update refresh_tokens
set revoked_at = now()
where id = @id
  and revoked_at is null;

-- name: RevokeRefreshTokenFamily :exec
-- Revokes every active token in the family. Called when a revoked token
-- is presented at /v1/auth/refresh — the canonical signal of token theft.
update refresh_tokens
set revoked_at = now()
where family_id = @family_id
  and revoked_at is null;

-- name: RevokeAllUserRefreshTokens :exec
-- Logout-from-all-sessions. Revokes every active token for the user.
update refresh_tokens
set revoked_at = now()
where user_id = @user_id
  and revoked_at is null;

-- name: TouchRefreshToken :exec
update refresh_tokens
set last_used_at = now()
where id = @id;

-- name: DeleteExpiredRefreshTokens :execrows
-- Cleanup query for the (out-of-scope here) periodic cleanup job. Drops
-- every row whose expiry is older than @cutoff. Returns the number of
-- rows deleted so the job can log progress.
delete from refresh_tokens
where expires_at < @cutoff;
