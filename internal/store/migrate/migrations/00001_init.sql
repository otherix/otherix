-- Otherix initial schema. User-owned resources carry owner_id (single-
-- tenancy model).
--
-- Pre-production schema squash (2026-05-08): folds the previously
-- separate migrations 00002_refresh_tokens, 00003_node_force_delete_support,
-- 00004_networks_mtu_default, 00005_firmwares_updated_at,
-- 00006_node_runtime_resources, and 00008_tasks into this file. The
-- river goose Go-bridge (formerly 00007_river_v6.go) is renumbered to
-- 00002_river_v6.go and remains separate because it lives at a
-- different layer (rivermigrate.MigrateTx through goose Go API).
-- The immutability rule applies to migrations after they ship to
-- production; the squash takes the explicit pre-production exception
-- before the schema accumulates archaeology.
--
-- Storage groundwork (2026-05-08) lands inline per the same pre-
-- production exception: drops node_image_cache and the
-- image_cache_status enum (replaced by storage_images junction);
-- converts storage_pools.type from enum to text + check;
-- adds templates.derived_vm_count; adds the storage_images junction
-- table; adds tasks.agent_task_id for the resumption pattern.
--
-- NO TRANSACTION: required because the river schema (embedded below) uses
-- ALTER TYPE ... ADD VALUE, which cannot be followed by a reference to the new
-- enum value within the same transaction (PostgreSQL restriction SQLSTATE 55P04).
-- +goose NO TRANSACTION

-- +goose Up

-- ============ extensions ============
create extension if not exists pgcrypto;

-- ============ helper functions ============

-- pure-SQL UUIDv7 generator (no extension required)
-- +goose StatementBegin
create or replace function uuid_generate_v7() returns uuid
language plpgsql parallel safe as $$
declare
  unix_ts_ms bigint;
  uuid_bytes bytea;
begin
  unix_ts_ms := (extract(epoch from clock_timestamp()) * 1000)::bigint;
  uuid_bytes := gen_random_bytes(16);
  uuid_bytes := set_byte(uuid_bytes, 0, ((unix_ts_ms >> 40) & 255)::int);
  uuid_bytes := set_byte(uuid_bytes, 1, ((unix_ts_ms >> 32) & 255)::int);
  uuid_bytes := set_byte(uuid_bytes, 2, ((unix_ts_ms >> 24) & 255)::int);
  uuid_bytes := set_byte(uuid_bytes, 3, ((unix_ts_ms >> 16) & 255)::int);
  uuid_bytes := set_byte(uuid_bytes, 4, ((unix_ts_ms >>  8) & 255)::int);
  uuid_bytes := set_byte(uuid_bytes, 5, ( unix_ts_ms        & 255)::int);
  -- version 7 in upper nibble of byte 6
  uuid_bytes := set_byte(uuid_bytes, 6, ((get_byte(uuid_bytes, 6) & 15) | 112));
  -- variant 10xx in upper bits of byte 8
  uuid_bytes := set_byte(uuid_bytes, 8, ((get_byte(uuid_bytes, 8) & 63) | 128));
  return encode(uuid_bytes, 'hex')::uuid;
end $$;
-- +goose StatementEnd

-- bump updated_at on every UPDATE
-- +goose StatementBegin
create or replace function trg_set_updated_at() returns trigger
language plpgsql as $$
begin
  new.updated_at := now();
  return new;
end $$;
-- +goose StatementEnd

-- bump generation when any whitelisted field (TG_ARGV) actually changes
-- +goose StatementBegin
create or replace function trg_bump_generation() returns trigger
language plpgsql as $$
declare
  field text;
  bumped boolean := false;
begin
  if tg_op = 'UPDATE' then
    foreach field in array tg_argv loop
      if not (to_jsonb(new) ? field) then
        raise warning 'trg_bump_generation: field % not found on row of table %.%',
          field, tg_table_schema, tg_table_name;
        continue;
      end if;
      if (to_jsonb(new) -> field) is distinct from (to_jsonb(old) -> field) then
        bumped := true;
        exit;
      end if;
    end loop;
    if bumped then
      new.generation := old.generation + 1;
    else
      new.generation := old.generation;
    end if;
  end if;
  return new;
end $$;
-- +goose StatementEnd

-- ============ users ============
-- `role` is the RBAC anchor for the user. Permissions are not stored in
-- the DB — see docs/rbac.md for the (role, permission, scope) matrix.
-- Roles are fixed in code; dynamic roles are not supported.
create table users (
  id              uuid primary key default uuid_generate_v7(),
  email           text not null,
  password_hash   text not null,
  display_name    text not null default '',
  role            text not null,
  last_login_at   timestamptz,
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now(),
  deleted_at      timestamptz,
  constraint chk_users_role check (role in ('admin', 'operator', 'developer', 'viewer'))
);
create unique index uq_users_email on users(lower(email)) where deleted_at is null;
create index idx_users_role on users(role) where deleted_at is null;
create index idx_users_pagination on users(created_at, id) where deleted_at is null;
create trigger users_set_updated_at before update on users
  for each row execute function trg_set_updated_at();

-- ============ api_tokens ============
create table api_tokens (
  id           uuid primary key default uuid_generate_v7(),
  user_id      uuid not null references users(id) on delete cascade,
  name         text not null,
  token_hash   bytea not null,
  prefix       text not null,
  scopes       text[] not null default '{}',
  last_used_at timestamptz,
  expires_at   timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz not null default now()
);
create unique index uq_api_tokens_hash on api_tokens(token_hash);
create index idx_api_tokens_user on api_tokens(user_id) where revoked_at is null;

-- ============ refresh_tokens ============
-- Stateful, rotated refresh-token flow. Each successful login starts a
-- new family (family_id = id of the first refresh token). On every
-- refresh, the presented token is revoked and a new token is issued in
-- the same family with parent_id pointing back. If a revoked token is
-- presented again, the entire family is revoked — that pattern is the
-- canonical detection-of-token-theft signal. See internal/auth/service.go.
--
-- token_hash stores sha256(plaintext) as bytea, mirroring api_tokens
-- and join_tokens. Plaintext is never persisted.
create table refresh_tokens (
  id           uuid primary key default uuid_generate_v7(),
  user_id      uuid not null references users(id) on delete cascade,
  token_hash   bytea not null,
  family_id    uuid not null,
  parent_id    uuid references refresh_tokens(id) on delete set null,
  user_agent   text,
  ip_address   inet,
  issued_at    timestamptz not null default now(),
  expires_at   timestamptz not null,
  revoked_at   timestamptz,
  last_used_at timestamptz
);
create unique index uq_refresh_tokens_hash    on refresh_tokens(token_hash);
create index        idx_refresh_tokens_user   on refresh_tokens(user_id) where revoked_at is null;
create index        idx_refresh_tokens_family on refresh_tokens(family_id);
create index        idx_refresh_tokens_expiry on refresh_tokens(expires_at) where revoked_at is null;

-- ============ enums (infra) ============
create type node_status as enum ('pending', 'ready', 'cordoned', 'draining', 'unreachable', 'gone');
create type cpu_arch as enum ('amd64', 'arm64');

