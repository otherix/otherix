-- name: CreateNetwork :one
-- Caller materialises mtu to its API default (1500) when not supplied;
-- the column carries a DB-level DEFAULT 1500 since migration 00004 so
-- a row that bypasses sqlc still gets a sensible value, but we always
-- pass through the API surface explicitly to keep the audit trail
-- honest.
insert into networks (
    id,
    name,
    type,
    bridge_name,
    vlan_tag,
    mtu,
    config
)
values (
    @id,
    @name,
    @type,
    @bridge_name,
    sqlc.narg('vlan_tag')::int,
    @mtu,
    @config
)
returning *;

-- name: GetNetworkByID :one
select *
from networks
where id = @id
  and deleted_at is null;

-- name: GetNetworkByName :one
-- Case-insensitive on lower(name) to match the uq_networks_name
-- functional index that backs the name-based resolver.
-- Used to detect rename collisions on PATCH before issuing the UPDATE
-- (handler-side dedup also covers it, but checking up front gives a
-- nicer 409 path with no half-applied state). Networks are not yet
-- wired into the resolver package, but the index and query are kept
-- consistent so a follow-up iteration can drop in ResolveNetwork
-- without a second schema pass.
select *
from networks
where lower(name) = lower(@name)
  and deleted_at is null;

-- name: ListNetworks :many
-- Optional ?type filter (only 'bridge' is currently a meaningful value
-- but the column is open to future enum extensions). Cursor pagination.
select *
from networks
where deleted_at is null
  and (sqlc.narg('type')::network_type is null or type = sqlc.narg('type')::network_type)
  and (
    sqlc.narg('cursor_created_at')::timestamptz is null
    or (created_at, id) > (
      sqlc.narg('cursor_created_at')::timestamptz,
      sqlc.narg('cursor_id')::uuid
    )
  )
order by created_at, id
limit @limit_count;

-- name: UpdateNetwork :one
-- Caller merges the patch with the current row before issuing this
-- query (the same pattern as UpdateUser). The `type` column is
-- intentionally absent from this query — it is API-immutable and
-- changing it would require schema-level coordination with vm_nics.
update networks
set name        = @name,
    bridge_name = @bridge_name,
    vlan_tag    = sqlc.narg('vlan_tag')::int,
    mtu         = @mtu,
    config      = @config
where id = @id
  and deleted_at is null
returning *;

-- name: SoftDeleteNetwork :exec
update networks
set deleted_at = now()
where id = @id
  and deleted_at is null;

-- name: CountVMNicsOnNetwork :one
-- Precheck before DELETE /v1/networks/{id}: counts active vm_nics that
-- still reference the network. Soft-deleted NICs are excluded — a
-- detached NIC has no live attachment and does not block network
-- deletion. The FK uses ON DELETE RESTRICT, so this check also avoids
-- the SQL-level error path; we surface 409 with a typed payload
-- instead.
select count(*)::bigint as nic_count
from vm_nics
where network_id = @network_id::uuid
  and deleted_at is null;
