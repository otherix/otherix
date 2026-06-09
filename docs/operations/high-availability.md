# High availability

Otherix has no external database. Each `otherix-api` process embeds an etcd
member, and multiple replicas form one self-clustering etcd Raft cluster. The
control-plane store, the async job queue, and all cluster state live in that
etcd. Run an odd number of replicas (3 or 5) so the cluster keeps quorum when
one replica is down.

All replicas are equal. There is no primary. Work (VM create/delete, lifecycle
operations, storage-pool scans) is enqueued as jobs in etcd and any replica with
workers enabled claims them off the queue, so adding replicas adds both quorum
resilience and worker capacity.

## How replicas cluster

Replicas talk to each other over the etcd Raft *peer* transport, secured with
mutual TLS. Every replica presents a peer certificate signed by the cluster CA
and rejects peers that do not - safe to run over an untrusted network. The peer
cert is auto-provisioned from the on-disk cluster CA before the etcd member
starts (no operator PKI work); see [Certificates](certificates.md).

Each member needs four etcd settings, configured under the `etcd:` block of
`api.yaml`:

| Key | Meaning |
| --- | --- |
| `etcd.mode` | `single` (default), `bootstrap`, or `join` - see below |
| `etcd.name` | Unique member name within the cluster (e.g. `otherix-0`) |
| `etcd.peer_url` | This member's Raft peer advertise/listen URL (`https://host:2380`) |
| `etcd.client_url` | This member's client advertise/listen URL (`http://host:2379`) |
| `etcd.initial_cluster` | Full member list `n0=peer0,n1=peer1,...` (bootstrap mode) |
| `etcd.cluster_token` | Initial-cluster token; isolates clusters sharing a network |

Peer URLs are `https` because peer mTLS is always on. The client URL is the
loopback endpoint the api-server, the promote loop, and the backup worker dial.

## The three modes

### `single` (default)

A standalone control plane: quorum of one, self-only initial cluster. This is
the default and needs no operator input beyond the shipped defaults
(`name: otherix-0`, loopback peer/client URLs). A single-node deployment runs in
this mode and can later grow to HA via `join` without reconfiguring node 0.

### `bootstrap` (form a fresh multi-node cluster)

Every member of a brand-new HA cluster starts together with `etcd.mode: bootstrap`
and the same full `etcd.initial_cluster` member list, forming quorum in one shot.
Use this when you are standing up three replicas at once and can list all their
peer URLs up front. Each member also needs a unique `etcd.name` and its own
`etcd.peer_url` / `etcd.client_url`.

```yaml
etcd:
  mode: bootstrap
  name: otherix-1
  peer_url: https://10.0.0.11:2380
  client_url: http://10.0.0.11:2379
  cluster_token: my-cluster
  initial_cluster: "otherix-0=https://10.0.0.10:2380,otherix-1=https://10.0.0.11:2380,otherix-2=https://10.0.0.12:2380"
```

### `join` (add a replica to a running cluster)

A new replica joins an already-running cluster with `etcd.mode: join`. It does
**not** need `etcd.initial_cluster` - it computes the member list from the
cluster itself. The joiner first calls `/v1/cluster/join` on an existing replica
to fetch the shared cluster CA (validated against a pinned fingerprint - trust on
first use) and to register its own peer URL. Configure the `cluster_join:` block:

```yaml
etcd:
  mode: join
  name: otherix-2
  peer_url: https://10.0.0.12:2380
  client_url: http://10.0.0.12:2379
  cluster_token: my-cluster
cluster_join:
  cp_url: https://10.0.0.10:8443     # an existing replica's agent-TLS listener
  token_path: /etc/otherix/cluster-join-token   # plaintext kind=cluster join token
  ca_fingerprint: sha256:<hex>        # pinned cluster CA fingerprint
  timeout: 30s
```

Mint the join token on an existing replica with
`POST /v1/nodes/join-tokens` (`{"kind":"cluster"}`); the response carries the
token plaintext and the cluster CA fingerprint to pin. Supply the token via
`token_path` (preferred, keeps the secret out of the config file) or inline
`token`; exactly one.