-- ============ nodes ============
-- cpu_cores_available and memory_available_mib are the volatile per-tick
-- resource counters reported in every heartbeat (NodeResourcesReport in
-- api/openapi/control-plane.yaml). Both nullable — pre-heartbeat
-- `pending` rows have NULL until the first heartbeat lands. The receiver
-- (POST /v1/nodes/{id}/heartbeat) populates them via UpdateNodeHeartbeat.
create table nodes (
  id                         uuid primary key default uuid_generate_v7(),
  name                       text not null,
  architecture               cpu_arch not null,
  advertised_endpoint        text not null,
  migration_host             text not null,
  migration_port_range_start integer not null,
  migration_port_range_end   integer not null,
  status                     node_status not null default 'pending',
  cordoned_at                timestamptz,
  cpu_cores_total            int,
  cpu_cores_available        int,
  cpu_model                  text,
  cpu_flags                  text[] not null default '{}',
  memory_total_mib           bigint,
  memory_available_mib       bigint,
  hugepages_2mib_total       int,
  hugepages_1gib_total       int,
  kernel_version             text,
  qemu_version               text,
  numa_topology              jsonb,
  capabilities               jsonb not null default '{}'::jsonb,
  last_heartbeat_at          timestamptz,
  agent_version              text,
  labels                     jsonb not null default '{}'::jsonb,
  memory_pressure_since      timestamptz,
  memory_pressure_count      int not null default 0,
  system_disk_total_bytes      bigint,
  system_disk_available_bytes  bigint,
  system_disk_pressure_since   timestamptz,
  system_disk_pressure_count   int not null default 0,
  created_at                 timestamptz not null default now(),
  updated_at                 timestamptz not null default now(),
  deleted_at                 timestamptz,
  constraint nodes_migration_port_range_valid check (
    migration_port_range_start >= 1024
    and migration_port_range_end <= 65535
    and migration_port_range_end >= migration_port_range_start
  ),
  constraint chk_nodes_memory_pressure_count_nonneg check (memory_pressure_count >= 0),
  constraint chk_nodes_system_disk_pressure_count_nonneg check (system_disk_pressure_count >= 0)
);
-- Pressure-tracking columns. CP-side
-- computed at heartbeat receipt: when reported memory availability falls
-- below placement.pressure.memory.threshold_percent, the count increments;
-- once it reaches placement.pressure.memory.consecutive_required the row
-- transitions to "pressured" by stamping memory_pressure_since with now().
-- Recovery is asymmetric — a single observation at-or-above the threshold
-- clears memory_pressure_since and zeroes the counter. The scheduler joins
-- through node_effective_availability and excludes rows where
-- memory_pressure_since IS NOT NULL (see queries/storage_pools.sql).
comment on column nodes.memory_pressure_since is
    'Timestamp when memory pressure condition was set. NULL when condition not active. '
    'CP-side computed from heartbeat metrics against placement.pressure.memory.threshold_percent.';
comment on column nodes.memory_pressure_count is
    'Consecutive heartbeat observations below memory pressure threshold. Used for debouncing — '
    'count reaches placement.pressure.memory.consecutive_required → pressure set. Reset to 0 on '
    'first observation at-or-above threshold.';

-- System disk metrics + pressure.
-- Agent collects `/` filesystem capacity via syscall.Statfs and reports the
-- raw bytes; CP detection mirrors memory pressure (3 consecutive heartbeats
-- below `placement.pressure.system_disk.threshold_percent` → set; single
-- observation at-or-above → clear). Same debounce-counter pattern as
-- memory.
comment on column nodes.system_disk_total_bytes is
    'Root filesystem total capacity (bytes) reported by the agent via syscall.Statfs("/"). '
    'NULL when the agent could not read the metric — CP carries existing pressure state forward.';
comment on column nodes.system_disk_available_bytes is
    'Root filesystem available bytes reported by the agent. Pairs with system_disk_total_bytes; both '
    'nullable together. Volatile per heartbeat.';
comment on column nodes.system_disk_pressure_since is
    'Timestamp when system_disk pressure condition was set. NULL when condition not active. '
    'CP-side computed from heartbeat metrics against placement.pressure.system_disk.threshold_percent.';
comment on column nodes.system_disk_pressure_count is
    'Consecutive heartbeat observations below system_disk pressure threshold. Used for debouncing — '
    'count reaches placement.pressure.system_disk.consecutive_required → pressure set. Reset to 0 on '
    'first observation at-or-above threshold.';
-- uq_nodes_name is a case-insensitive functional UNIQUE on lower(name):
-- the resolver layer (internal/api/handlers/internal/resolver) looks up
-- nodes by name with `where lower(name) = lower(@name)`, so the index
-- must match that query shape OR the planner falls back to a sequential
-- scan. Original casing is preserved on the row; only equality is
-- case-folded.
create unique index uq_nodes_name on nodes(lower(name)) where deleted_at is null;
create index idx_nodes_status on nodes(status) where deleted_at is null;
create index idx_nodes_arch on nodes(architecture) where deleted_at is null;
create index idx_nodes_labels_gin on nodes using gin (labels jsonb_path_ops);
create index idx_nodes_pagination on nodes(created_at, id) where deleted_at is null;
create trigger nodes_set_updated_at before update on nodes
  for each row execute function trg_set_updated_at();

-- ============ agent_certs ============
create table agent_certs (
  id                  uuid primary key default uuid_generate_v7(),
  node_id             uuid not null references nodes(id) on delete cascade,
  serial              bytea not null,
  fingerprint_sha256  bytea not null,
  subject_dn          text not null,
  not_before          timestamptz not null,
  not_after           timestamptz not null,
  issued_at           timestamptz not null default now(),
  revoked_at          timestamptz,
  revocation_reason   text
);
create unique index uq_agent_certs_fingerprint on agent_certs(fingerprint_sha256);
create unique index uq_agent_certs_serial on agent_certs(serial);
create index idx_agent_certs_node on agent_certs(node_id) where revoked_at is null;

-- ============ join_tokens ============
-- Multi-use bootstrap tokens. max_uses NULL = unlimited
-- within TTL; non-null = strict consumption cap (decremented at
-- redemption time in Step 2 of the join-token bootstrap landing).
-- intended_node_name SET ⇒ max_uses MUST be 1 (single-use pre-bound)
-- — defense-in-depth: API edge validates, SQL CHECK constraint
-- enforces. consumption_count is a derived value computed via
-- correlated subquery against join_token_consumptions.
--
-- Revocation: set expires_at = LEAST(expires_at, now()). No
-- revoked_at column — TTL-driven natural decay is the only
-- invalidation path. Idx on expires_at supports both range queries and
-- the inevitable cleanup sweep (future ROADMAP item).
create table join_tokens (
  id                    uuid primary key default uuid_generate_v7(),
  token_hash            bytea not null,
  intended_node_name    text,
  created_by_user_id    uuid references users(id) on delete set null,
  expires_at            timestamptz not null,
  max_uses              int,
  created_at            timestamptz not null default now(),
  constraint chk_join_tokens_max_uses_positive
    check (max_uses is null or max_uses >= 1),
  constraint chk_join_tokens_prebound_single_use
    check (intended_node_name is null or max_uses = 1)
);
create unique index uq_join_tokens_hash on join_tokens(token_hash);
-- Predicate index on expires_at without a `now()` clause — PostgreSQL
-- forbids volatile functions in index predicates. Range queries over
-- the column still benefit from the index in practice.
create index idx_join_tokens_expiry on join_tokens(expires_at);

-- ============ join_token_consumptions ============
-- Audit trail for multi-use join tokens. Each successful redemption
-- (Step 2 of the bootstrap landing) inserts a row in the same TX as
-- the agent_certs INSERT and optional node row UPSERT. The audit
-- surface (GET /v1/nodes/join-tokens/{id}/consumptions) is operator-
-- facing.
--
-- FK semantics:
--   - join_token_id ON DELETE CASCADE — wiping a token wipes its trail
--     (operator action; if a revoked token is then deleted out-of-band,
--     orphan rows have no caller).
--   - consumed_by_node_id ON DELETE SET NULL — a soft-deleted node
--     should not erase the historical fact that some agent bootstrapped
--     via this token. NULL surfaces in API as "node since removed".
create table join_token_consumptions (
  id                  uuid primary key default uuid_generate_v7(),
  join_token_id       uuid not null references join_tokens(id) on delete cascade,
  consumed_by_node_id uuid references nodes(id) on delete set null,
  consumed_at         timestamptz not null default now(),
  source_ip           inet
);
create index idx_join_token_consumptions_by_token
  on join_token_consumptions(join_token_id, consumed_at);
create index idx_join_token_consumptions_by_node
  on join_token_consumptions(consumed_by_node_id)
  where consumed_by_node_id is not null;

-- ============ ca_certs ============
-- Cluster Certificate Authority material. The CP
-- auto-generates an ECDSA P-384 self-signed CA on first boot when
-- no active row exists; subsequent boots skip generation idempotently.
-- Rotation (future ROADMAP) flips active=false on existing row before
-- inserting a new active=true row — partial unique index enforces the
-- at-most-one-active invariant.
--
-- Trust model: cert_pem + key_pem stored plaintext (DB-level security
-- responsibility, equivalent posture to jwt_secret / refresh tokens /
-- argon2id password hashes). KMS / encryption-at-rest tracked as
-- future iteration.
create table ca_certs (
  id                 uuid primary key default uuid_generate_v7(),
  cert_pem           bytea not null,
  key_pem            bytea not null,
  fingerprint_sha256 bytea not null,
  not_before         timestamptz not null,
  not_after          timestamptz not null,
  active             boolean not null default true,
  created_at         timestamptz not null default now()
);
create unique index uq_ca_certs_fingerprint on ca_certs(fingerprint_sha256);
create unique index uq_ca_certs_active on ca_certs(active) where active = true;

