# RBAC Model

Otherix uses **fixed roles defined in code** with permissions described as a
matrix of `(role, permission, scope)` tuples. There are no custom roles
and no dynamic permission editing through the API.

## Roles

There are exactly four roles. A user has exactly one. The role is stored
in `users.role` and carried in the JWT `role` claim.

- **`admin`** — full system access including users, nodes, firmware,
  storage pools, and RBAC. Sees everything.
- **`operator`** — full virtualization management (VMs, templates,
  networks, the public template catalogue, node maintenance), but **not**
  user/RBAC management, not node lifecycle (create/delete), not firmware
  registration, not storage-pool lifecycle.
- **`developer`** — manages **own** VMs and **own** snapshots, creates
  **private** templates, reads infrastructure for context. Cannot
  publish templates, cannot operate other users' VMs, cannot migrate.
- **`viewer`** — read-only access to visible resources. No mutation,
  no console access, no own resource creation.

## Scopes

Permissions for resources that carry an `owner_id` (`vms`, `templates`,
`snapshots`) come in two scopes:

- **`own`** — the resource's `owner_id` must equal the requesting
  user's `id`.
- **`any`** — all matching resources, regardless of ownership.

For resources without `owner_id` (`nodes`, `networks`, `storage_pools`,
`firmwares`), only one scope makes sense and the matrix uses **yes / —**
to mean "permission held / not held".

## Templates and visibility

Templates have a `visibility` field — `public` or `private` (default
`private`). Visibility controls who can list and use the template:

- `public` templates are listed and usable for VM creation by every
  authenticated user.
- `private` templates are listed and usable only by the owner and by
  `admin` / `operator`.

Visibility is changed only through the dedicated
`POST /v1/templates/{id}/set-visibility` endpoint, which requires the
`template:set_visibility` permission held by `admin` and `operator`.
The author (`templates.owner_id`) **does not change** when visibility
changes — publishing a private template to public does not transfer
ownership. Admins and operators moderate the public catalogue through
their role permissions, not through ownership transfer. There is no
`created_by` or `published_by` column.

A `developer` cannot publish their own template. They can, however,
delete it, hand-off the workflow to an operator (out-of-band), or
clone it.

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

### Templates

| Permission                | admin | operator | developer | viewer |
|---------------------------|-------|----------|-----------|--------|
| `template:read:public`    | yes   | yes      | yes       | yes    |
| `template:read`           | any   | any      | own       | —      |
| `template:create`         | yes   | yes      | yes       | —      |
| `template:update`         | any   | any      | own       | —      |
| `template:delete`         | any   | any      | own       | —      |
| `template:set_visibility` | yes   | yes      | —         | —      |
| `template:use`            | any   | any      | own + public | own + public |

`template:create` is unscoped: visibility is fixed to `private` at create
time and cannot be set in the body. `template:set_visibility` (admin /
operator) is the only path to flip visibility, so the previous
`template:create:{public,private}` split — which encoded "may you create
a public template?" — is no longer needed.

`template:read:public` is the catalogue-browse permission held by every
role. `template:read` (with scope) controls whether **private**
templates are visible: developers see their own; admin/operator see all.

`template:use` is what `vm:create` checks against the source template:
admin/operator may create from any template; developer/viewer may
create from public templates and their own private ones (`viewer` has
no `vm:create`, so this only matters for completeness — viewers cannot
actually create a VM).

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

`image_cache:read` covers both firmware-catalogue listings and the
storage-images read endpoints (`GET /v1/storage-pools/{pool_id}/images`,
`GET /v1/storage-pools/{pool_id}/images/{image_id}`); both shapes
expose per-pool image cache state, so they share the read permission.

### Storage images

`storage_images` is the per-pool projection of a template's
content (the junction created when an image is imported into a
pool). The mutator scopes mirror `template:delete`: admin /
operator hold both at `any`, developer at `own`. The "own" scope
reduces to "the image's owning template's `owner_id` ==
caller.id"; handlers also apply a public-bypass branch
(`template:read:public` AND `template.visibility = 'public'`) as
a composite check, not a Scope value (parallels `template:use`).

| Permission              | admin | operator | developer | viewer |
|-------------------------|-------|----------|-----------|--------|
| `storage_image:import`  | any   | any      | own       | —      |
| `storage_image:manage`  | any   | any      | own       | —      |

`storage_image:import` gates `POST /v1/templates/{template_id}/images`.
`storage_image:manage` gates `DELETE
/v1/storage-pools/{pool_id}/images/{image_id}`. Reads use
`image_cache:read` (held by every authenticated role).

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
`cluster_settings` singleton; future: default-template, default-network,
…) sit on `/v1/cluster/*`. Reads are open к every authenticated role
because the operator-facing context (e.g. "which pool defaults?") is
not a secret; mutations are admin-only by precedent with other
cluster-shaping permissions (`storage_pool:manage`, `node:manage`).

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
