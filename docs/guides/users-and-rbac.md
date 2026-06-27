# Users and RBAC

Otherix uses **fixed roles defined in code**. There are no custom roles and no
dynamic permission editing through the API. This guide covers the four roles,
how to manage users and API tokens, and the visibility rule that governs 404 vs
403. For the complete permission table, see the [RBAC matrix](../rbac.md).

## The four roles

A user has exactly one role, stored in `users.role` and carried in the JWT
`role` claim:

| Role | What it can do |
| --- | --- |
| `admin` | Full system access, including users, nodes, firmware, storage pools, and networks. Sees everything. |
| `operator` | Full VM and network management on behalf of users; node maintenance (cordon/uncordon). **Not** user management, node lifecycle, firmware, or storage-pool lifecycle. |
| `developer` | Manages **own** VMs and **own** snapshots; reads infrastructure for context. Cannot operate other users' VMs and cannot migrate. |
| `viewer` | Read-only across visible resources. No mutation, no console access. |

## Scopes: own vs any

Permissions on resources that carry an `owner_id` (VMs, snapshots) come in two
scopes:

- **`own`** - the resource's `owner_id` must equal the caller's user id.
- **`any`** - all matching resources, regardless of ownership.

For example a `developer` holds `vm:read` at `any` scope (they can list every
VM's name and metadata) but `vm:lifecycle` and `vm:delete` only at `own` scope
(they can start or delete only their own VMs). Resources without an owner
(networks, storage pools, firmwares, nodes) have a single meaningful scope -
the permission is simply held or not. The full per-permission table is in the
[RBAC matrix](../rbac.md).

## Managing users

The `otherix user` CLI is the primary way to manage users. Every command maps to
the Control Plane REST API under `/v1/users`, which you can also call directly
(see the [CLI reference](../reference/cli.md#otherix-user) and the
[REST API](../reference/api.md)). User management requires an `admin` credential.

A **username** is the account's unique identity and its login credential:
lowercase letters, digits, and interior hyphens (3..32, e.g. `dev-user`).
`email` and `display_name` are optional. Passwords are 12..256 characters (no
composition rules). Valid `role` values are `admin`, `operator`, `developer`,
`viewer`.

### Create a user (admin)

```bash
# Password is read from a no-echo prompt, or piped in with --password-stdin.
otherix user create dev-user --role developer
otherix user create dev-user --role developer --password-stdin <<<"$DEV_PASSWORD"

# Optional contact / label fields:
otherix user create dev-user --role developer \
  --email dev@example.com --display-name "Dev User"
```

The equivalent REST call (`POST /v1/users`, with an admin bearer token - an admin
JWT from `POST /v1/auth/login` or an admin `otx_*` API token; the server selects
the scheme by the `otx_` prefix):

```bash
curl -sS -X POST https://cp.example.com/v1/users \
  -H "Authorization: Bearer $OTHERIX_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "username": "dev-user",
        "password": "correct-horse-battery",
        "role": "developer"
      }'
```

### List, read, update, delete

```bash
otherix user list                          # cursor-paginated
otherix user get dev-user
otherix user set-role dev-user operator    # admin only
otherix user set-password dev-user         # prompts, or --password-stdin
otherix user delete dev-user               # refused while they still own resources
otherix user whoami                        # the calling user (any role)
```

A user can read and update **themselves** (`whoami`, and password via
`set-password` on their own account) - mapped to `GET /v1/users/me` and
`PATCH /v1/users/me`; a user cannot change their own `role`.

!!! tip "Onboarding a new user end to end"
    For a complete walkthrough - an admin creating a developer, that developer
    authenticating their CLI, and launching their first VM - see
    [Onboard a developer](onboarding-a-developer.md).

## API tokens

API tokens are long-lived `otx_*` credentials, an alternative to short-lived
JWTs for CLI and automation. The plaintext is shown exactly once on creation;
the server stores only its SHA-256.

### Via the CLI

`otherix config add cluster` runs a one-time bootstrap: it logs in with a
username and password, creates a long-lived API token named
`otherix-cli-<cluster>` via `POST /v1/users/me/api-tokens`, and stores the
`(server, token)` pair in your CLI config so subsequent commands authenticate
automatically:

```bash
otherix config add cluster \
  --name prod \
  --server https://cp.example.com \
  --login admin
# password prompted interactively, or pass --password / OTHERIX_PASSWORD
```

Re-run with `--force` to replace an existing cluster entry (it best-effort
revokes the previous token server-side first).

Beyond that bootstrap token, manage tokens explicitly with `otherix api-token`:

```bash
otherix api-token create ci-bot --ttl 90d   # plaintext shown once - copy it
otherix api-token list
otherix api-token revoke otx_xxxx
```

`--ttl` sets a relative lifetime (`90d`, `720h`, `30d12h`); omit it for a
long-lived token. An admin can mint, list, or revoke on behalf of another user
by adding `--user <username>`:

```bash
otherix api-token create deploy --user alice --ttl 30d
otherix api-token list --user alice
```

The plaintext is returned only once, at creation; if it leaks or is lost, revoke
it and mint a new one.

### Via the REST API

`POST /v1/users/me/api-tokens` issues a token for the calling user. The `token`
field in the response is the plaintext, returned only this once:

```bash
curl -sS -X POST https://cp.example.com/v1/users/me/api-tokens \
  -H "Authorization: Bearer $OTHERIX_JWT" \
  -H "Content-Type: application/json" \
  -d '{"name": "ci-runner", "expires_at": null}'
# => { "id": "...", "name": "ci-runner", "prefix": "otx_abcd", "token": "otx_...", ... }
```

List your tokens with `GET /v1/users/me/api-tokens` and revoke one with
`DELETE /v1/users/me/api-tokens/{token_id}`. Admins can manage tokens on behalf
of any user through the `/v1/users/{id}/api-tokens` endpoints.

## The 404-not-403 visibility rule

Otherix never leaks the existence of a resource a caller cannot see. When a
resource exists but belongs to another user and the caller's scope is `own`, the
API returns **`404 not_found`**, not `403`. Returning 403 in that case would
reveal that the resource exists.

`403 permission_denied` is reserved for the case where the caller can *see* a
resource but their **role** lacks the capability to act on it - for example a
`developer` viewing their own VM but lacking `vm:migrate`. The error payload's
`details.required_permission` names the missing permission.

In short:

- Resource you should not even know about -> `404 not_found`.
- Resource you can see but cannot act on -> `403 permission_denied`.