-- ============ enums (firmware/storage/network) ============
create type firmware_type as enum ('bios', 'uefi');
create type network_type as enum ('bridge');

-- ============ firmwares ============
-- updated_at + trg_set_updated_at trigger so PATCH /v1/firmwares/{id}
-- can advertise when the metadata last changed; consistent with
-- networks / storage_pools / templates / users / vms.
create table firmwares (
  id            uuid primary key default uuid_generate_v7(),
  name          text not null,
  architecture  cpu_arch not null,
  type          firmware_type not null,
  version       text,
  secure_boot   boolean not null default false,
  is_default    boolean not null default false,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);
create unique index uq_firmwares_name_arch on firmwares(name, architecture);
create unique index uq_firmwares_default on firmwares(architecture, type) where is_default = true;
create trigger firmwares_set_updated_at before update on firmwares
  for each row execute function trg_set_updated_at();

-- ============ node_firmwares ============
create table node_firmwares (
  node_id      uuid not null references nodes(id) on delete cascade,
  firmware_id  uuid not null references firmwares(id) on delete cascade,
  code_path    text not null,
  vars_path    text,
  available    boolean not null default true,
  reported_at  timestamptz not null default now(),
  primary key (node_id, firmware_id)
);

-- ============ storage_pools ============
-- Multi-instance pool architecture:
-- Pool name is a cluster-wide concept (analogous to k8s StorageClass);
-- a pool *instance* is the per-node row materialising that name on a
-- specific node's storage. The same name `default` may exist on
-- multiple nodes simultaneously, each as its own instance row. The
-- cluster default-pool name is tracked separately in cluster_settings
-- (singleton); is_default is intentionally NOT a per-row property.
create table storage_pools (
  id                  uuid primary key default uuid_generate_v7(),
  node_id             uuid not null references nodes(id) on delete cascade,
  name                text not null,
  -- type is text + check (not enum): future pool backends (lvm-thin,
  -- nfs, ceph-rbd, zfspool) widen the constraint rather than requiring
  -- an ENUM ALTER. The value 'local_dir' is preserved from the prior
  -- enum to keep the
  -- agent OpenAPI contract (StoragePoolReportType) and downstream
  -- generated code unchanged; renaming to a shorter canonical name
  -- is its own naming-alignment slice.
  type                text not null default 'local_dir' check (type in ('local_dir')),
  path                text not null,
  capacity_bytes      bigint,
  available_bytes     bigint,
  reported_at         timestamptz,
  config              jsonb not null default '{}'::jsonb,
  disk_pressure_since timestamptz,
  disk_pressure_count int not null default 0,
  -- Reconciliation status (resource reconciliation via heartbeat).
  -- The agent reconciler reports the outcome of its mkdir / registry mutation
  -- attempt through the heartbeat path; CP merges the report onto the row so
  -- operator-facing `GET /v1/storage-pools/{id}` surfaces real readiness.
  -- 'pending' is the initial state right after `POST /v1/storage-pools`;
  -- the owning node moves it to 'ready' on successful reconciliation or
  -- 'failed' when mkdir / path validation on the node fails.
  reconciliation_status text not null default 'pending'
      check (reconciliation_status in ('pending', 'ready', 'failed')),
  last_reconciled_at  timestamptz,
  reconciliation_error text,
  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),
  deleted_at          timestamptz,
  constraint chk_pools_disk_pressure_count_nonneg check (disk_pressure_count >= 0)
);
-- Pressure-tracking columns. CP-side
-- computed at scan-completion: when the agent's UpsertStoragePoolUsage
-- write lands, the scan worker compares `available_bytes / capacity_bytes`
-- against `placement.pressure.disk.threshold_percent` and runs the same
-- transition logic as node-level memory / system_disk pressure. Defaults
-- to single-observation debouncing (`consecutive_required: 1`) since scan
-- cadence is minutes apart, not seconds.
comment on column storage_pools.disk_pressure_since is
    'Timestamp when pool disk pressure was set. NULL when condition not active. '
    'CP-side computed from scan metrics against placement.pressure.disk.threshold_percent.';
comment on column storage_pools.disk_pressure_count is
    'Consecutive scan observations below pool disk pressure threshold. Used for debouncing — '
    'default consecutive_required=1 means a single scan sets pressure. Reset to 0 on first '
    'observation at-or-above threshold.';
-- uq_storage_pools_name is per-node UNIQUE on lower(name): same
-- conceptual pool may be materialised across multiple nodes (one
-- instance row per node). Resolution from a bare name returns all
-- instances; the scheduler picks the node-bound instance at VM-create
-- time.
create unique index uq_storage_pools_name on storage_pools(node_id, lower(name)) where deleted_at is null;
create trigger storage_pools_set_updated_at before update on storage_pools
  for each row execute function trg_set_updated_at();

-- ============ cluster_settings ============
-- Singleton table for cluster-level configuration. Row PK is fixed to
-- 1 by check constraint so the table holds exactly one row; the seed
-- INSERT below materialises it during migration. Future-extensible
-- (default_template_name, default_network_name, …) without altering
-- the singleton invariant.
create table cluster_settings (
  id                integer primary key default 1 check (id = 1),
  default_pool_name text,
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now()
);
insert into cluster_settings (id) values (1);
create trigger cluster_settings_set_updated_at before update on cluster_settings
  for each row execute function trg_set_updated_at();

-- ============ networks ============
-- mtu is NOT NULL DEFAULT 1500. MTU mismatch between bridge, TAP, and
-- guest NIC is a hard failure mode for QEMU networking — surfacing
-- NULL as "inherit" was a footgun. 1500 is the universally-safe
-- Ethernet default; jumbo (9000) and overlay (1450) deployments
-- override explicitly per network.
create table networks (
  id            uuid primary key default uuid_generate_v7(),
  name          text not null,
  type          network_type not null,
  bridge_name   text not null,
  vlan_tag      int,
  mtu           int not null default 1500,
  config        jsonb not null default '{}'::jsonb,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  deleted_at    timestamptz,
  constraint chk_networks_vlan check (vlan_tag is null or (vlan_tag between 1 and 4094))
);
-- uq_networks_name parallels uq_storage_pools_name / uq_templates_name /
-- uq_nodes_name / uq_vms_name on the case-insensitive shape: the
-- name-based reference set does not yet add networks to the resolver
-- (networks are admin-only and addressable by id in current flows),
-- but the schema invariant is kept consistent so a later iteration
-- that introduces `ResolveNetwork` does not need a second DDL pass.
create unique index uq_networks_name on networks(lower(name)) where deleted_at is null;
create trigger networks_set_updated_at before update on networks
  for each row execute function trg_set_updated_at();

-- ============ enums (templates) ============
create type os_family as enum ('linux', 'windows', 'bsd', 'other');
create type image_format as enum ('qcow2', 'raw');

