-- name: GetClusterSettings :one
-- Returns the singleton cluster_settings row. The migration seeds the
-- row at id=1 with NULL default_pool_name; this query never returns
-- pgx.ErrNoRows in a healthy schema.
select *
from cluster_settings
where id = 1;

-- name: SetDefaultPoolName :exec
-- Sets the cluster's default pool name. The handler validates beforehand
-- that at least one pool instance с this name exists.
update cluster_settings
set default_pool_name = @default_pool_name
where id = 1;

-- name: ClearDefaultPoolName :exec
-- Clears the cluster default-pool reference. VM create without --pool
-- afterwards returns default_pool_not_set until a new default is set.
update cluster_settings
set default_pool_name = null
where id = 1;
