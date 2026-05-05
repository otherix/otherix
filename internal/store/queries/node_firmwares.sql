-- name: ListNodeFirmwares :many
-- Backs GET /v1/nodes/{id}/firmwares. Returns one row per
-- (node, firmware) pair recorded in node_firmwares, joined with the
-- firmwares row so the API can project the nested Firmware view in
-- one trip. Cursor pagination — the cursor is anchored
-- on the firmwares.created_at + firmwares.id pair (stable across
-- heartbeats), not on node_firmwares.reported_at, so a heartbeat
-- bumping reported_at does not destabilise an in-flight pagination
-- session.
--
-- Until the heartbeat receiver lands node_firmwares is empty and
-- this query always returns no rows; the endpoint stands so clients
-- and tests can exercise the contract end-to-end ahead of the
-- ingest path.
select
    sqlc.embed(nf),
    sqlc.embed(f)
from node_firmwares nf
join firmwares f on f.id = nf.firmware_id
where nf.node_id = @node_id::uuid
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (f.created_at, f.id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by f.created_at, f.id
limit @limit_count;

-- name: UpsertNodeFirmware :exec
-- Heartbeat-driven projection: refresh the (node_id, firmware_id) row
-- with the agent-reported paths and bump reported_at. Conflict on the
-- composite primary key resolves to UPDATE; this is the only writer
-- so race-free.
insert into node_firmwares (
    node_id,
    firmware_id,
    code_path,
    vars_path,
    available,
    reported_at
)
values (
    @node_id,
    @firmware_id,
    @code_path,
    sqlc.narg('vars_path')::text,
    @available,
    now()
)
on conflict (node_id, firmware_id) do update
set code_path   = excluded.code_path,
    vars_path   = excluded.vars_path,
    available   = excluded.available,
    reported_at = excluded.reported_at;