-- ============ templates ============
-- `visibility` controls who can list/use the template. `private` (default)
-- means only the owner and operator/admin see it; `public` means any
-- authenticated user can list and use it. Changing visibility is an
-- operator/admin action (`template:set_visibility`); the owner does NOT
-- become operator/admin upon publish — see docs/rbac.md.
create table templates (
  id                          uuid primary key default uuid_generate_v7(),
  owner_id                    uuid not null references users(id) on delete restrict,
  name                        text not null,
  description                 text not null default '',
  visibility                  text not null default 'private',
  architecture                cpu_arch not null,
  os_family                   os_family not null,
  os_variant                  text not null default '',
  image_url                   text not null,
  -- Nullable. NULL means "agent computes during import" (compute
  -- mode). Non-null means "operator pre-validated; agent verifies
  -- during download" (verify mode). After a successful
  -- compute-mode import, the worker idempotently back-propagates the
  -- computed hash via UpdateTemplateImageChecksumIfNull (only when
  -- column is still NULL) so the template effectively transitions
  -- to verify mode for all future operations.
  image_checksum_sha256       bytea,
  image_format                image_format not null default 'qcow2',
  image_size_bytes            bigint,
  firmware_type               firmware_type not null default 'uefi',
  firmware_id                 uuid references firmwares(id) on delete set null,
  default_cpu_cores           int not null default 2,
  default_memory_mib          int not null default 2048,
  default_disk_gib            int not null default 20,
  cloud_init_supported        boolean not null default true,
  -- L3: raw `#cloud-config` YAML baked into the template.
  -- NULL = none; CP-side VM-create resolves vm.user_data ?:
  -- template.cloud_init_user_data; agent receives a single resolved
  -- blob (Area 3 lock — full-replace override). Hostname injected
  -- CP-side via gopkg.in/yaml.v3 if the resolved YAML omits
  -- top-level `hostname:`.
  cloud_init_user_data        text,
  qemu_guest_agent_expected   boolean not null default false,
  metadata                    jsonb not null default '{}'::jsonb,
  -- Preparation for linked-clone semantics. Currently
  -- inert; a future vm.create handler increments on linked clone,
  -- decrements on delete or flatten. The check guards against
  -- decrement underflow if the lifecycle code ever miscounts.
  derived_vm_count            integer not null default 0 check (derived_vm_count >= 0),
  created_at                  timestamptz not null default now(),
  updated_at                  timestamptz not null default now(),
  deleted_at                  timestamptz,
  constraint chk_templates_visibility check (visibility in ('public', 'private')),
  constraint chk_templates_cpu    check (default_cpu_cores between 1 and 256),
  constraint chk_templates_memory check (default_memory_mib between 128 and 4194304),
  constraint chk_templates_disk   check (default_disk_gib between 1 and 65536)
);
-- uq_templates_name is case-insensitive on lower(name) for parity with
-- the other resolver-backed tables. The name-based resolver looks up
-- templates by name with `where lower(name) = lower(@name)` and this
-- index makes that an index scan rather than a seq scan. The
-- uniqueness contract — already documented in queries/templates.sql
-- as "global, not per-owner" — is preserved; only equality is folded.
create unique index uq_templates_name on templates(lower(name)) where deleted_at is null;
create index idx_templates_arch on templates(architecture) where deleted_at is null;
create index idx_templates_owner on templates(owner_id) where deleted_at is null;
-- Supports the common list query "public OR owned by me".
create index idx_templates_visibility on templates(visibility, owner_id) where deleted_at is null;
create trigger templates_set_updated_at before update on templates
  for each row execute function trg_set_updated_at();

-- ============ storage_images ============
-- Per-pool projection of a template's content. This junction
-- replaces the earlier node_image_cache shape.
--
-- Composite UNIQUE (template_id, pool_id) enforces "exactly one
-- image per template per pool". NO unique on
-- (pool_id, checksum_sha256) — content-addressable deduplication
-- permits multiple rows in the same pool to share one
-- <sha256>.qcow2 file on disk; the refcount gate at delete time
-- (CountStorageImagesBySHA256InPool) keeps the file alive until
-- the last referent goes.
create table storage_images (
  id              uuid primary key default uuid_generate_v7(),
  template_id     uuid not null references templates(id)     on delete restrict,
  pool_id         uuid not null references storage_pools(id) on delete restrict,
  checksum_sha256 text not null check (checksum_sha256 ~ '^[0-9a-f]{64}$'),
  size_bytes      bigint not null check (size_bytes > 0),
  format          text not null default 'qcow2',
  imported_at     timestamptz not null default now(),
  unique (template_id, pool_id)
);
create index storage_images_pool_sha256_idx on storage_images (pool_id, checksum_sha256);
create index storage_images_template_idx    on storage_images (template_id);

-- ============ enums (vm) ============
-- vm_phase includes 'orphaned': set on vm_runtime rows whose owning
-- node was force-deleted. Distinct from 'gone' (terminal post-deletion
-- state for the VM itself). desired_phase is NOT touched on
-- force-delete: the user still wants whatever they wanted before; the
-- runtime just can't deliver it.
create type vm_desired_phase as enum ('running', 'stopped', 'deleted');
create type disk_bus as enum ('virtio', 'sata', 'scsi', 'ide');
-- nic_model carries both Intel models: 'e1000' (older 8254x) and 'e1000e'
-- (newer 82574L). Operators choose per VM.
create type nic_model as enum ('virtio', 'e1000', 'e1000e', 'rtl8139');
create type snapshot_status as enum ('creating', 'ready', 'restoring', 'deleting', 'error');
-- disk format / cache mode / discard policy — surfaced via API on vm_disks.
-- 'image_format' (declared in the templates section above) is reused by
-- vm_disks.format; the values coincide.
create type disk_cache_mode as enum ('none', 'writeback', 'writethrough', 'directsync', 'unsafe');
create type disk_discard as enum ('ignore', 'unmap');
-- The state the VM was in at the moment a snapshot was captured. Distinct
-- from snapshot_status (which is the snapshot's own lifecycle); a 'ready'
-- snapshot may have been taken from a 'running' or 'stopped' VM.
create type vm_state_at_snapshot as enum ('running', 'paused', 'stopped');

-- ============ vms ============
create table vms (
  id                  uuid primary key default uuid_generate_v7(),
  owner_id            uuid not null references users(id) on delete restrict,
  name                text not null,
  description         text not null default '',
  desired_phase       vm_desired_phase not null default 'running',
  template_id         uuid references templates(id) on delete set null,
  architecture        cpu_arch not null,
  cpu_cores           int not null,
  memory_mib          int not null,
  cpu_model           text not null default 'host',
  machine_type        text not null,
  firmware_id         uuid references firmwares(id) on delete restrict,
  pinned_node_id      uuid references nodes(id) on delete set null,
  scheduler_hints     jsonb not null default '{}'::jsonb,
  user_data           text,
  -- L3 cloud-init disable flag (operator UX iteration). When true, the
  -- CP-side resolver returns empty user_data to the agent even if
  -- the template has a baked cloud_init_user_data — the agent skips
  -- cidata.iso generation entirely. Mutually exclusive with user_data:
  -- the operator either supplies a VM-level override OR explicitly
  -- disables cloud-init, never both. The CHECK enforces that pairing
  -- so the resolver does not have to guess intent on conflicting input.
  cloud_init_disabled boolean not null default false,
  meta_data           text,
  network_config      text,
  ssh_authorized_keys text[] not null default '{}',
  labels              jsonb not null default '{}'::jsonb,
  generation          bigint not null default 1,
  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),
  deleted_at          timestamptz,
  constraint chk_vms_cpu    check (cpu_cores between 1 and 256),
  constraint chk_vms_memory check (memory_mib between 128 and 4194304),
  constraint chk_vms_cloud_init_disabled_no_userdata check (
    cloud_init_disabled = false OR user_data IS NULL
  )
);
-- uq_vms_name is case-insensitive on lower(name): operator-facing VM
-- names are case-folded at the resolver edge so `demo-vm` and
-- `Demo-VM` collide. Same rationale as uq_nodes_name and the rest of
-- the name-based resolver set.
create unique index uq_vms_name on vms(lower(name)) where deleted_at is null;
create index idx_vms_owner on vms(owner_id) where deleted_at is null;
create index idx_vms_template on vms(template_id) where deleted_at is null;
create index idx_vms_pinned_node on vms(pinned_node_id) where deleted_at is null and pinned_node_id is not null;
create index idx_vms_labels_gin on vms using gin (labels jsonb_path_ops);
create trigger vms_set_updated_at before update on vms
  for each row execute function trg_set_updated_at();
create trigger vms_bump_generation before update on vms
  for each row execute function trg_bump_generation(
    'desired_phase', 'cpu_cores', 'memory_mib', 'cpu_model', 'machine_type',
    'firmware_id', 'pinned_node_id', 'scheduler_hints',
    'user_data', 'cloud_init_disabled',
    'meta_data', 'network_config', 'ssh_authorized_keys'
  );

