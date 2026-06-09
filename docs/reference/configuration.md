# Configuration reference

Otherix binaries load configuration from an optional YAML file overlaid with
environment variables, using koanf. This page lists every config key for the
two stateful binaries: the api-server (`api.yaml`) and the agent
(`agent.yaml`).

Keys below are dotted koanf paths. Defaults are taken from the in-code
`defaultAPIConfig` / `defaultAgentConfig`; a value of "(none)" means the field
has no default (empty string / zero) and "(required)" means the binary refuses
to start without it.

## Environment overrides

Every key can be overridden by an environment variable:

- Prefix: `OTHERIX_`.
- Nesting separator: `__` (double underscore), because single `_` is valid
  inside snake_case keys.

So `server.listen` is `OTHERIX_SERVER__LISTEN`, and
`workers.heartbeat.interval` is `OTHERIX_WORKERS__HEARTBEAT__INTERVAL`.

Two api-server bootstrap secrets are read directly from the environment (not
koanf keys):

| Env var | Meaning |
| --- | --- |
| `OTHERIX_BOOTSTRAP_ADMIN_EMAIL` | Email of the first admin seeded on first boot. |
| `OTHERIX_BOOTSTRAP_ADMIN_PASSWORD` | Password for that admin. |

Both must be set together (or neither); setting only one is fatal. With an
existing admin they are a no-op.

---

## api-server (`api.yaml`)

### server

User-facing HTTP listener.

| Key | Default | Meaning |
| --- | --- | --- |
| `server.listen` | `0.0.0.0:8080` | Listen address (required). |
| `server.read_timeout` | `30s` | HTTP read timeout. |
| `server.write_timeout` | `30s` | HTTP write timeout. |
| `server.shutdown_grace` | `30s` | Graceful-shutdown budget. |

### agent_server

Optional second HTTPS listener dedicated to mTLS agent traffic.

| Key | Default | Meaning |
| --- | --- | --- |
| `agent_server.enabled` | `false` | Enable the agent-facing listener. |
| `agent_server.listen` | (none) | Listen address (required when enabled). |

mTLS material is not configured here; each replica auto-generates its server
cert from the cluster CA (see `cp_cert`).

### agent_client

CP -> agent outbound HTTP client (polling) used by in-process workers.

| Key | Default | Meaning |
| --- | --- | --- |
| `agent_client.enabled` | `false` | Gate the client; workers that dispatch to agents need `true`. |
| `agent_client.timeout` | `5m` | Per-operation timeout (must be > 0 when enabled). |
| `agent_client.poll_interval` | `1s` | Initial poll interval (must be > 0 when enabled). |
| `agent_client.poll_max_interval` | `30s` | Max poll interval; must be >= `poll_interval`. |

### cp_cert

Per-replica CP server-cert lifecycle. Three modes: operator override
(`cert_file` + `key_file`), local cache, or auto-generate (default).

| Key | Default | Meaning |
| --- | --- | --- |
| `cp_cert.cert_file` | (none) | Operator-override cert path (paired with `key_file`). |
| `cp_cert.key_file` | (none) | Operator-override key path (paired with `cert_file`). |
| `cp_cert.local_cache.enabled` | `false` | Persist/reuse a generated cert across restarts. |
| `cp_cert.local_cache.cert_path` | `/var/lib/otherix/certs/cp-cert.crt` | Cache cert path. |
| `cp_cert.local_cache.key_path` | `/var/lib/otherix/certs/cp-cert.key` | Cache key path. |
| `cp_cert.additional_sans` | (none) | Extra SANs unioned with the auto-detected baseline. |
| `cp_cert.validity` | `365d` (8760h) | Generated-cert validity (>= 24h when set). |

### cluster_ca

On-disk cluster CA location (cert + key), provisioned before etcd starts.

| Key | Default | Meaning |
| --- | --- | --- |
| `cluster_ca.cert_file` | `/var/lib/otherix/ca/cluster-ca.crt` | CA cert path (required). |
| `cluster_ca.key_file` | `/var/lib/otherix/ca/cluster-ca.key` | CA key path (required). |

### cluster_join

Joiner-side cluster-CA fetch; consulted only when `etcd.mode=join` and no CA is
on disk yet.

| Key | Default | Meaning |
| --- | --- | --- |
| `cluster_join.cp_url` | (none) | Existing replica's base URL to redeem the join token against. |
| `cluster_join.token` | (none) | Inline join token (mutually exclusive with `token_path`). |
| `cluster_join.token_path` | (none) | Path to a file holding the join token (preferred). |
| `cluster_join.ca_fingerprint` | (none) | Expected CA fingerprint (out-of-band TOFU pin). |
| `cluster_join.timeout` | (none) | Per-request timeout for the join fetch. |

