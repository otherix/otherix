# RBAC Model

Otherix uses **fixed roles defined in code** with permissions described as a
matrix of `(role, permission, scope)` tuples. There are no custom roles
and no dynamic permission editing through the API.

## Roles

There are exactly four roles. A user has exactly one. The role is stored
in `users.role` and carried in the JWT `role` claim.

- **`admin`** — full system access including users, nodes, firmware,
  storage pools, and RBAC. Sees everything.
- **`operator`** — full virtualization management (VMs, networks, node
  maintenance), but **not** user/RBAC management, not node lifecycle
  (create/delete), not firmware registration, not storage-pool lifecycle.
- **`developer`** — manages **own** VMs and **own** snapshots, reads
  infrastructure for context. Cannot operate other users' VMs, cannot
  migrate.
- **`viewer`** — read-only access to visible resources. No mutation,
  no console access, no own resource creation.

## Scopes

Permissions for resources that carry an `owner_id` (`vms`, `snapshots`)
come in two scopes:

- **`own`** — the resource's `owner_id` must equal the requesting
  user's `id`.
- **`any`** — all matching resources, regardless of ownership.

For resources without `owner_id` (`nodes`, `networks`, `storage_pools`,
`firmwares`), only one scope makes sense and the matrix uses **yes / —**
to mean "permission held / not held".

## Image use

There is no template entity and no per-image authorization. A VM is
created directly from an image URL the caller supplies; the agent
fetches it. `vm:create` is the single gate for materializing an image
into a VM — any role that holds `vm:create` may create a VM from any
image URL. Read access to the node-side image cache is gated separately
by `image_cache:read`.

## Permissions matrix

Each table covers one resource group. Cell values:

- `any` — permission with `any` scope.
- `own` — permission with `own` scope (resource's `owner_id` must
  match caller).
- `yes` — permission held (no scope dimension).
- `—` — permission **not** held by this role.

### Virtual machines

Every entry is scoped against `vms.owner_id`.

| Permission   | admin | operator | developer | viewer |
|--------------|-------|----------|-----------|--------|
| `vm:read`    | any   | any      | any       | any    |
| `vm:create`  | yes   | yes      | yes       | —      |
| `vm:update`  | any   | any      | own       | —      |
| `vm:delete`  | any   | any      | own       | —      |
| `vm:lifecycle` | any | any      | own       | —      |
| `vm:resize`  | any   | any      | own       | —      |
| `vm:console` | any   | any      | own       | —      |
| `vm:revert`  | any   | any      | own       | —      |
| `vm:migrate` | any   | any      | —         | —      |

`vm:lifecycle` covers start, stop, poweroff, reboot, reset, pause,
resume — every transition between desired phases.

`vm:read` is `any` for every role: VM names and high-level metadata are
not considered confidential within a single self-hosted installation.
Sensitive runtime fields (qemu pid, file paths, raw VNC ports) are
**not** in the public API at all, so role-based filtering on the response
is not the gate.

### Snapshots

Snapshots carry their own `owner_id`, set at creation to the caller.
For `developer`, "own snapshot" means `snapshots.owner_id == user.id`,
which in normal flow coincides with "snapshot of a VM I own".

| Permission        | admin | operator | developer | viewer |
|-------------------|-------|----------|-----------|--------|
| `snapshot:read`   | any   | any      | own       | own    |
| `snapshot:create` | any   | any      | own       | —      |
| `snapshot:delete` | any   | any      | own       | —      |
| `snapshot:revert` | any   | any      | own       | —      |

> **Edge case — transfer of VM ownership.** The current schema does
> not provide a transfer-of-ownership operation; if one is added later,
> `snapshots.owner_id` will diverge from `vms.owner_id`. The matrix
> above evaluates strictly against `snapshots.owner_id`. Revisit when
> transfer ships.

### Networks

Networks have no owner. `manage` covers create / update / delete and is
admin-only — networks are infrastructure (bridges, VLAN tags, MTU)
tightly coupled to host networking, on the same axis as storage pools
and firmware. Operator reads but does not provision.

| Permission        | admin | operator | developer | viewer |
|-------------------|-------|----------|-----------|--------|
| `network:read`    | yes   | yes      | yes       | yes    |
| `network:manage`  | yes   | —        | —         | —      |

### Storage pools

Storage pools have no owner. Pool lifecycle is `admin`-only because it
is tightly coupled to host filesystem layout and capacity planning.

