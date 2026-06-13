# Troubleshooting

A practical reference for the failure modes operators hit most. Each row gives the
likely cause and how to inspect. Most diagnostics start from one of:

- `otherix node list` / `otherix node get <name>` - node status, last heartbeat.
- `otherix vm get <name>` - VM status and, for a failed async operation, the task
  error code + message.
- `otherix pool list` / `otherix pool get <name>` - pool status, available space.
- The task error itself - async operations (vm create/delete, lifecycle, scan)
  report a stable `code` and a human message. The CLI prints CP-side failures as
  `api_error: <code>: <message>`.
- `journalctl -u otherix-agent` on the node - agent-side bootstrap, heartbeat, and
  qemu logs.
- The api-server logs - cluster, CA, and worker activity.

Error codes are catalogued in [Error codes](../reference/error-codes.md).

## Nodes and connectivity

| Symptom | Likely cause | How to inspect / recover |
| --- | --- | --- |
| Node stuck in `pending` past ~60s | Agent registered (CSR redeemed) but heartbeat not arriving - usually mTLS handshake failure or the agent cannot reach the CP | `journalctl -u otherix-agent -f` for `heartbeat` lines or TLS handshake errors. Confirm the agent reaches the CP at a name/IP in the CP cert SAN set (see [Certificates](certificates.md)); add missing names to `cp_cert.additional_sans` |
| Node flips `ready` -> `unreachable` | Heartbeats stopped arriving for longer than the stale threshold (default 90s) | Check the agent process is alive and the network path to the CP. The node returns to `ready` automatically once heartbeats resume |
| Node advances to `gone` | Unreachable past the gone-grace window (default 5m) | Recover the agent and restart it; if the node is permanently retired, no action needed |
| `agent_unreachable` on an operation | The CP could not reach the agent's mTLS endpoint when dispatching work | Verify the agent is running and its advertised endpoint is reachable from the CP. Check `otherix node get <name>` for last heartbeat |
| `api_error: unauthenticated` from the CLI | Access token expired (JWTs default to 15-min TTL) or the stored token was revoked | Re-login, or use a long-lived `otx_*` API token (`otherix config add cluster` stores one). If the cluster CA rotated, refresh the stored credential |

## VM lifecycle

| Symptom | Likely cause | How to inspect / recover |
| --- | --- | --- |
| `otherix vm get` shows `status: creating` indefinitely | The owning agent is unreachable, so the create task cannot progress | Check `otherix node get <node>` for the node's status / last heartbeat; confirm the agent endpoint is reachable from the CP |
| Task error `qemu_spawn_failed` | qemu could not launch on the node. Commonly KVM is unavailable (e.g. nested virt not exposed) so the agent falls back to TCG software emulation; a malformed cmdline can still fail | Read the task error message. On the node, `journalctl -u otherix-agent` shows the qemu invocation. TCG is functional but slow; provision KVM for real workloads |
| Task error `image_unavailable` | The agent could not materialize the image URL onto the chosen pool during create (download or cache write failed) | Read the task error - the underlying agent fetch failure surfaces under its own code (below). Fix the cause and retry |
| Task error `checksum_mismatch` | The downloaded image's SHA-256 does not match the `--image-sha256` you supplied (or the cached sidecar) | The URL is serving different bytes than expected. Verify the URL and checksum, then retry |
| Task error `download_failed` | The agent could not fetch the image URL (network, 404, auth) | Confirm the URL is reachable from the node. Check agent logs for the HTTP error, fix, and retry |
| Task error `node_not_ready` | The target node is not in `ready` state at dispatch | `otherix node list`; wait for the node to reach `ready`, or remove a `--node` hint pinning an unavailable node |
| Task error `vm_create_failed` / `vm_delete_failed` | Catch-all for a create/delete failure not covered by a more specific code | Read the task error message for the underlying cause |

## Placement and storage pools