### logger

| Key | Default | Meaning |
| --- | --- | --- |
| `logger.level` | `info` | Log level. |
| `logger.format` | `json` | Log format. |

### auth

JWT signing material and token lifetimes.

| Key | Default | Meaning |
| --- | --- | --- |
| `auth.jwt_secret` | (required) | HS256 secret; must be >= 32 bytes. |
| `auth.jwt_access_ttl` | `15m` | Access-token TTL (> 0). |
| `auth.jwt_refresh_ttl` | `30d` (720h) | Refresh-token TTL (> 0). |

### console

| Key | Default | Meaning |
| --- | --- | --- |
| `console.access_mode` | `proxy` | Console bridging mode: `proxy` or `direct`. |

### workers

In-process worker pool and its sub-blocks.

| Key | Default | Meaning |
| --- | --- | --- |
| `workers.enabled` | `true` | Run the in-process worker pool. |
| `workers.max_workers` | `10` | Bounded concurrency. |
| `workers.tasks.retention.completed` | `7d` (168h) | Retention for completed tasks. |
| `workers.tasks.retention.failed` | `30d` (720h) | Retention for failed / cancelled tasks. |
| `workers.heartbeat.stale_threshold` | `90s` | Window after which a silent node flips to `unreachable`. |
| `workers.heartbeat.gone_grace` | `5m` | Window after which an unreachable node advances to `gone` (must exceed `stale_threshold`). |
| `workers.heartbeat.interval` | `30s` | Node-health reconciler cadence. |
| `workers.storage_pool_scan.enabled` | `true` | Register the periodic pool-scan trigger. |
| `workers.storage_pool_scan.interval` | `15m` | Scan-trigger cadence (>= 0). |
| `workers.storage_pool_scan.jitter` | `30s` | Random stagger; must be < `interval`. |
| `workers.backup.enabled` | `false` | Periodic etcd snapshot worker (opt-in). |
| `workers.backup.interval` | `6h` | Snapshot interval (>= 0). |
| `workers.backup.dir` | (none) | Snapshot destination (required when backup enabled). |
| `workers.backup.retention` | `7` | Number of snapshot files kept (>= 0). |

### placement

VM placement algorithm and per-resource gating.

| Key | Default | Meaning |
| --- | --- | --- |
| `placement.algorithm` | `resource_aware` | `resource_aware` or `least_vm_count`. |
| `placement.resources.cpu.enabled` | `true` | Include CPU in fit check + scoring. |
| `placement.resources.cpu.overcommit_ratio` | `1.0` | CPU overcommit multiplier (> 0). |
| `placement.resources.memory.enabled` | `true` | Include memory. |
| `placement.resources.memory.overcommit_ratio` | `1.0` | Memory overcommit multiplier (> 0). |
| `placement.resources.disk.enabled` | `true` | Include disk. |
| `placement.resources.disk.overcommit_ratio` | `1.0` | Disk overcommit multiplier (> 0). |
| `placement.pressure.memory.enabled` | `true` | Detect memory pressure. |
| `placement.pressure.memory.threshold_percent` | `10` | Trigger when % available falls below this (1..99). |
| `placement.pressure.memory.consecutive_required` | `3` | Observations below threshold before flagging (>= 1). |
| `placement.pressure.system_disk.enabled` | `true` | Detect root-filesystem pressure. |
| `placement.pressure.system_disk.threshold_percent` | `10` | Threshold (1..99). |
| `placement.pressure.system_disk.consecutive_required` | `3` | Observations before flagging (>= 1). |
| `placement.pressure.disk.enabled` | `true` | Detect per-pool disk pressure. |
| `placement.pressure.disk.threshold_percent` | `15` | Threshold (1..99). |
| `placement.pressure.disk.consecutive_required` | `1` | Observations before flagging (>= 1). |

### storage_pools

| Key | Default | Meaning |
| --- | --- | --- |
| `storage_pools.allowed_path_prefixes` | `[/var/lib/otherix/pools/]` | Allowed `path` prefixes for pool create (each must be absolute and end with `/`; at least one entry). |
| `storage_pools.default_pool_name` | `default` | Cluster default pool auto-provisioned on boot; empty string opts out. |

### network

Overlay-network bounds, seed-only (read once at first boot to seed
`cluster_settings`; replicas read the etcd value thereafter).

