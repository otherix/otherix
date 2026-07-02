# REST API reference

The control plane exposes a versioned REST API under `/v1`. This page covers the
cross-cutting conventions; the full interactive specification lives in the
[REST API browser](../api/index.md) and is generated from
[`api/openapi/control-plane.yaml`](https://github.com/otherix/otherix/blob/main/api/openapi/control-plane.yaml),
the source of truth.

## Base URL and versioning

All endpoints live under `/v1` (for example `http://localhost:8080/v1/vms`).
Breaking changes go to a new major path (`/v2`); `/v1` is never modified in a
breaking way. Health probes (`/healthz`, `/readyz`) live outside `/v1` so
Kubernetes probes are not coupled to the API version.

## Authentication

Both schemes use the `Authorization: Bearer <token>` header; the server picks the
verifier by token shape:

| Credential | Shape | How to get one |
|---|---|---|
| JWT access token | a signed JWT (no prefix), 15 min default TTL | `POST /v1/auth/login` |
| API token | `otx_<base64url>` | `otherix config add cluster`, or `POST /v1/users/me/api-tokens` |

```bash
# Login -> access token
TOKEN=$(curl -s -X POST http://localhost:8080/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"..."}' | jq -r .access_token)

curl -s http://localhost:8080/v1/vms -H "Authorization: Bearer $TOKEN"
```

Anonymous endpoints (`POST /v1/auth/login`, `POST /v1/auth/refresh`, `GET /v1/ca`,
`POST /v1/nodes/join`, `POST /v1/cluster/join`) declare `security: []`. See
[Users and RBAC](../guides/users-and-rbac.md) for roles and scopes.

## Error envelope

Every error response has the same shape:

```json
{ "error": { "code": "validation_failed", "message": "...", "details": {} } }
```

`code` is a stable snake_case string; `message` is human-readable; `details` is
optional. The full catalog is in [Error codes](error-codes.md).

!!! note "404, not 403, for invisibility"
    A resource that exists but is not visible to the caller returns `404 not_found`,
    never `403`. Existence is never leaked.

## Pagination

List endpoints use opaque cursor pagination:

- Query: `limit` (1..200, default 50) and `cursor` (opaque base64).
- Response: `{ "data": [...], "meta": { "next_cursor": "<string|null>" } }`.

There is no total count. A `null` `next_cursor` means the last page. Treat the
cursor as opaque.

## Idempotency

Every mutating request (POST/PATCH/DELETE) accepts an optional `Idempotency-Key`
header (up to 255 chars, 24 h TTL):

- Same key + same body within the TTL replays the original response.
- Same key + different body returns `409 idempotency_key_mismatch`.

```bash
curl -s -X POST http://localhost:8080/v1/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' \
  -d '{ "name": "demo", "image_url": "...", "architecture": "amd64" }'
```

!!! warning "Carve-out"
    `POST /v1/auth/login` and `POST /v1/auth/refresh` do **not** accept an
    `Idempotency-Key` - replaying a cached token pair would break refresh-token
    rotation.

## Async operations

Operations that touch a node or take more than a moment are asynchronous:

1. The request returns `202 Accepted` with
   `{ "task_id": "...", "status": "pending", "links": { "self": "/v1/tasks/{id}" } }`.
2. Poll `GET /v1/tasks/{task_id}` until `status` reaches `success`, `failed`, or
   `cancelled`. `POST /v1/tasks/{id}/cancel` is best-effort.

The `otherix` CLI hides this behind `--wait`. VM create/delete, the VM lifecycle
verbs (start/stop/poweroff/reboot), and storage-pool scans are async; pause,
resume, reset, and console-token issuance are synchronous `200`s. See
[Desired vs observed state](../concepts/desired-vs-observed.md).

## Conventions

- Paths are plural nouns; VM sub-operations are sub-resources
  (`/v1/vms/{id}/start`). `{id}` for VMs and nodes is the **name** (UUID literals
  are rejected with `400`).
- Datetimes are RFC 3339; UUIDs are `format: uuid`.
- `operationId` is `<resource>.<action>` (`users.list`, `apiTokens.createMe`).

## Resources

| Tag | Guide |
|---|---|
| `auth`, `users`, `api-tokens` | [Users and RBAC](../guides/users-and-rbac.md) |
| `nodes`, `join-tokens` | [Join a node](../guides/join-a-node.md) |
| `vms` | [Create and manage VMs](../guides/create-and-manage-vms.md) |
| `storage-pools` | [Storage pools](../guides/storage-pools.md) |
| `networks` | [Networks](../guides/networks.md) |
| `tasks` | (async operations, above) |

## Browse the full API

The complete specification is published as an interactive browser:

- **[REST API browser](../api/index.md)** - the full spec rendered with Swagger UI,
  live on this site.

To try it locally against your own running control plane, use the bundled preview:

```bash
make api-preview      # Swagger UI on :8081, Redoc on :8082
make api-preview-stop
```

You can also point any OpenAPI viewer at your control plane's copy of
`api/openapi/control-plane.yaml` (the source of truth).
