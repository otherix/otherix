-- name: CreateJoinToken :one
-- Persists а freshly minted join token. Plaintext is never stored —
-- the caller hashes via auth.HashToken before invoking this query.
-- intended_node_name + max_uses are validated at the API edge AND by
-- SQL CHECK constraints (defense-in-depth).
insert into join_tokens (
    id, token_hash, intended_node_name,
    created_by_user_id, expires_at, max_uses
)
values (
    @id, @token_hash, sqlc.narg('intended_node_name'),
    sqlc.narg('created_by_user_id'), @expires_at, sqlc.narg('max_uses')
)
returning *;

-- name: GetJoinTokenByID :one
-- Includes expired rows; the caller (admin handler) decides what к do.
select *
from join_tokens
where id = @id;

-- name: GetJoinTokenByHash :one
-- Non-locking read for callers that need а token row outside an
-- exclusive transaction. Filters expired tokens at SQL так а fresh
-- read never returns а usable-but-expired row. Step 2 redemption
-- uses GetJoinTokenByHashForUpdate instead (acquires а row lock for
-- race-safe max_uses enforcement). pgx.ErrNoRows ⇒ ErrTokenInvalid.
select *
from join_tokens
where token_hash = @token_hash
  and expires_at > now();

-- name: GetJoinTokenByHashForUpdate :one
-- Row-locking variant used by the Step 2 redemption handler. Acquires
-- а FOR UPDATE lock so concurrent redemptions on the same token
-- queue, then race-fairly observe each other's consumption count.
-- Preserves the expires_at filter — an already-expired token surfaces
-- as ErrNoRows и no lock is held. pgx.ErrNoRows ⇒ ErrTokenInvalid.
select *
from join_tokens
where token_hash = @token_hash
  and expires_at > now()
for update;

-- name: ListJoinTokens :many
-- Cursor pagination. include_expired toggles whether
-- expired rows surface (default false; UI / scripts opt in). The
-- consumption_count surfaces via correlated subquery — first slice
-- has low cardinality, no perf concern.
select
    jt.*,
    (
        select count(*)
        from join_token_consumptions c
        where c.join_token_id = jt.id
    )::bigint as consumption_count
from join_tokens jt
where (@include_expired::boolean or jt.expires_at > now())
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (jt.created_at, jt.id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by jt.created_at, jt.id
limit @limit_count;

-- name: CountJoinTokenConsumptions :one
-- Used by Step 2 redemption to enforce max_uses + by the get handler
-- to surface consumption_count on а single-row lookup.
select count(*)::bigint
from join_token_consumptions
where join_token_id = @join_token_id;

-- name: RevokeJoinToken :exec
-- Sets expires_at to the lesser of its current value и now() — а
-- token already в the past stays where it is, а live token jumps к
-- the deadline. Idempotent; repeat calls produce no further effect.
update join_tokens
set expires_at = least(expires_at, now())
where id = @id;

-- name: ListJoinTokenConsumptions :many
-- Cursor pagination. Chronologically-ordered audit
-- entries for а single token. Empty until Step 2 ships redemption.
select *
from join_token_consumptions
where join_token_id = @join_token_id
  and (
    sqlc.narg('cursor_consumed_at')::timestamptz is null
    or (consumed_at, id) > (
      sqlc.narg('cursor_consumed_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by consumed_at, id
limit @limit_count;

-- name: CreateJoinTokenConsumption :one
-- Step 2 dependency: redemption handler inserts а consumption row в
-- the same TX as agent_certs INSERT + optional node UPSERT. Pre-
-- declared в Step 1 so the signature locks early и Step 2 only
-- changes call sites.
insert into join_token_consumptions (
    id, join_token_id, consumed_by_node_id, source_ip
)
values (
    @id, @join_token_id, sqlc.narg('consumed_by_node_id'), sqlc.narg('source_ip')
)
returning *;