-- ============ node_effective_availability (view) ============
-- Closes the race between vm.create commit and the next agent
-- heartbeat that subtracts the new VM's resources: until that
-- heartbeat lands, the agent-reported cpu_cores_available /
-- memory_available_mib are stale.
-- A second placement decision in that window reads the unsubtracted
-- numbers and can over-allocate even though the advisory lock
-- (store.LockKeyPlacement) serializes the decisions.
--
-- This view recomputes "effective" availability by subtracting VMs
-- pinned after the node's last heartbeat from the raw heartbeat-
-- reported availability. Filter:
--
--   - vms.pinned_node_id = n.id        — pinned by the scheduler to this node
--   - vms.deleted_at IS NULL           — not soft-deleted
--   - vms.desired_phase != 'deleted'   — operator intent is not "tear down"
--                                         (matches CountRunningVMsByNode)
--   - n.last_heartbeat_at IS NULL
--     OR vms.created_at > n.last_heartbeat_at
--                                       — agent has not yet observed the VM
--
-- Self-correcting: once the next heartbeat lands, vms.created_at <
-- n.last_heartbeat_at and the VM drops out of the LATERAL aggregate;
-- the agent's report now reflects it, so no double-counting.
--
-- Edge cases:
--   - Raw column NULL (node has never heartbeat'd, or heartbeat
--     omitted a metric column): effective stays NULL, preserving the
--     scheduler's "fall back to count-based scoring" path for that
--     node — partial nils stay treated as missing metrics.
--   - Sum(pending) > raw available: floor at zero via GREATEST.
--   - Soft-deleted nodes: passed through; callers (ListEligiblePools‑
--     ByName) filter on deleted_at IS NULL same as before.
--
-- Trade-off: a VM committed within the agent's heartbeat round-trip
-- (~100ms) can be under-subtract — agent's snapshot predates the VM but
-- last_heartbeat_at is stamped on receipt (after the commit). Window
-- closes on the next heartbeat. Tightening requires agent-side
-- observed_at tracking; future iteration.
create view node_effective_availability as
select
    n.id,
    n.name,
    n.architecture,
    n.advertised_endpoint,
    n.migration_host,
    n.migration_port_range_start,
    n.migration_port_range_end,
    n.status,
    n.cordoned_at,
    n.cpu_cores_total,
    n.cpu_cores_available,
    n.cpu_model,
    n.cpu_flags,
    n.memory_total_mib,
    n.memory_available_mib,
    n.hugepages_2mib_total,
    n.hugepages_1gib_total,
    n.kernel_version,
    n.qemu_version,
    n.numa_topology,
    n.capabilities,
    n.last_heartbeat_at,
    n.agent_version,
    n.labels,
    n.memory_pressure_since,
    n.memory_pressure_count,
    n.system_disk_total_bytes,
    n.system_disk_available_bytes,
    n.system_disk_pressure_since,
    n.system_disk_pressure_count,
    n.created_at,
    n.updated_at,
    n.deleted_at,
    case
        when n.cpu_cores_available is null then null
        else greatest(0, n.cpu_cores_available - coalesce(pending.cpu_cores, 0))::int
    end as cpu_cores_effective,
    case
        when n.memory_available_mib is null then null
        else greatest(0, n.memory_available_mib - coalesce(pending.memory_mib, 0))::bigint
    end as memory_effective_mib
from nodes n
left join lateral (
    select
        sum(vms.cpu_cores)::int     as cpu_cores,
        sum(vms.memory_mib)::bigint as memory_mib
    from vms
    where vms.pinned_node_id = n.id
      and vms.deleted_at is null
      and vms.desired_phase != 'deleted'
      and (n.last_heartbeat_at is null or vms.created_at > n.last_heartbeat_at)
) pending on true;

comment on view node_effective_availability is
    'Node availability with pending-VM accounting. Subtracts vms.cpu_cores / vms.memory_mib '
    'pinned after last heartbeat (CP committed but agent has not yet observed). '
    'Self-correcting once next heartbeat arrives. Read by the scheduler at placement time.';

-- ============ vm_disks ============
create table vm_disks (
  id                  uuid primary key default uuid_generate_v7(),
  vm_id               uuid not null references vms(id) on delete cascade,
  storage_pool_id     uuid not null references storage_pools(id) on delete restrict,
  device_order        int not null,
  bus                 disk_bus not null default 'virtio',
  size_gib            int not null,
  source_kind         text not null,
  source_template_id  uuid references templates(id) on delete restrict,
  format              image_format not null default 'qcow2',
  read_only           boolean not null default false,
  cache_mode          disk_cache_mode not null default 'writeback',
  discard             disk_discard not null default 'unmap',
  boot_order          int,
  generation          bigint not null default 1,
  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),
  deleted_at          timestamptz,
  constraint chk_vm_disks_size       check (size_gib between 1 and 65536),
  constraint chk_vm_disks_source     check (source_kind in ('template', 'blank')),
  constraint chk_vm_disks_template_required check (
    (source_kind = 'template' and source_template_id is not null) or
    (source_kind = 'blank' and source_template_id is null)
  ),
  constraint chk_vm_disks_boot_order check (boot_order is null or boot_order >= 0)
);
create unique index uq_vm_disks_order on vm_disks(vm_id, device_order) where deleted_at is null;
create index idx_vm_disks_pool on vm_disks(storage_pool_id) where deleted_at is null;
create trigger vm_disks_set_updated_at before update on vm_disks
  for each row execute function trg_set_updated_at();
create trigger vm_disks_bump_generation before update on vm_disks
  for each row execute function trg_bump_generation(
    'size_gib', 'bus', 'device_order', 'format', 'read_only',
    'cache_mode', 'discard', 'boot_order'
  );

-- ============ pool_effective_capacity (view) ============
-- Disk-dimension parallel to node_effective_availability (above):
-- subtracts VM disks committed since the pool's last scan from the
-- agent-reported availability. Closes a race analogous to pinned-but-
-- not-running VMs, just at storage layer. Read by Sub-iteration C of
-- the disk-aware scheduler; operator-visible on the pool
-- API + CLI immediately so divergence is observable.
--
-- Filter:
--   - vd.storage_pool_id = p.id        — disk pinned to this pool
--   - vd.deleted_at IS NULL            — not soft-deleted. vm_disks
--                                         lifecycle is independent of
--                                         vms.desired_phase per
--                                         SoftDeleteVMDisksByVM —
--                                         disk space remains committed
--                                         until the row itself is
--                                         stamped deleted_at, regardless
--                                         of operator-intent phase.
--   - p.reported_at IS NULL OR
--     vd.created_at > p.reported_at   — agent's scan has not yet
--                                         observed the disk. reported_at
--                                         stamps on UpsertStoragePool‑
--                                         Usage (Sub-iteration A
--                                         periodic worker bumps it
--                                         every scan).
--
-- Self-correcting: once the next scan lands, p.reported_at advances
-- past vd.created_at and the disk drops out of the LATERAL aggregate;
-- the agent's report now reflects it, so no double-counting.
--
-- Edge cases:
--   - Raw available_bytes NULL (pool has never been scanned, or scan
--     ran but agent reported no usage): effective stays NULL through
--     the CASE, preserving the future scheduler fallback path. Partial
--     nils stay treated as "no signal".
--   - Sum(pending) > raw available: floor at zero via GREATEST so the
--     operator sees "0 free" rather than negative bytes.
--   - size_gib INT × 1 GiB per row. Cast to bigint before the multiply
--     so the per-row product cannot overflow int (size_gib max 65536,
--     ×1073741824 = 70 TiB; bigint range is ±9.2 EiB — comfortable
--     headroom). The outer ::bigint pin keeps the column type
--     consistent with available_bytes.
--
-- Trade-off: VM disks committed during the scan's network round-trip
-- can fall on either side of the reported_at stamp — the agent's
-- filesystem snapshot is taken before reported_at lands, so a disk
-- created in that window may already be in the scan's available_bytes
-- AND subtracted here. Effect: one-cycle over-subtraction →
-- over-conservative placement, never
-- over-allocation. Tightening requires agent-side observed_at
-- payload per disk; future iteration if real workloads expose it.
create view pool_effective_capacity as
select
    p.id,
    p.node_id,
    p.name,
    p.type,
    p.path,
    p.capacity_bytes,
    p.available_bytes,
    p.reported_at,
    p.config,
    p.disk_pressure_since,
    p.disk_pressure_count,
    p.reconciliation_status,
    p.last_reconciled_at,
    p.reconciliation_error,
    p.created_at,
    p.updated_at,
    p.deleted_at,
    case
        when p.available_bytes is null then null
        else greatest(0, p.available_bytes - coalesce(pending.committed_bytes, 0))::bigint
    end as available_bytes_effective
from storage_pools p
left join lateral (
    select sum(vd.size_gib::bigint * 1073741824)::bigint as committed_bytes
    from vm_disks vd
    where vd.storage_pool_id = p.id
      and vd.deleted_at is null
      and (p.reported_at is null or vd.created_at > p.reported_at)
) pending on true;

comment on view pool_effective_capacity is
    'Pool availability with pending-VM-disk accounting. Subtracts vm_disks '
    'committed after last scan (CP committed but agent has not yet observed). '
    'Self-correcting once next scan completes. Operator-visible via the pool '
    'API + CLI; future scheduler integration.';