### Growing from single to HA (recommended)

The fastest way to grow a running single-node control plane into HA is the pair
of operator wrappers below. They drive the same `join` engine documented above -
no hand-edited config, no raw API calls.

**Step 1 - mint a join token (on an existing replica).** Run:

```
otherix cluster join-token create --ttl 1h
```

This prints the token plaintext and the cluster CA fingerprint as
`sha256:<hex>`. A cluster token redeems for the cluster CA *private* key, so it
is single-use by default; pass `--max-uses N` to grow several replicas with one
token. `--output text|json` selects the format.

**Step 2 - join the cluster (on the new control-plane host).** With the
`otherix-api` .deb already installed and the daemon running in `single` mode,
run:

```
sudo otherix-api join \
  --cp-url https://<existing-replica>:8443 \
  --token <token> \
  --ca-fingerprint sha256:<fingerprint> \
  --name otherix-1
```

`--cp-url` points at the existing replica's agent-TLS listener (`:8443` by
default), where `/v1/cluster/join` is served - not the public API listener
(`:8080`).

This writes the `cluster_join:` block, sets `etcd.mode: join`, and records a
unique `etcd.name` into `/etc/otherix/api.yaml`, then restarts the unit. At boot
the daemon redeems the token, registers itself as a learner, and auto-promotes
to a voter once caught up (see below). `etcd.name` must be unique across the
cluster - do not reuse the seed node's default `otherix-0`; `--name` defaults to
the host's hostname. `etcd.peer_url` is left at the packaged default `auto`,
which resolves to the host's routable IPv4 at boot, so there is no per-host peer
URL to set.

Pass the token inline with `--token` or from a file with `--token-path`
(exactly one). Other flags: `--config` (default `/etc/otherix/api.yaml`),
`--token-dest` (default `/var/lib/otherix/cluster-join-token`), `--no-restart`
(write config but leave the unit for a later restart), and `--force`. The
existing replica and the new host must share the same `etcd.cluster_token`; the
packaged default matches, so this is automatic unless an operator changed it.

**Step 3 - confirm quorum.** Once the new replica has promoted to a voter:

```
otherix cluster member list
```

The `cluster_join:` YAML block shown above is the equivalent manual edit for
IaC-templated deployments - the wrappers exist for convenience and do not
replace the config-driven path.

### Learner registration and auto-promotion

When a `join` node calls `/v1/cluster/join`, the existing replica registers the
joiner's peer URL as an etcd **learner** - a non-voting member that replicates the
log without affecting quorum until it has caught up. Every replica runs an
always-on promote loop (a ~15s tick) that promotes any caught-up learner to a
full voter automatically. There is no operator promote step. Watch the voter
count converge with:

```
GET /v1/cluster/members
```

The joiner is safe to restart: once its etcd data dir has a write-ahead log, etcd
recovers membership from the WAL and the node rejoins without re-reading
`initial_cluster`.

## Quorum and sizing

etcd needs a majority of voters to commit a write. Size for the failures you want
to survive:

| Voters | Tolerates down | Notes |
| --- | --- | --- |
| 1 | 0 | `single` mode, no redundancy |
| 3 | 1 | Recommended HA minimum |
| 5 | 2 | Larger clusters |

Always run an **odd** number of voters. An even count adds a member without
improving fault tolerance (4 voters still tolerate only 1 down) and widens the
quorum. Grow one member at a time and let each promote to voter (watch
`/v1/cluster/members`) before adding the next - etcd settle-gates membership
changes and rejects a second change until the cluster has stabilized.

If a majority of voters is lost, the cluster stops accepting writes until enough
members return. Recover by restarting the down replicas (their data dirs let them
rejoin); if their data is gone, restore from a snapshot - see
[Backups](backups.md).

## Reference smoke

`make smoke-ha` runs three real `otherix-api` processes on loopback, grows a
single node to a 3-voter cluster entirely through the self-driving join +
auto-promote path (no manual etcd calls), then asserts replication, survives a
1-of-3 partition, and heals on restart. It is the executable reference for the
flows on this page and runs with no Docker and no Lima.
