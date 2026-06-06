# Error codes

Every 4xx / 5xx response from the control-plane API carries a stable,
machine-readable `code` inside a uniform envelope:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "human-readable, en-US",
    "details": {}
  }
}
```

- `code` is snake_case and stable across releases.
- `message` is human-readable and may change.
- `details` is an optional object with code-specific context (omitted when
  empty).

Codes are also surfaced in async task error envelopes
(`GET /v1/tasks/{id}`): agent-side failures during `vm.create` and the VM
lifecycle relay their code through the task's error object rather than an HTTP
status. Clients must accept unknown codes gracefully; new endpoint-specific
codes may appear.

The catalog below mirrors `internal/api/response/error.go` and the `ErrorCode`
enum in `api/openapi/control-plane.yaml`. HTTP statuses are the typical mapping;
a given code may appear with a different status in an endpoint-specific case.

## Cross-cutting

| Code | Typical HTTP | Meaning |
| --- | --- | --- |
| `unauthenticated` | 401 | No / invalid credentials presented. |
| `invalid_credentials` | 401 | Login or token verification rejected the credential. |
| `token_revoked` | 401 | The presented token was revoked. |
| `permission_denied` | 403 | Caller's role lacks the required capability on a resource it can see. |
| `not_found` | 404 | Resource missing, or exists but invisible to the caller (existence is never leaked). |
| `method_not_allowed` | 405 | HTTP method not supported on the path. |
| `conflict` | 409 | Generic conflict (e.g. invalid state transition, resource cannot be operated on now). |
| `resource_in_use` | 409 | Resource has dependents and cannot be deleted. |
| `idempotency_key_mismatch` | 409 | Same `Idempotency-Key` reused with a different body within the TTL. |
| `validation_failed` | 400 | Request body / params failed validation. |
| `rate_limited` | 429 | Request rate exceeded. |
| `request_timeout` | 503 | Per-request deadline exceeded. |
| `agent_unreachable` | 502 | CP could not reach the agent owning the resource. |
| `internal` | 500 | Unexpected server-side failure. |

## vm.create passthrough (agent-relayed)

These surface in the `vm.create` task error envelope; the CP relays the agent's
code verbatim.

| Code | Typical HTTP / task status | Meaning |
| --- | --- | --- |
| `agent_task_lost` | failed | The agent lost the in-flight task before completion. |
| `import_timeout` | failed | Image download / import exceeded its deadline. |
| `qcow2_header_invalid` | failed | Downloaded image is not a valid qcow2. |
| `checksum_mismatch` | failed | Served image digest does not match `--image-sha256`. |
| `unsupported_format` | failed | Image disk format is not supported. |
| `pool_full` | failed | Target pool has insufficient space. |
| `pool_filesystem_unsupported` | failed | Pool's filesystem cannot host the disk image. |
| `image_collision` | failed | Cached image name collides with a different digest. |
| `download_failed` | failed | Image download failed (network / HTTP error). |
| `image_unavailable` | failed | Image source could not be retrieved. |
| `disk_too_small` | failed | Requested `--disk-gib` is smaller than the image's virtual size. |
| `path_not_allowed` | 400 | Pool `path` is not under any configured `storage_pools.allowed_path_prefixes`. |

## vm.create / vm.delete (CP-side + agent spawn)

| Code | Typical HTTP / task status | Meaning |
| --- | --- | --- |
| `vm_not_found` | 404 | Named VM does not exist. |
| `vm_name_in_use` | 409 | VM name already taken (globally unique). |
| `pool_not_writable` | 400 | Target pool is not writable for create. |
| `node_not_ready` | 409 | Owning / target node is not in a ready state. |
| `node_not_found` | 404 | Referenced node does not exist. |
| `vm_create_failed` | failed | Create failed (fall-through classification). |
| `vm_delete_failed` | failed | Delete failed (fall-through classification). |
| `qemu_spawn_failed` | failed | Agent could not launch the QEMU process. |

## Placement / cluster-default

| Code | Typical HTTP | Meaning |
| --- | --- | --- |
| `pool_not_found` | 400 | Named pool does not exist in the cluster. |
| `pool_not_on_node` | 409 | `--node` hint names a node where the pool has no instance. |
| `no_eligible_nodes` | 409 | Scheduler found no node satisfying the placement constraints. |
| `default_pool_not_set` | 400 | `--pool` omitted and no cluster default pool is configured. |
| `pool_name_ambiguous` | 409 | Pool name maps to multiple per-node instances; disambiguate by node or UUID. |

## Console

| Code | Typical HTTP | Meaning |
| --- | --- | --- |
| `vm_not_running` | 409 | Console requested against a VM that is not running. |
| `console_in_use` | 409 | Another console session already holds the per-VM single-session lock. |
| `protocol_not_available` | 409 | The VM does not expose the requested console protocol (e.g. serial). |

!!! note "Task-only codes"
    Some agent-side task failures use codes that live in task error envelopes
    only and are not part of the CP `ErrorCode` enum, e.g. `stop_timeout`
    (graceful stop / reboot did not complete within the agent's grace window).
    Treat any unrecognised code as an opaque failure label.

---

## See also

- [CLI reference](cli.md) - the operator CLI surfaces these codes in its output.
- [Configuration reference](configuration.md) - server and agent config keys.