-- ============ vm_nics ============
create table vm_nics (
  id            uuid primary key default uuid_generate_v7(),
  vm_id         uuid not null references vms(id) on delete cascade,
  network_id    uuid not null references networks(id) on delete restrict,
  device_order  int not null,
  model         nic_model not null default 'virtio',
  mac_address   macaddr not null,
  ipv4_address  inet,
  ipv6_address  inet,
  generation    bigint not null default 1,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  deleted_at    timestamptz
);
create unique index uq_vm_nics_order on vm_nics(vm_id, device_order) where deleted_at is null;
create unique index uq_vm_nics_mac on vm_nics(mac_address) where deleted_at is null;
create index idx_vm_nics_network on vm_nics(network_id) where deleted_at is null;
create trigger vm_nics_set_updated_at before update on vm_nics
  for each row execute function trg_set_updated_at();
create trigger vm_nics_bump_generation before update on vm_nics
  for each row execute function trg_bump_generation('model', 'ipv4_address', 'ipv6_address', 'device_order');

-- ============ snapshots ============
create table snapshots (
  id                     uuid primary key default uuid_generate_v7(),
  vm_id                  uuid not null references vms(id) on delete cascade,
  parent_snapshot_id     uuid references snapshots(id) on delete restrict,
  owner_id               uuid not null references users(id) on delete restrict,
  name                   text not null,
  description            text not null default '',
  status                 snapshot_status not null default 'creating',
  with_memory            boolean not null default false,
  vm_state_at_snapshot   vm_state_at_snapshot not null,
  disk_size_bytes        bigint,
  memory_size_bytes      bigint,
  error_message          text,
  created_at             timestamptz not null default now(),
  updated_at             timestamptz not null default now(),
  deleted_at             timestamptz,
  -- with_memory implies a non-null memory_size_bytes once the snapshot
  -- reaches 'ready'; we don't enforce the join with status because
  -- size discovery may legitimately lag the status transition.
  constraint chk_snapshots_memory_consistency check (
    with_memory or memory_size_bytes is null
  )
);
create unique index uq_snapshots_vm_name on snapshots(vm_id, name) where deleted_at is null;
create index idx_snapshots_parent on snapshots(parent_snapshot_id) where deleted_at is null;
create index idx_snapshots_vm on snapshots(vm_id) where deleted_at is null;
create index idx_snapshots_owner on snapshots(owner_id) where deleted_at is null;
create trigger snapshots_set_updated_at before update on snapshots
  for each row execute function trg_set_updated_at();

-- ============ enums (runtime) ============
-- 'orphaned': set on vm_runtime.phase when the VM's owning node is
-- force-deleted. Surfaces "node is gone, manual remediation required"
-- distinct from the VM itself reaching its terminal 'gone' state.
create type vm_phase as enum ('pending', 'running', 'paused', 'stopped', 'error', 'gone', 'orphaned');

-- ============ vm_runtime ============
create table vm_runtime (
  vm_id                uuid primary key references vms(id) on delete cascade,
  current_node_id      uuid references nodes(id) on delete set null,
  phase                vm_phase not null default 'pending',
  conditions           jsonb not null default '[]'::jsonb,
  observed_generation  bigint not null default 0,
  qemu_pid             int,
  qmp_socket_path      text,
  vnc_port             int,
  spice_port           int,
  serial_console_path  text,
  guest_os             text,
  guest_hostname       text,
  guest_ip_addresses   inet[],
  cpu_usage_percent    real,
  memory_used_mib      bigint,
  last_started_at      timestamptz,
  last_stopped_at      timestamptz,
  last_error_at        timestamptz,
  last_error_message   text,
  last_observed_at     timestamptz,
  updated_at           timestamptz not null default now()
);
create index idx_vm_runtime_node on vm_runtime(current_node_id) where phase != 'gone';
create index idx_vm_runtime_phase on vm_runtime(phase);
create trigger vm_runtime_set_updated_at before update on vm_runtime
  for each row execute function trg_set_updated_at();

-- ============ vm_disk_runtime ============
create table vm_disk_runtime (
  vm_disk_id           uuid primary key references vm_disks(id) on delete cascade,
  current_node_id      uuid references nodes(id) on delete set null,
  storage_pool_id      uuid references storage_pools(id) on delete set null,
  file_path            text,
  actual_size_bytes    bigint,
  virtual_size_bytes   bigint,
  observed_generation  bigint not null default 0,
  last_observed_at     timestamptz,
  updated_at           timestamptz not null default now()
);
create index idx_vm_disk_runtime_node on vm_disk_runtime(current_node_id);
create trigger vm_disk_runtime_set_updated_at before update on vm_disk_runtime
  for each row execute function trg_set_updated_at();

-- ============ vm_nic_runtime ============
create table vm_nic_runtime (
  vm_nic_id            uuid primary key references vm_nics(id) on delete cascade,
  current_node_id      uuid references nodes(id) on delete set null,
  tap_device           text,
  observed_generation  bigint not null default 0,
  last_observed_at     timestamptz,
  updated_at           timestamptz not null default now()
);
create index idx_vm_nic_runtime_node on vm_nic_runtime(current_node_id);
create trigger vm_nic_runtime_set_updated_at before update on vm_nic_runtime
  for each row execute function trg_set_updated_at();

-- ============ enums (operations) ============
-- migration_phase mirrors QEMU's QMP migration status vocabulary so the
-- agent can pass-through what QEMU reports without translation. Mapping:
--   QEMU 'setup'           → 'setup'           (initialization)
--   QEMU 'active'          → 'active'          (precopy in progress)
--   QEMU 'postcopy-active' → 'postcopy_active' (snake_case)
--   QEMU 'completed'       → 'completed'
--   QEMU 'failed'          → 'failed'
--   QEMU 'cancelled'       → 'cancelled'
-- The agent must convert the dashed QEMU value to snake_case at the boundary.
create type migration_phase as enum ('setup', 'active', 'postcopy_active', 'completed', 'failed', 'cancelled');
create type migration_reason as enum ('manual', 'drain', 'rebalance', 'evacuation');

-- ============ migrations ============
-- source_node_id and target_node_id are nullable + ON DELETE SET NULL.
-- Active migrations involving a deleted node must still be cancelled by
-- the node-delete handler (status = 'cancelled' with an error_message
-- naming the cause) BEFORE the DELETE; under normal operation only
-- historical (completed/failed/cancelled) rows ever have their FK
-- NULLed. SET NULL is the safety net, not the primary mechanism.
create table migrations (
  id                   uuid primary key default uuid_generate_v7(),
  vm_id                uuid not null references vms(id) on delete cascade,
  source_node_id       uuid references nodes(id) on delete set null,
  target_node_id       uuid references nodes(id) on delete set null,
  initiated_by_user_id uuid references users(id) on delete set null,
  reason               migration_reason not null,
  phase                migration_phase not null default 'setup',
  progress_percent     smallint not null default 0,
  bytes_total          bigint,
  bytes_transferred    bigint not null default 0,
  bytes_remaining      bigint,
  pages_total          bigint,
  pages_transferred    bigint,
  downtime_ms          int,
  setup_time_ms        int,
  qmp_capabilities     jsonb not null default '{}'::jsonb,
  max_bandwidth_bytes  bigint,
  max_downtime_ms      int,
  started_at           timestamptz,
  completed_at         timestamptz,
  error_message        text,
  created_at           timestamptz not null default now(),
  updated_at           timestamptz not null default now(),
  constraint chk_migrations_progress       check (progress_percent between 0 and 100),
  constraint chk_migrations_distinct_nodes check (source_node_id is null or target_node_id is null or source_node_id <> target_node_id)
);
create unique index uq_migrations_active_vm on migrations(vm_id)
  where phase not in ('completed', 'failed', 'cancelled');
create index idx_migrations_target_active on migrations(target_node_id)
  where phase not in ('completed', 'failed', 'cancelled');
create index idx_migrations_source_active on migrations(source_node_id)
  where phase not in ('completed', 'failed', 'cancelled');
create index idx_migrations_vm_history on migrations(vm_id, created_at desc);
create trigger migrations_set_updated_at before update on migrations
  for each row execute function trg_set_updated_at();

-- ============ idempotency_keys ============
create table idempotency_keys (
  key              text primary key,
  user_id          uuid references users(id) on delete set null,
  request_method   text not null,
  request_path     text not null,
  request_hash     bytea not null,
  response_status  int,
  response_headers jsonb,
  response_body    bytea,
  state            text not null default 'in_flight',
  created_at       timestamptz not null default now(),
  completed_at     timestamptz,
  expires_at       timestamptz not null,
  constraint chk_idempotency_state check (state in ('in_flight', 'completed'))
);
create index idx_idempotency_keys_expires on idempotency_keys(expires_at);

