# Certificates

Otherix runs its own internal PKI. A single cluster CA anchors three certificate
types: the etcd peer (Raft) certs that secure replica-to-replica traffic, the
per-replica control-plane server certs, and the per-node agent certs. All of it
is provisioned automatically on first boot - operators normally do not touch a
certificate file. This page documents the lifecycle so you can reason about
trust, expiry, and recovery.

## The cluster CA

The cluster CA is the root of trust for everything below it.

- **Algorithm / validity:** ECDSA P-384, self-signed, ~10-year validity,
  `CN=otherix-cluster-ca`, marked as a CA.
- **Generated once, on first boot** of a `single` or `bootstrap` replica.
- **Stored in two places that must stay consistent:**
    - On disk, at `cluster_ca.cert_file` / `cluster_ca.key_file`
      (default `/opt/otherix/ca/cluster-ca.{crt,key}`). The CA must be on disk
      *before* etcd starts, because the peer-mTLS plane needs a CA-signed cert
      pre-start.
    - In etcd, as the active `ca_certs` row. On boot the api-server syncs the
      on-disk CA into etcd (`BootstrapClusterCA`) so the `/v1/ca` endpoint and the
      node-join CSR signer have an active row.

On a `single`/`bootstrap` node the on-disk CA is generated on first boot and
reloaded on every restart. A `join` node does **not** mint its own CA: it fetches
the existing cluster's CA over the join protocol (see below) and persists it
before its etcd member starts.

### Joiner CA fetch (trust on first use)

A joining replica with no CA on disk fetches it from an existing replica via
`POST /v1/cluster/join`. The transport uses trust-on-first-use: the connection
skips chain verification (the target's serving cert does not chain to the CA being
fetched), and the joiner instead pins the returned CA against an operator-supplied
`ca_fingerprint` (SHA-256 hex). A mismatch aborts the join. This is the same TOFU
model agents use to bootstrap.

!!! warning "On-disk CA and etcd CA must agree"
    If the on-disk CA fingerprint does not match the active `ca_certs` row in etcd,
    the api-server refuses to start (CA divergence) rather than serve two trust
    roots. This happens if you wipe the etcd data dir but keep an old on-disk CA
    (or vice-versa), or restore an etcd snapshot whose CA differs from the on-disk
    files. Keep the two in sync. For dev, `make etcd-reset` wipes the etcd data dir
    **and** the on-disk CA together so the next boot regenerates a matched pair.
    See [Backups](backups.md) for restore-time guidance.

## Per-replica control-plane cert

Each replica presents one leaf cert on both its inbound agent listener and its
outbound dials to agents (ExtKeyUsage `serverAuth` + `clientAuth`). The loader
(`LoadOrGenerateCPCert`) selects one of three modes:

- **Mode A - operator files.** Set `cp_cert.cert_file` and `cp_cert.key_file` to
  use an externally-issued cert (corporate CA, Let's Encrypt, FIPS scenarios).
  Both must be set; missing files when configured is fatal. The cluster CA is still
  loaded from etcd to validate inbound agent client certs.
- **Mode B - local cache.** With `cp_cert.local_cache.enabled: true`, a cert
  cached at `/opt/otherix/certs/cp-cert.{crt,key}` is reused across restarts when
  it still chains to the current CA, is not near expiry, and its SANs cover the
  expected set. Otherwise it falls through to Mode C and the regenerated cert is
  re-cached.
- **Mode C - auto-generate (default).** A fresh ECDSA P-384 leaf signed by the
  cluster CA, ~365-day validity (`cp_cert.validity`, minimum 24h), `CN=otherix-cp-replica`.

### SANs

The auto-generated cert's SAN set is the auto-detected baseline - `localhost`,
`127.0.0.1`, `os.Hostname()`, and the non-wildcard listen address - unioned with
any `cp_cert.additional_sans` you configure, deduped. **The agent must be able to
reach the CP at a name or IP present in this SAN set.** If agents dial the CP by a
hostname or VIP that auto-detection misses, add it to `cp_cert.additional_sans`.

## etcd peer (Raft) certs

Replica-to-replica etcd traffic uses mutual TLS. Before the member starts, the
api-server provisions peer material from the on-disk cluster CA
(`ProvisionPeerCert`):

- **Auto-generate (default):** a fresh peer leaf signed by the cluster CA, written
  under `etcd.peer_auto_dir` (default `/opt/otherix/peer/`), with the CA cert
  materialized as the peer trust file. SANs derive from the member's `peer_url` host
  plus the localhost baseline.
- **Operator override:** set all three of `etcd.peer_cert_file`,
  `etcd.peer_key_file`, `etcd.peer_ca_file` to pre-distribute peer certs. The files
  must exist and the leaf must chain to the cluster CA (verified at boot, so a
  wrong-CA cert fails fast instead of being rejected opaquely at Raft-handshake
  time).

Peer URLs are therefore `https` whenever peer mTLS material is set, which is
always in production.

## Node (agent) certs

Each agent gets a client cert when it joins, via the node-join CSR flow:

- The agent generates a keypair and submits a CSR to `POST /v1/nodes/join` with a
  plaintext join token.
- The CP validates the token and signs the CSR with the cluster CA. The cert
  template is server-authoritative: `CN=node-<name>`, SAN includes
  `node-<name>.agents.otherix.local`, ~1-year validity. The CSR's own
  Subject/SAN are ignored (defense against CN injection).
- **Agent identity is its cert CN.** The agent parses `node-<name>` from its own
  cert at startup to learn its node name; the CP binds heartbeats to that identity.

The agent persists its cert material under `/opt/otherix/certs/` and reuses it
across restarts. Bootstrap is a one-time event; routine agent restarts do not
re-issue.

## Rotation

!!! warning "Rotation is not yet automated"
    There is no automatic certificate rotation. When a cert approaches expiry the
    operator re-issues it manually:

    - **CP replica cert:** in Mode C it regenerates every boot, so restarting the
      replica issues a fresh leaf. In Mode B, delete the cached cert and restart. In
      Mode A, replace the operator files and restart.
    - **Node cert:** re-run the agent bootstrap to obtain a fresh node cert (delete
      the existing cert material first; see [Troubleshooting](troubleshooting.md)).
    - **Cluster CA / peer cert:** CA rotation is planned but not implemented. The
      10-year CA validity makes this a long-horizon concern; peer certs regenerate
      from the CA on each boot.

    Track cert expiry out of band (e.g. monitor `not_after` from the boot logs, or
    inspect a member with `openssl s_client`).
