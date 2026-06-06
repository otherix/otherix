# Join a node

Enrol a hypervisor node into the cluster. The flow is two-sided: an
admin mints a short-lived join token on the control plane, then the
operator runs the agent's one-shot `bootstrap` on the node host to
redeem it. Redemption signs the node's mTLS identity (no manual cert
distribution), after which `otherix-agent serve` brings the agent to
full runtime and the node appears in `otherix node list`.

## 1. Mint a join token

Join tokens are admin-only and managed through the control-plane CLI
(`otherix node join-token ...`, the `/v1/nodes/join-tokens` surface).

```bash
otherix node join-token create --node-name node-1 --ttl 10m
```

```text
Token created (id: 9f1c...):

  otx_join_AbC123...

CA fingerprint:

  sha256:1a2b3c...

Save BOTH NOW - server stores only hash, plaintext cannot be retrieved.
Pass to agent via OTHERIX_BOOTSTRAP__TOKEN + OTHERIX_BOOTSTRAP__CA_FINGERPRINT (koanf double-underscore nesting).
Expires: 2026-06-06T12:10:00Z (TTL: 10m0s).
Max uses: 1.
Bound to node-name: node-1 (single-use).
```

The token plaintext and the cluster CA fingerprint are printed exactly
once - the server stores only `sha256(token)`. Save both: the agent
needs the token to redeem and the fingerprint to pin the CP's TLS cert
(trust-on-first-use).

### Flags

| Flag | Default | Notes |
|---|---|---|
| `--ttl` | `1h` | Validity, clamped to `[1m, 24h]`. |
| `--max-uses` | `0` | Consumption cap. `0` means unlimited within the TTL. |
| `--node-name` | (unset) | Bind the token to a specific node identity. Forces single-use (`--max-uses` is set to 1 server-side). |
| `--output` | `text` | `text` or `json`. |

Two minting modes:

- **Pre-bound, single-use** - pass `--node-name`. The token can only
  ever provision that one node identity. Defense-in-depth for known
  hosts.
- **Fleet bootstrap** - omit `--node-name`. Pass `--max-uses N` for a
  strict cap, or leave it at `0` for unlimited redemptions within the
  TTL. Each agent supplies its own `--node-name` at bootstrap time.

### Manage tokens

```bash
# Cursor-paginated list. Expired tokens are hidden unless --include-expired.
otherix node join-token list
otherix node join-token list --include-expired --limit 50

# Revoke an unconsumed token by id (idempotent; already-expired returns 409).
otherix node join-token revoke <token-id>

# Audit trail: which agents consumed a token, when, from which source IP.
otherix node join-token consumptions <token-id>
```

`list` renders `ID | NODE-NAME | TTL-REMAINING | MAX-USES | CONSUMED |
CREATED-BY`; `consumptions` renders `ID | NODE-ID | CONSUMED-AT |
SOURCE-IP`. Both accept `--output json` and `--limit` / `--cursor` for
pagination.

## 2. Bootstrap the agent on the node host

Run `otherix-agent bootstrap` on the node itself. It executes the join
protocol against the CP and writes the issued cert material plus a
generated `agent.yaml` to disk.

```bash
otherix-agent bootstrap \
  --token otx_join_AbC123... \
  --ca-fingerprint sha256:1a2b3c... \
  --cp-url https://cp.example:8443 \
  --node-name node-1 \
  --advertised-endpoint https://10.0.0.21:9443 \
  --migration-host 10.0.0.21
```

The token can come from three mutually exclusive sources (exactly one
required):

- `--token <plaintext>` - the literal value.
- `--token-path <file>` - read from a file (whitespace-trimmed).
- `--token-env <NAME>` - read from the named env var at invocation.

### Flags