-- ============ tasks ============
-- Tasks resource — first-class user-facing surface for async operations.
--
-- The table mirrors the public OpenAPI Task schema verbatim. river_job_id
-- is a *weak reference* to river_job.id (no FK, by design): the
-- transactional-enqueue guarantee removes the need for DB-level
-- integrity, and skipping the FK keeps tasks immune to river schema
-- churn.
create type task_status as enum (
    'pending', 'running', 'success', 'failed', 'cancelled'
);

create table tasks (
    id              uuid primary key default uuid_generate_v7(),
    type            text         not null,
    status          task_status  not null default 'pending',
    resource_type   text         not null,
    resource_id     uuid,
    progress        smallint,
    args            jsonb        not null default '{}'::jsonb,
    result          jsonb,
    error           jsonb,
    attempts        integer      not null default 0,
    max_attempts    integer      not null default 25,
    river_job_id    bigint,
    -- Set by insert-driven agent-saga workers after the agent's 202
    -- response. Persisting the agent task id makes a CP-side restart
    -- resume polling the existing agent task instead of issuing a
    -- duplicate POST. storage_pool.scan does NOT populate this column
    -- (scans are short and do not commit blobs to disk);
    -- storage_image.import is the first consumer. Nullable so legacy
    -- task types stay clean.
    agent_task_id   uuid,
    created_by      uuid references users(id) on delete set null,
    created_at      timestamptz  not null default now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    constraint chk_tasks_progress
        check (progress is null or (progress between 0 and 100))
);

-- Listing by status; default ordering is created_at desc.
create index idx_tasks_status_created_at
    on tasks (status, created_at desc);

-- Hot path: "any active task of type X?" — checked before issuing
-- duplicate scans. Partial keeps the index small.
create index idx_tasks_active_by_type
    on tasks (type)
    where status in ('pending', 'running');

-- tasks.list?resource_type=X&resource_id=Y
create index idx_tasks_resource
    on tasks (resource_type, resource_id);

-- tasks.cleanup periodic worker scans finalized rows past retention.
create index idx_tasks_finished_at
    on tasks (finished_at)
    where finished_at is not null;

-- RBAC-scoped listing for developer / viewer.
create index idx_tasks_created_by_created_at
    on tasks (created_by, created_at desc);

-- ============ audit_log (partitioned by month) ============
-- Append-only audit table. Partitioned by created_at (monthly) so old
-- months can be detached / archived independently. No FKs by design —
-- audit must survive the deletion of the entities it logs, and the
-- write path is hot.
create table audit_log (
  id              uuid not null default uuid_generate_v7(),
  actor_user_id   uuid,
  actor_token_id  uuid,
  actor_kind      text not null,
  action          text not null,
  resource_type   text,
  resource_id     uuid,
  request_id      uuid,
  ip_addr         inet,
  user_agent      text,
  before          jsonb,
  after           jsonb,
  metadata        jsonb,
  created_at      timestamptz not null default now(),
  primary key (created_at, id),
  constraint chk_audit_actor_kind
    check (actor_kind in ('user', 'agent', 'system', 'scheduler', 'reconciler'))
) partition by range (created_at);

-- Partitioned indexes declared on the root automatically propagate to children.
create index idx_audit_log_resource      on audit_log(resource_type, resource_id, created_at desc);
create index idx_audit_log_actor_user    on audit_log(actor_user_id, created_at desc);
create index idx_audit_log_action        on audit_log(action, created_at desc);

-- Initial monthly partitions: current month + next two. The recurring
-- partition-creation job will keep current+2 always covered.
create table audit_log_y2026_m05 partition of audit_log
  for values from ('2026-05-01 00:00:00+00') to ('2026-06-01 00:00:00+00');
create table audit_log_y2026_m06 partition of audit_log
  for values from ('2026-06-01 00:00:00+00') to ('2026-07-01 00:00:00+00');
create table audit_log_y2026_m07 partition of audit_log
  for values from ('2026-07-01 00:00:00+00') to ('2026-08-01 00:00:00+00');
-- Safety net: rows whose created_at falls outside the seeded monthly partitions
-- land here. The auto-partition job (out of scope for this migration) will create
-- proper monthly partitions ahead of time; until it ships, this DEFAULT prevents
-- audit writes from failing.
create table audit_log_default partition of audit_log default;

-- ============ river queue (UPSTREAM-SYNCED) ============
-- Embedded verbatim from riverqueue/river migrations 001..006 at v0.35.1.
--
-- DO NOT edit these tables/indexes/functions by hand. To upgrade river:
--   1) bump go.mod to a new river version
--   2) read CHANGELOG and migration files at:
--      https://github.com/riverqueue/river/tree/v0.35.1/riverdriver/riverpgxv5/internal/migration
--   3) add ONLY the delta as a new goose file, e.g.,
--      migrations/00003_river_v0.36.0.up.sql — do not modify this block.
--
-- After this block we INSERT into river_migration to mark these versions as
-- applied so rivermigrate.Migrator does not try to re-run them.
--
-- Source last synced: 2026-05-02.
-- Pinned versions:
--   github.com/riverqueue/river                        v0.35.1
--   github.com/riverqueue/river/riverdriver/riverpgxv5 v0.35.1
--   github.com/riverqueue/river/rivermigrate           v0.35.1 (subpackage of river)

-- ---- BEGIN VERBATIM PASTE FROM UPSTREAM ----

-- 001_create_river_migration.up.sql
CREATE TABLE river_migration(
  id bigserial PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT NOW(),
  version bigint NOT NULL,
  CONSTRAINT version CHECK (version >= 1)
);

CREATE UNIQUE INDEX ON river_migration USING btree(version);

-- 002_initial_schema.up.sql
CREATE TYPE river_job_state AS ENUM(
  'available',
  'cancelled',
  'completed',
  'discarded',
  'retryable',
  'running',
  'scheduled'
);

CREATE TABLE river_job(
  -- 8 bytes
  id bigserial PRIMARY KEY,

  -- 8 bytes (4 bytes + 2 bytes + 2 bytes)
  --
  -- `state` is kept near the top of the table for operator convenience -- when
  -- looking at jobs with `SELECT *` it'll appear first after ID. The other two
  -- fields aren't as important but are kept adjacent to `state` for alignment
  -- to get an 8-byte block.
  state river_job_state NOT NULL DEFAULT 'available',
  attempt smallint NOT NULL DEFAULT 0,
  max_attempts smallint NOT NULL,

  -- 8 bytes each (no alignment needed)
  attempted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT NOW(),
  finalized_at timestamptz,
  scheduled_at timestamptz NOT NULL DEFAULT NOW(),

  -- 2 bytes (some wasted padding probably)
  priority smallint NOT NULL DEFAULT 1,

  -- types stored out-of-band
  args jsonb,
  attempted_by text[],
  errors jsonb[],
  kind text NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}',
  queue text NOT NULL DEFAULT 'default',
  tags varchar(255)[],

  CONSTRAINT finalized_or_finalized_at_null CHECK ((state IN ('cancelled', 'completed', 'discarded') AND finalized_at IS NOT NULL) OR finalized_at IS NULL),
  CONSTRAINT max_attempts_is_positive CHECK (max_attempts > 0),
  CONSTRAINT priority_in_range CHECK (priority >= 1 AND priority <= 4),
  CONSTRAINT queue_length CHECK (char_length(queue) > 0 AND char_length(queue) < 128),
  CONSTRAINT kind_length CHECK (char_length(kind) > 0 AND char_length(kind) < 128)
);

-- We may want to consider adding another property here after `kind` if it seems
-- like it'd be useful for something.
CREATE INDEX river_job_kind ON river_job USING btree(kind);

CREATE INDEX river_job_state_and_finalized_at_index ON river_job USING btree(state, finalized_at) WHERE finalized_at IS NOT NULL;

CREATE INDEX river_job_prioritized_fetching_index ON river_job USING btree(state, queue, priority, scheduled_at, id);

CREATE INDEX river_job_args_index ON river_job USING GIN(args);

CREATE INDEX river_job_metadata_index ON river_job USING GIN(metadata);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION river_job_notify()
  RETURNS TRIGGER
  AS $$