| Permission             | admin | operator | developer | viewer |
|------------------------|-------|----------|-----------|--------|
| `storage_pool:read`    | yes   | yes      | yes       | yes    |
| `storage_pool:manage`  | yes   | —        | —         | —      |
| `storage_pool:scan`    | yes   | yes      | —         | —      |

### Firmwares and node images

| Permission         | admin | operator | developer | viewer |
|--------------------|-------|----------|-----------|--------|
| `firmware:read`    | yes   | yes      | yes       | yes    |
| `firmware:manage`  | yes   | —        | —         | —      |
| `image_cache:read` | yes   | yes      | yes       | yes    |

`image_cache:read` covers the node-side image-cache read endpoints
(per-pool image cache state). It is the only image-related permission:
materializing an image into a VM is gated by `vm:create` (no per-image
authorization), and there is no template entity.

### Nodes

| Permission         | admin | operator | developer | viewer |
|--------------------|-------|----------|-----------|--------|
| `node:read`        | yes (full) | yes (full) | yes (summary) | yes (summary) |
| `node:maintenance` | yes   | yes      | —         | —      |
| `node:manage`      | yes   | —        | —         | —      |

`node:maintenance` covers cordon / uncordon / drain. `node:manage`
covers register (via join-tokens), update, soft-delete. The
`(full)` / `(summary)` distinction reflects which Node schema variant
is returned — see `Node` and `NodeSummary` in `control-plane.yaml`.

### Users and API tokens

| Permission         | admin | operator | developer | viewer |
|--------------------|-------|----------|-----------|--------|
| `user:read`        | yes   | yes      | —         | —      |
| `user:manage`      | yes   | —        | —         | —      |
| `api_token:manage` | any   | own      | own       | own    |

`api_token:manage` is `any` for admin and `own` for every other role.
Every authenticated user may issue and revoke their own personal API
tokens via `/v1/users/me/api-tokens*` and (for admin only) on behalf
of any user via `/v1/users/{id}/api-tokens*`. A non-admin caller that
targets another user's `{id}` receives `404 not_found` rather than
`403`, to avoid leaking which user ids exist.

`user:read` is **not** held by `developer`/`viewer`: the user
directory is administrative information.

### Tasks

`tasks` is a contract surface over river jobs and ad-hoc operation
tracking; the "owner" of a task is the user that initiated it.

| Permission     | admin | operator | developer | viewer |
|----------------|-------|----------|-----------|--------|
| `task:read`    | any   | any      | own       | own    |
| `task:cancel`  | any   | any      | own       | —      |

### Cluster configuration

Cluster-level settings (today: default-pool reference held in the
`cluster_settings` singleton; future: default-network, …) sit on
`/v1/cluster/*`. Reads are open to every authenticated role
because the operator-facing context (e.g. "which pool defaults?") is
not a secret; mutations are admin-only by precedent with other
cluster-shaping permissions (`storage_pool:manage`, `node:manage`).

`cluster:manage` also governs etcd cluster membership: `GET /v1/cluster/members`
(inspect the current members) and `DELETE /v1/cluster/members/{id}` (evict a
stale or failed member) are admin-only under this same permission.

| Permission         | admin | operator | developer | viewer |
|--------------------|-------|----------|-----------|--------|
| `cluster:read`     | any   | any      | any       | any    |
| `cluster:manage`   | any   | —        | —         | —      |

## Implementation

This document is the human-readable contract; runtime enforcement
lives in code:

- The four roles are an enum in Go (`internal/auth/roles.go`).
- The permissions matrix is a static `map[Role]Permissions` in
  code — no DB-backed roles, no admin-time editing, no migration on
  permission changes.
- The HTTP middleware uses `RequirePermission(p)` to gate handlers.
- Scope checks (`own` vs `any`) happen inside the handler: it loads
  the resource, compares `resource.OwnerID` to the authenticated
  user's `id`, and rejects the request with `403 permission_denied`
  on mismatch.
- The `403` response body uses the standard error envelope
  (`{ error: { code, message, details? } }`); for permission
  failures, `code` is `permission_denied` and `details.required`
  carries the missing permission string for client-side debugging
  and audit log correlation.
- `404 not_found` is preferred over `403` when revealing existence
  itself would leak information.

The middleware and matrix code is **not** part of the
architecture-and-contracts phase; it ships in the Core
Implementation phase together with the rest of the auth stack.