| Flag | Default | Notes |
|---|---|---|
| `--token` / `--token-path` / `--token-env` | - | Token source (exactly one). |
| `--ca-fingerprint` | - | Cluster CA `sha256` fingerprint (`sha256:<hex>` or bare hex). Required. |
| `--cp-url` | - | Control-plane base URL (`https://...`). Required. |
| `--node-name` | - | Cluster-unique node name. Required. |
| `--advertised-endpoint` | - | HTTPS URL the CP uses to reach this agent. Required. |
| `--migration-host` | - | Host/IP advertised for live-migration ingress. Required. |
| `--migration-port-range-start` | `49152` | Migration port range lower bound. |
| `--migration-port-range-end` | `49251` | Migration port range upper bound. |
| `--cert-dir` | `/opt/otherix/certs` | Destination for `agent.key` / `agent.crt` / `ca.crt`. |
| `--config-path` | `/etc/otherix/agent.yaml` | Destination for the generated config. |
| `--listen` | `0.0.0.0:9443` | Agent HTTPS bind address baked into the generated config. |
| `--heartbeat-interval` | `30s` | Heartbeat cadence baked into the generated config. |
| `--request-timeout` | `30s` | Per-HTTP-request timeout against the CP. |
| `--force` | `false` | Re-issue cert material on an already-bootstrapped host. |

The architecture is auto-detected from the host (`runtime.GOARCH`);
operators do not supply it.

### What bootstrap does

1. Fetches `/v1/ca` with TLS verification skipped, then pins the
   returned cert against `--ca-fingerprint` (trust-on-first-use). A
   mismatch aborts - an operator typo or an active MITM.
2. Generates an ECDSA P-384 keypair and a CSR.
3. Redeems the token at `POST /v1/nodes/join`. The CP signs the CSR
   with the cluster CA and returns the leaf cert plus the CA bundle.
4. Re-verifies the response chains to the pinned CA, then writes
   `agent.key` (0600), `agent.crt` and `ca.crt` (0644) atomically, and
   `agent.yaml` (only if absent).

!!! warning "The token is consumed on redemption"
    The CP consumes the token once `POST /v1/nodes/join` returns,
    even if the agent never observes the response. A failed bootstrap
    after that point needs a fresh token - re-run step 1.

### Idempotency

`bootstrap` is safe to re-run:

- All three cert files present and `--force` not set: prints
  "already bootstrapped" and exits 0.
- A partial cert state (some files present, not all) without `--force`:
  refuses - delete the orphans or re-run with `--force`.
- `--force` re-issues cert material but **never** overwrites an
  existing `agent.yaml`. Operator-tuned config (logger, storage paths,
  endpoints) always survives. To regenerate the config, delete it
  first.

## 3. Start the agent

```bash
otherix-agent serve
```

`serve` (also the default when the binary is run with no subcommand)
boots in **State A**: every 5 seconds it checks whether the config and
all three cert files exist and parse. Once they do it transitions
one-way to **State B** - the full runtime (HTTPS mTLS server +
heartbeat sender). If you ran `bootstrap` while `serve` was already
running, no restart is needed; the next poll picks up the new files.

The transition is one-way per process lifetime: cert material lost
mid-run is a heartbeat failure, not a regression to State A. Restart
the process to recover. Override the config path with
`--config <path>` (default `/etc/otherix/agent.yaml`).

## 4. Verify

The node row arrives in `pending` and flips to `ready` after the first
heartbeat lands.

```bash
otherix node list
```

```text
NAME    ARCH   STATUS  CORDONED  AGE
node-1  arm64  ready   no        20s
```

```bash
otherix node get node-1
```

`node list` and `node get` are read-only for every authenticated role.
admin / operator callers see the full projection (migration capability,
hardware inventory from heartbeat); developer / viewer callers see the
reduced `NodeSummary` shape. The positional for `node get` is a node
**name** - UUID literals are rejected with `400 validation_failed`.
Both accept `--output json`; `--show-ids` surfaces the UUIDs the table
hides. `node list` filters with `--architecture` and `--status`.

## Day-2: cordon, uncordon, delete

Node creation, cordon / uncordon, and delete are **not** CLI commands -
they are control-plane API operations:

- `POST /v1/nodes/{id}/cordon` and `POST /v1/nodes/{id}/uncordon` -
  sync, idempotent. Cordoning excludes a node from VM placement
  without evicting running VMs.
- `DELETE /v1/nodes/{id}` (admin only) - refuses with `409 conflict`
  when the node hosts VMs or active migrations unless called with
  `?force=true`.

Maintenance is the `cordoned` state in the node status, not a separate
flag.

## See also

- [Create and manage VMs](create-and-manage-vms.md)
- [Console and logs](console-and-logs.md)
- [Storage pools](storage-pools.md)