| Symptom | Likely cause | How to inspect / recover |
| --- | --- | --- |
| `default_pool_not_set` on `vm create` without `--pool` | No cluster default pool is configured | `otherix cluster set-default-pool <name>`, or pass `--pool` explicitly |
| `pool_not_found` | The named pool does not exist anywhere in the cluster | `otherix pool list` to see registered pools; create it or fix the name |
| `pool_not_on_node` on `vm create --node X` | The pool name has no instance on node `X` | `otherix pool list --node X`; pick a different node, register the pool on `X`, or drop the `--node` hint |
| `pool_name_ambiguous` | The pool name resolves to multiple instances and the request did not disambiguate | Specify the node (`--node`) so a single pool instance is selected |
| `no_eligible_nodes` on `vm create` | The pool exists but every hosting node is cordoned, unreachable, not yet `ready`, or out of capacity (CPU / memory / disk, including node-pressure exclusion) | `otherix node list` and `otherix pool list` for status and free space. Uncordon a node, free capacity, or add a node. The 409 payload lists per-candidate utilization |
| `path_not_allowed` on `pool create` | The pool `path` is not under any `storage_pools.allowed_path_prefixes` entry | Pick a path under an existing prefix, or widen the allowlist in `api.yaml` |

## Cluster join and bootstrap

These surface during `join`-node startup or agent bootstrap; inspect the
api-server logs (joiner) or `journalctl -u otherix-agent` (agent).

| Symptom | Likely cause | How to inspect / recover |
| --- | --- | --- |
| `cluster CA fingerprint mismatch` (joiner) or `bootstrap: CA fingerprint mismatch` (agent) | The pinned `ca_fingerprint` does not match the CA the server returned - operator typo or, rarely, a MITM | Re-check the pinned fingerprint against the live cluster CA (mint a fresh join token to read the current fingerprint). If it genuinely differs, treat as a security event |
| `HTTP 401 unauthenticated` ("token not recognized or expired") | Join token TTL elapsed or token unknown | Mint a fresh join token and retry. The api-server slog WARN `reason` field (`token_invalid`) distinguishes the cause |
| `HTTP 401 unauthenticated` ("token max_uses exceeded") | A multi-use join token hit its `max_uses` cap | Mint a fresh token. The slog WARN `reason` field is `token_exhausted` |
| `cluster CA divergence: on-disk CA ... does not match active etcd CA` | The on-disk cluster CA and the active `ca_certs` row in etcd disagree (e.g. wiped etcd data dir with a stale on-disk CA, or a restore mismatch) | Make the two consistent - restore the matching CA, or for dev `make etcd-reset` wipes both together. See [Certificates](certificates.md) and [Backups](backups.md) |
| Joiner registered as learner but never becomes a voter | The learner has not caught up, or the promote loop cannot reach quorum | Watch `GET /v1/cluster/members`; the ~15s promote loop converts caught-up learners automatically. Check the joiner's etcd logs for replication progress |
| `cert <path> exists but key <path> missing` (agent) | Partial bootstrap state - a mid-flight crash or manual file deletion left cert without key (or vice-versa) | Delete the orphaned cert + key + CA material under the agent's cert dir (`/var/lib/otherix/certs/`), then re-run the agent bootstrap. Identity is derived from the cert CN - no sidecar file to clean up |
| `connection refused` fetching `/v1/ca` or `/v1/cluster/join` | The target CP is not running or not reachable from the joining host | Confirm the CP is up (`curl -k https://<cp-host>:<port>/healthz`) and that the join host can reach it |

## General checks

- **Liveness / readiness:** `/healthz` (process up) and `/readyz` (dependencies
  reachable) sit outside `/v1/`. A 503 from `/readyz` means the api-server cannot
  reach its store.
- **Cluster membership:** `GET /v1/cluster/members` shows voters and learners -
  the first thing to check on any HA / quorum issue. See
  [High availability](high-availability.md).
- **Worker activity:** if async tasks stay `pending` forever, confirm
  `workers.enabled: true` and that the agent client is provisioned - a CP with
  workers disabled accepts tasks but never runs them.