DECLARE
  payload json;
BEGIN
  IF NEW.state = 'available' THEN
    -- Notify will coalesce duplicate notifications within a transaction, so
    -- keep these payloads generalized:
    payload = json_build_object('queue', NEW.queue);
    PERFORM
      pg_notify('river_insert', payload::text);
  END IF;
  RETURN NULL;
END;
$$
LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER river_notify
  AFTER INSERT ON river_job
  FOR EACH ROW
  EXECUTE PROCEDURE river_job_notify();

CREATE UNLOGGED TABLE river_leader(
    -- 8 bytes each (no alignment needed)
    elected_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,

    -- types stored out-of-band
    leader_id text NOT NULL,
    name text PRIMARY KEY,

    CONSTRAINT name_length CHECK (char_length(name) > 0 AND char_length(name) < 128),
    CONSTRAINT leader_id_length CHECK (char_length(leader_id) > 0 AND char_length(leader_id) < 128)
);

-- 003_river_job_tags_non_null.up.sql
ALTER TABLE river_job ALTER COLUMN tags SET DEFAULT '{}';
UPDATE river_job SET tags = '{}' WHERE tags IS NULL;
ALTER TABLE river_job ALTER COLUMN tags SET NOT NULL;

-- 004_pending_and_more.up.sql
-- The args column never had a NOT NULL constraint or default value at the
-- database level, though we tried to ensure one at the application level.
ALTER TABLE river_job ALTER COLUMN args SET DEFAULT '{}';
UPDATE river_job SET args = '{}' WHERE args IS NULL;
ALTER TABLE river_job ALTER COLUMN args SET NOT NULL;
ALTER TABLE river_job ALTER COLUMN args DROP DEFAULT;

-- The metadata column never had a NOT NULL constraint or default value at the
-- database level, though we tried to ensure one at the application level.
ALTER TABLE river_job ALTER COLUMN metadata SET DEFAULT '{}';
UPDATE river_job SET metadata = '{}' WHERE metadata IS NULL;
ALTER TABLE river_job ALTER COLUMN metadata SET NOT NULL;

-- The 'pending' job state will be used for upcoming functionality:
ALTER TYPE river_job_state ADD VALUE IF NOT EXISTS 'pending' AFTER 'discarded';

ALTER TABLE river_job DROP CONSTRAINT finalized_or_finalized_at_null;
ALTER TABLE river_job ADD CONSTRAINT finalized_or_finalized_at_null CHECK (
    (finalized_at IS NULL AND state NOT IN ('cancelled', 'completed', 'discarded')) OR
    (finalized_at IS NOT NULL AND state IN ('cancelled', 'completed', 'discarded'))
);

DROP TRIGGER river_notify ON river_job;
DROP FUNCTION river_job_notify;

--
-- Create table `river_queue`.
--

CREATE TABLE river_queue (
    name text PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}' ::jsonb,
    paused_at timestamptz,
    updated_at timestamptz NOT NULL
);

--
-- Alter `river_leader` to add a default value of 'default` to `name`.
--

ALTER TABLE river_leader
    ALTER COLUMN name SET DEFAULT 'default',
    DROP CONSTRAINT name_length,
    ADD CONSTRAINT name_length CHECK (name = 'default');

-- 005_migration_unique_client.up.sql
--
-- Rebuild the migration table so it's based on `(line, version)`.
--
-- +goose StatementBegin
DO
$body$
BEGIN
    -- Tolerate users who may be using their own migration system rather than
    -- River's. If they are, they will have skipped version 001 containing
    -- `CREATE TABLE river_migration`, so this table won't exist.
    IF (SELECT to_regclass('river_migration') IS NOT NULL) THEN
        ALTER TABLE river_migration
            RENAME TO river_migration_old;

        CREATE TABLE river_migration(
            line TEXT NOT NULL,
            version bigint NOT NULL,
            created_at timestamptz NOT NULL DEFAULT NOW(),
            CONSTRAINT line_length CHECK (char_length(line) > 0 AND char_length(line) < 128),
            CONSTRAINT version_gte_1 CHECK (version >= 1),
            PRIMARY KEY (line, version)
        );

        INSERT INTO river_migration
            (created_at, line, version)
        SELECT created_at, 'main', version
        FROM river_migration_old;

        DROP TABLE river_migration_old;
    END IF;
END;
$body$
LANGUAGE 'plpgsql';
-- +goose StatementEnd

--
-- Add `river_job.unique_key` and bring up an index on it.
--

-- These statements use `IF NOT EXISTS` to allow users with a `river_job` table
-- of non-trivial size to build the index `CONCURRENTLY` out of band of this
-- migration, then follow by completing the migration.
ALTER TABLE river_job
    ADD COLUMN IF NOT EXISTS unique_key bytea;

CREATE UNIQUE INDEX IF NOT EXISTS river_job_kind_unique_key_idx ON river_job (kind, unique_key) WHERE unique_key IS NOT NULL;

--
-- Create `river_client` and derivative.
--
-- This feature hasn't quite yet been implemented, but we're taking advantage of
-- the migration to add the schema early so that we can add it later without an
-- additional migration.
--

CREATE UNLOGGED TABLE river_client (
    id text PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}',
    paused_at timestamptz,
    updated_at timestamptz NOT NULL,
    CONSTRAINT name_length CHECK (char_length(id) > 0 AND char_length(id) < 128)
);

-- Differs from `river_queue` in that it tracks the queue state for a particular
-- active client.
CREATE UNLOGGED TABLE river_client_queue (
    river_client_id text NOT NULL REFERENCES river_client (id) ON DELETE CASCADE,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    max_workers bigint NOT NULL DEFAULT 0,
    metadata jsonb NOT NULL DEFAULT '{}',
    num_jobs_completed bigint NOT NULL DEFAULT 0,
    num_jobs_running bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (river_client_id, name),
    CONSTRAINT name_length CHECK (char_length(name) > 0 AND char_length(name) < 128),
    CONSTRAINT num_jobs_completed_zero_or_positive CHECK (num_jobs_completed >= 0),
    CONSTRAINT num_jobs_running_zero_or_positive CHECK (num_jobs_running >= 0)
);

-- 006_bulk_unique.up.sql
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION river_job_state_in_bitmask(bitmask BIT(8), state river_job_state)
RETURNS boolean
LANGUAGE SQL
IMMUTABLE
AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;
-- +goose StatementEnd

--
-- Add `river_job.unique_states` and bring up an index on it.
--
-- This column may exist already if users manually created the column and index
-- as instructed in the changelog so the index could be created `CONCURRENTLY`.
--
ALTER TABLE river_job ADD COLUMN IF NOT EXISTS unique_states BIT(8);

-- This statement uses `IF NOT EXISTS` to allow users with a `river_job` table
-- of non-trivial size to build the index `CONCURRENTLY` out of band of this
-- migration, then follow by completing the migration.
CREATE UNIQUE INDEX IF NOT EXISTS river_job_unique_idx ON river_job (unique_key)
    WHERE unique_key IS NOT NULL
      AND unique_states IS NOT NULL
      AND river_job_state_in_bitmask(unique_states, state);

-- Remove the old unique index. Users who are actively using the unique jobs
-- feature and who wish to avoid deploy downtime may want od drop this in a
-- subsequent migration once all jobs using the old unique system have been
-- completed (i.e. no more rows with non-null unique_key and null
-- unique_states).
DROP INDEX river_job_kind_unique_key_idx;

-- ---- END VERBATIM PASTE FROM UPSTREAM ----

-- Mark all upstream-bundled versions as applied so rivermigrate.Migrator
-- does not try to re-run them (versions 1..6 embedded above).
INSERT INTO river_migration (line, version)
  SELECT 'main', v FROM generate_series(1, 6) AS v;

-- +goose Down
-- Honest rollback: drop and recreate public schema, then recreate
-- goose's bookkeeping table so the migrate runner can finalize the down
-- step (it INSERTs into goose_db_version after the SQL runs).
drop schema if exists public cascade;
create schema public;
grant all on schema public to public;

create table if not exists goose_db_version (
  id          serial primary key,
  version_id  bigint not null,
  is_applied  boolean not null,
  tstamp      timestamp default now()
);

-- Seed the sentinel row goose expects: version 0, applied = true.
-- Without this row EnsureDBVersion returns ErrNoNextVersion and goose
-- cannot run any subsequent "up" migration.
insert into goose_db_version (version_id, is_applied) values (0, true);
