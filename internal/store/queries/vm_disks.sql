-- vm_disks queries — Phase A foundations. The vms table itself does
-- not carry a pool_id / node_id column (per the original schema design
-- multi-disk VMs span pools and node placement is observed through
-- vm_runtime). Pool persistence lives on vm_disks.storage_pool_id.
--
-- For MVP each VM has exactly one disk row, sourced from a template,
-- pinned to one storage_pool. CountVMsByPool joins through vm_disks
-- to support the storage_pools.delete extended-refusal check (Phase B).

-- name: CreateVMDisk :one
-- Inserts a single disk row for a vm. Phase B vm.create handler invokes
-- this once per VM (single-disk MVP). Multi-disk callers loop с distinct
-- device_order values.
insert into vm_disks (
    vm_id,
    storage_pool_id,
    device_order,
    bus,
    size_gib,
    source_kind,
    source_template_id,
    format,
    read_only,
    cache_mode,
    discard,
    boot_order
)
values (
    @vm_id,
    @storage_pool_id,
    @device_order,
    @bus,
    @size_gib,
    @source_kind,
    sqlc.narg('source_template_id')::uuid,
    @format,
    @read_only,
    @cache_mode,
    @discard,
    sqlc.narg('boot_order')::int
)
returning *;

-- name: ListVMDisksByVM :many
-- Returns every active (not soft-deleted) disk row for a vm, ordered
-- by device_order. Phase B's GET /v1/vms/{id} response projects the
-- pool_id from disk[0].storage_pool_id (single-disk MVP).
select *
from vm_disks
where vm_id = @vm_id
  and deleted_at is null
order by device_order asc;

-- name: SoftDeleteVMDisksByVM :exec
-- Bulk soft-delete every disk row attached к a VM. Called by the
-- worker's vm.delete cleanup phase together с SoftDeleteVM. The
-- per-row deleted_at stamp keeps the on delete cascade behaviour of
-- vm_disks.vm_id from removing rows that audit reads might still
-- need.
update vm_disks
set deleted_at = now()
where vm_id = @vm_id
  and deleted_at is null;

-- name: CountVMsByPool :one
-- Distinct-VM count for a given storage_pool_id, used by
-- storage_pools.delete to refuse deletion of pools backing live VMs.
-- Counts via vm_disks (the only place pool_id lives для a vm) and
-- excludes soft-deleted disks AND soft-deleted VMs.
select count(distinct d.vm_id)::bigint
from vm_disks d
join vms v on v.id = d.vm_id
where d.storage_pool_id = @storage_pool_id
  and d.deleted_at is null
  and v.deleted_at is null;
