-- Idempotency-Key middleware queries against the idempotency_keys table
-- created in 00001_init.sql. The middleware contract:
--
--   1. Get → check for an existing row keyed by the client-supplied key.
--   2. If absent, Begin atomically inserts an in_flight row. ON CONFLICT
--      DO NOTHING returns no row when a concurrent caller already
--      claimed the key; the middleware then re-runs Get and treats the
--      existing row as authoritative.
--   3. If present and expired, Reclaim overwrites the row in place under
--      WHERE expires_at < now() so a row that just got refreshed by a
--      concurrent caller is never clobbered.
--   4. After the wrapped handler finishes, Complete records the cached
--      response and flips state to 'completed'.

-- name: GetIdempotencyKey :one
select *
from idempotency_keys
where key = @key;

-- name: BeginIdempotencyKey :one
-- Inserts a fresh in_flight row. Returns pgx.ErrNoRows when another
-- caller has already inserted this key — the caller must re-issue
-- GetIdempotencyKey to read whatever they ended up with.
insert into idempotency_keys (
    key, user_id, request_method, request_path, request_hash, expires_at
) values (
    @key, @user_id, @request_method, @request_path, @request_hash, @expires_at
)
on conflict (key) do nothing
returning *;

-- name: ReclaimIdempotencyKey :one
-- Overwrites an expired row in place. Returns pgx.ErrNoRows when a
-- concurrent caller already reclaimed (or the row no longer satisfies
-- expires_at < now()).
update idempotency_keys
set user_id          = @user_id,
    request_method   = @request_method,
    request_path     = @request_path,
    request_hash     = @request_hash,
    response_status  = null,
    response_headers = null,
    response_body    = null,
    state            = 'in_flight',
    created_at       = now(),
    completed_at     = null,
    expires_at       = @expires_at
where key = @key
  and expires_at < now()
returning *;

-- name: CompleteIdempotencyKey :exec
-- Marks an in_flight row as completed and stores the cached response.
-- The state='in_flight' guard prevents a late-arriving Complete from
-- clobbering an already-completed (or reclaimed) row.
update idempotency_keys
set state            = 'completed',
    response_status  = @response_status,
    response_headers = @response_headers,
    response_body    = @response_body,
    completed_at     = now()
where key = @key
  and state = 'in_flight';

-- name: DeleteIdempotencyKey :exec
-- Removes an in_flight row when the wrapped handler returns a non-2xx
-- response. The state guard avoids racing with a Complete that already
-- landed (which would mean the row is no longer ours to delete).
delete from idempotency_keys
where key = @key
  and state = 'in_flight';

-- name: DeleteExpiredIdempotencyKeys :execrows
-- Cleanup hook for the future maintenance loop. Not wired up yet.
delete from idempotency_keys
where expires_at < now();