| Key | Default | Meaning |
| --- | --- | --- |
| `network.overlay_supernet` | `10.42.0.0/16` | Overlay supernet CIDR. |
| `network.vni_range.min` | `1000` | Min VXLAN VNI (>= 1000 when set). |
| `network.vni_range.max` | `65535` | Max VNI (< 16777215, > min). |
| `network.underlay_mtu` | `1500` | Physical underlay MTU; `0` or `1390..65535`. |

### etcd

Embedded etcd member backing the store. Single-node defaults let a standalone
api-server boot with no operator input.

| Key | Default | Meaning |
| --- | --- | --- |
| `etcd.mode` | `single` | `single`, `bootstrap`, or `join`. |
| `etcd.name` | `otherix-0` | Unique member name within the cluster. |
| `etcd.data_dir` | `/var/lib/otherix/etcd` | Member data directory (WAL + snapshots). |
| `etcd.peer_url` | `https://127.0.0.1:2380` | Raft peer advertise/listen URL. |
| `etcd.client_url` | `http://127.0.0.1:2379` | Client advertise/listen URL. |
| `etcd.cluster_token` | `otherix-cluster` | Initial-cluster token isolating clusters. |
| `etcd.initial_cluster` | (none) | Full member list (required for `bootstrap` / `join`). |
| `etcd.peer_cert_file` | (none) | Operator peer (Raft) mTLS cert. |
| `etcd.peer_key_file` | (none) | Operator peer mTLS key. |
| `etcd.peer_ca_file` | (none) | Operator cluster-CA trust anchor for peer mTLS. |
| `etcd.peer_auto_dir` | `/var/lib/otherix/peer` | Directory for auto-generated peer cert/key/ca. |
| `etcd.compaction_mode` | `periodic` | `periodic` or `revision`. |
| `etcd.compaction_retention` | `1h` | Duration (periodic) or count (revision). |

---

## agent (`agent.yaml`)

### server

The agent's HTTPS (mTLS) server.

| Key | Default | Meaning |
| --- | --- | --- |
| `server.listen` | `0.0.0.0:9443` | Listen address (required). |
| `server.read_timeout` | `30s` | HTTP read timeout. |
| `server.write_timeout` | `30s` | HTTP write timeout. |
| `server.shutdown_grace` | `30s` | Graceful-shutdown budget. |

### logger

| Key | Default | Meaning |
| --- | --- | --- |
| `logger.level` | `info` | Log level. |
| `logger.format` | `json` | Log format. |

### top-level

| Key | Default | Meaning |
| --- | --- | --- |
| `state_path` | `/var/lib/otherix/vms` | Agent local state root (running VMs, caches). |

### control_plane

| Key | Default | Meaning |
| --- | --- | --- |
| `control_plane.url` | (required) | CP base URL the agent reaches. |
| `control_plane.heartbeat_interval` | `30s` | Heartbeat send cadence. |

### tls

mTLS material paths (written by `otherix-agent bootstrap`).

| Key | Default | Meaning |
| --- | --- | --- |
| `tls.ca_cert_path` | (none) | Cluster CA trust anchor. |
| `tls.cert_path` | (none) | Agent leaf cert. |
| `tls.key_path` | (none) | Agent leaf key. |

### migration

Peer-to-peer migration ingress (port range supports parallel migrations).

| Key | Default | Meaning |
| --- | --- | --- |
| `migration.host` | (required) | Migration advertise host (separate from `server.listen`). |
| `migration.port_range_start` | `49152` | Range start (1024..65535). |
| `migration.port_range_end` | `49251` | Range end (1024..65535, >= start). |

### qemu

| Key | Default | Meaning |
| --- | --- | --- |
| `qemu.aarch64_firmware_path` | `/usr/share/AAVMF/AAVMF_CODE.fd` | UEFI firmware blob for aarch64 guests; ignored on amd64. |

### wireguard

WG overlay-fabric tunables. The keypair is generated lazily at serve time.

| Key | Default | Meaning |
| --- | --- | --- |
| `wireguard.listen_port` | `51820` | WG UDP listen port. |
| `wireguard.persistent_keepalive` | `25s` | Per-peer keepalive interval. |
| `wireguard.private_key_path` | `/var/lib/otherix/wg/private.key` | Persisted private-key path. |
| `wireguard.advertised_endpoint` | (none) | `host:port` advertised to peers (required for mesh reachability; empty is valid single-node). |

!!! note "Agent bootstrap is not config"
    `otherix-agent bootstrap` is driven by CLI flags, not config keys. It
    writes the `tls.*` material and a generated `agent.yaml`; it never
    overwrites an existing `agent.yaml`.

---

## See also

- [CLI reference](cli.md) - the operator CLI and its own credential store.
- [Error codes](error-codes.md) - the API error-code catalog.
