# Running Otherix on macOS for development

Otherix does not have a native macOS agent. For local development on
macOS, run the standard Linux agent inside a [Lima](https://lima-vm.io)
VM.

The control plane (`otherix-api`) is plain Go and runs natively on
macOS without help from Lima. Only the agent needs Linux. The dev
pipeline below automates VM creation, cross-compilation, и agent
service management. Cluster CA + CP server cert auto-generate inside
the CP on first boot; the agent picks up its mTLS material via the
join-token bootstrap protocol orchestrated by `make seed-mvp`.

## Prerequisites

- macOS 13 (Ventura) or later. Older versions have limited Lima
  support.
- Apple Silicon (M1/M2/M3+) or Intel Mac with virtualization
  extensions. Apple Silicon is recommended.
- [Homebrew](https://brew.sh).
- [Lima](https://lima-vm.io): `brew install lima`.

### Nested KVM and CPU choice

The agent uses KVM, which inside Lima requires nested virtualization.
Whether nested virtualization is available depends on your macOS
version, your Lima version, and the VM type Lima picks (`vz` vs
`qemu`).

- **M3 / M3 Pro / M3 Max and later** with macOS 14+: `vz` mode
  exposes nested virtualization. Lima's default works; `/dev/kvm`
  appears inside the VM.
- **M1 / M2** with `vz` mode: no nested virtualization is exposed by
  Apple's Hypervisor.framework. The agent provisioner detects this
  and prints a warning; QEMU falls back to TCG (software emulation),
  which is functional for stub-level Iteration 0 work but too slow
  for real VM workloads in Iteration 1+. Switch Lima to `qemu` mode
  if you need KVM (`limactl edit otherix-dev` and set
  `vmType: qemu`), at the cost of slower VM I/O.
- **Intel Macs**: nested virtualization is not exposed; same TCG
  fallback applies.

`dev/lima/otherix-dev.yaml` does **not** lock `vmType` so Lima can
pick the best mode per host. If `ls /dev/kvm` inside the VM reports
the device is missing and you need real KVM, see Lima's
[VM type documentation](https://lima-vm.io/docs/config/vmtype/) and
edit the VM template.

## Setup

```bash
# 1. Start dev dependencies (Postgres) and apply migrations natively.
make dev-up
make migrate-up

# 2. Stage the Lima VM (Ubuntu 24.04, native arch): provision qemu /
#    dirs / systemd unit, cross-build agent, copy binary + config
#    into the VM. The agent is NOT started — the join-token bootstrap
#    flow (Step 5) needs to provision bootstrap.env + token first.
make bootstrap-dev

# 3. Run the control plane natively on macOS. On first boot it
#    auto-generates the cluster CA + а per-replica CP server cert.
#    The dev config flips agent_client.enabled=true so the
#    in-process workers can dispatch к the agent over mTLS.
#
#    First-time only: provide bootstrap admin credentials so the CP
#    seeds the admin user row на initial migration.
export OTHERIX_BOOTSTRAP_ADMIN_EMAIL=admin@otherix.local
export OTHERIX_BOOTSTRAP_ADMIN_PASSWORD='correct-horse-battery-staple'
make run-api-dev

# 4. (separate terminal) bootstrap the agent — mints а join token via
#    the CLI, provisions bootstrap.env + token plaintext к the Lima
#    VM, starts the agent, и waits для the CP-side `nodes` row к
#    appear after the bootstrap protocol commits. Idempotent — re-
#    runs revoke the previous CLI token и mint а fresh one.
#
#    Re-export the admin credentials if running в а fresh shell:
export OTHERIX_BOOTSTRAP_ADMIN_EMAIL=admin@otherix.local
export OTHERIX_BOOTSTRAP_ADMIN_PASSWORD='correct-horse-battery-staple'
make seed-mvp

# 5. Verify the agent is reachable. The heartbeat path is the
#    canonical reachability proof — once seed-mvp finishes the node
#    flips к `ready` within а heartbeat cycle (15s в dev).
./bin/otherix node list
# NAME      ARCHITECTURE  STATUS  CORDONED  AGE
# node-mvp  arm64         ready   no        20s

# 6. Daily redeploy after agent code changes (cross-build + copy + restart).
#    No cert material needs к be re-provisioned — bootstrap is а
#    one-time event и the cert material survives restarts.
make deploy-dev

# 7. Tail agent logs from inside the VM.
limactl shell otherix-dev sudo journalctl -u otherix-agent -f

# 8. Tear down (stops + deletes the Lima VM; CP cert + cluster CA
#    persist в Postgres until `make db-reset`).
make clean-dev
```

The Lima VM is named `otherix-dev`. Inside the VM the agent reads its
config от `/etc/otherix/agent.yaml` и persists its cert material к
`/opt/otherix/certs/` (filesystem convention — `/opt/otherix/` for
runtime state, `/etc/otherix/` для operator-provided config).

## Verifying CP↔agent connectivity

The heartbeat path is the canonical reachability proof — `otherix node
list` shows the live state of every registered node. Once `make
seed-mvp` finishes, the agent's first heartbeat lands within а cycle
(15s в dev) и the row flips к `ready`:

```bash
./bin/otherix node list
# NAME      ARCHITECTURE  STATUS  CORDONED  AGE
# node-mvp  arm64         ready   no        20s
```

If the node lingers в `pending` past ~30s, inspect the agent journal:

```bash
limactl shell otherix-dev sudo journalctl -u otherix-agent --no-pager | tail -50
```

Common boot patterns to look for:

- `agent: bootstrap complete` — first-boot bootstrap landed successfully.
- `agent: using existing cert material` — subsequent boot,
  reusing `/opt/otherix/certs/agent.{crt,key}`.
- `bootstrap protocol:` — bootstrap-side error. Token may be expired,
  CA fingerprint may not match, или CP unreachable.
- `partial bootstrap state` — fatal. Cert OR key file missing; delete
  the orphan и mint а fresh token.

A direct mTLS dial is also possible via а CP-issued cert, но requires
extracting the per-replica CP cert from the local cache (when enabled)
OR an `openssl s_client` дамп. The forthcoming CP-mediated `otherix
node ping <name>` command (ROADMAP Planned) replaces this need —
а direct ping-agent operator-workstation flow was removed в Step 4
because the inter-step CP cert lifecycle (per-replica certs signed by
the cluster CA, kept replica-local) made out-of-band distribution
к the operator workstation impractical.

## Iteration 3 Phase C — `otherix vm` subcommands

After Phase C lands, operator-driven VM management через CLI is
available. The CLI talks к the Control Plane's `/v1/vms` surface
over HTTP с a bearer token.

**Prerequisites:**

1. The Control Plane is running и has at least one admin user. The
   bootstrap path (CLAUDE.md "Authentication" section) seeds an admin
   when `OTHERIX_BOOTSTRAP_ADMIN_EMAIL` + `OTHERIX_BOOTSTRAP_ADMIN_PASSWORD`
   are set on first start.
2. A node is registered, has a storage pool, and a template with a
   matching storage_image has been imported. (Phase D будет document
   the manual seed sequence end-to-end.)
3. An access token in hand. Two options:

```bash
# Option A — JWT access token (15-min default TTL).
TOKEN=$(curl -s -X POST http://localhost:8080/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"…"}' \
  | jq -r .access_token)

# Option B — long-lived API token (no expiry by default).
# Requires a JWT first к authenticate the create call.
JWT=$(... see Option A ...)
TOKEN=$(curl -s -X POST http://localhost:8080/v1/users/me/api-tokens \
  -H "Authorization: Bearer $JWT" \
  -H 'Content-Type: application/json' \
  -d '{"name":"cli","description":"operator workstation"}' \
  | jq -r .plaintext_token)

export OTHERIX_API_TOKEN="$TOKEN"
```

**Subcommands:**

```bash
# Create a VM (async; returns task id).
./bin/otherix vm create \
  --name demo-vm \
  --template-id <template-uuid> \
  --pool-id <pool-uuid> \
  --vcpus 2 --memory-mb 2048 \
  --wait
# created task=<task-uuid> status=pending
# .....
# vm running task=<task-uuid>

# List VMs.
./bin/otherix vm list
# ID                                    NAME      STATUS   POOL                                  TEMPLATE
# <vm-uuid>                             demo-vm   running  <pool-uuid>                           <template-uuid>

# Get a single VM.
./bin/otherix vm get <vm-uuid>
# id: <vm-uuid>
# name: demo-vm
# status: running
# ...

# Delete (с confirmation prompt unless --force).
./bin/otherix vm delete <vm-uuid> --wait --force
# deleted task=<task-uuid> status=pending
# .....
# vm deleted task=<task-uuid>
```

**Output formats:** `--output json` (`get`, `list`) emits the raw
envelope для programmatic consumers; default is multi-line key=value
для `get` и aligned table для `list`.

**Authentication:** `--token` flag overrides `$OTHERIX_API_TOKEN`.
Either source MUST be set; the CLI exits 1 with a usage error
otherwise. Both JWT и `otx_*` API tokens are accepted by the CP's
Authn middleware. The kubectl-style alternative `otherix config add
cluster` (see "CLI configuration" below) stores а long-lived token
on disk so subsequent invocations need neither `--token` nor
`OTHERIX_API_TOKEN`.

**Error classification (parseable from shell):** `api_error: <code>:
<message>` for CP-side failures (vm_not_found, template_not_accessible,
node_not_ready, qemu_spawn_failed, …), `request_timeout`,
`connection_refused`, `request_failed` for transport failures.

## CLI configuration

`otherix config` manages а kubectl-style credential store at
`~/.otherix/config` (or `$OTHERIX_CONFIG`). After а one-time
`otherix config add cluster` the operator can drop `--token` и
`--endpoint` from every subsequent invocation — the stored
(server, token) pair is the default.

### Initial setup

```bash
$ otherix config add cluster \
    --name production \
    --server http://localhost:8080 \
    --login admin@otherix.local \
    --password 'correct-horse-battery-staple'
cluster added: name=production server=http://localhost:8080 current=true
```

Missing flags trigger interactive prompts when stdin is а TTY (the
password prompt is masked via `golang.org/x/term`). In non-TTY
contexts (CI, scripts) every required value must come from а flag
or env var (`OTHERIX_SERVER`, `OTHERIX_LOGIN`, `OTHERIX_PASSWORD`).

### Daily usage

```bash
# Uses the current cluster automatically.
$ otherix vm list
ID                                    NAME     STATUS   POOL                                  TEMPLATE
…

# Override per-invocation (no persistent state change).
$ otherix --cluster localdev vm list
```

### Multi-cluster management

```bash
$ otherix config list
NAME        SERVER                       CURRENT
production  https://otherix.example.com  *
localdev    http://localhost:8080  

$ otherix config use localdev
current cluster: localdev

$ otherix config show              # current cluster, token masked
$ otherix config show production --show-token   # reveal plaintext

$ otherix config remove localdev --force         # skips confirmation
```

`config remove` with stdin attached к а TTY prompts for `y/N`;
`--force` is required for non-interactive removal.

### Config file location

Resolved in this order — the first match wins:

1. `--config <path>` flag
2. `$OTHERIX_CONFIG`
3. `~/.otherix/config` (default)

The file is а small YAML document (apiVersion / kind / clusters /
current-cluster) written с 0600 file и 0700 parent-directory
perms. Atomic write (sibling temp + fsync + rename) means а crash
mid-`config add cluster` cannot leave а half-written credential
store.

`XDG_CONFIG_HOME` is intentionally NOT consulted — kubectl и
docker both pick а tool-specific home-relative path и we follow,
keeping "where is my config?" а one-answer question.

### Authentication precedence

`--endpoint` и `--token` are resolved independently — each follows
its own chain:

| Layer | endpoint | token |
|---|---|---|
| 1 | `--endpoint` | `--token` |
| 2 | `$OTHERIX_SERVER` | `$OTHERIX_API_TOKEN` |
| 3 | named cluster (`--cluster`) | named cluster (`--cluster`) |
| 4 | current-cluster | current-cluster |
| 5 | fail с "no endpoint configured" | fail с "no token configured" |

The two layers can come from different sources — а typical CI
flow sets `OTHERIX_API_TOKEN` from а secret и `--endpoint` from
а config repo.

**Backward compat:** Phase C scripts that set only
`OTHERIX_API_TOKEN` против а local CP now need а matching
`OTHERIX_SERVER=http://localhost:8080` (или a `config use` of
а pre-seeded cluster). The implicit "default К localhost" of
the old vm-level `--endpoint` flag was dropped — the new model
refuses to guess.

## CLI resource discovery

Phase 2 of the name-based references work introduced read-only
discovery commands so operators can resolve names before submitting
`vm create`. Each command supports `--output table|json|text` (defaults
vary), cursor pagination via `--limit` / `--cursor`, и `--show-ids`
к surface the UUIDs the table normally hides.

### Templates

```bash
$ otherix template list
NAME            ARCHITECTURE  OS_FAMILY  VISIBILITY  AGE
ubuntu-jammy    arm64         linux      public      2d
ubuntu-noble    arm64         linux      public      1d

$ otherix template list --visibility public --arch arm64

$ otherix template get ubuntu-jammy
name: ubuntu-jammy
visibility: public
architecture: arm64
os_family: linux
image_url: https://cloud-images.ubuntu.com/jammy/current/jammy-arm64.img
image_checksum_sha256: <hex>
default_cpu_cores: 2
default_memory_mib: 2048
...
```

### Storage pools

Phase 1.5 multi-instance reality: the same pool name may live on
multiple nodes, and `pool list` exposes one row per per-node
instance. The DEFAULT column flips on когда the pool name matches
`cluster_settings.default_pool_name`.

Full CRUD surface:

```bash
$ otherix pool list
NAME      NODE      TYPE       PATH                           AVAILABLE  DEFAULT  AGE
default   node-a    local_dir  /opt/otherix/pools/default     80.0GiB    yes      5d
default   node-b    local_dir  /opt/otherix/pools/default     95.0GiB    yes      5d
fast-ssd  node-a    local_dir  /opt/otherix/pools/fast        400.0GiB            3d

# Aggregated cluster-wide view by name
$ otherix pool get default
name: default
type: local_dir
is_cluster_default: true
instances:
  - node: node-a
    id: <uuid>
    path: /opt/otherix/pools/default
    available: 80.0GiB
  - node: node-b
    id: <uuid>
    path: /opt/otherix/pools/default
    available: 95.0GiB

# Flat instance view by UUID
$ otherix pool get <pool-uuid>
id: <pool-uuid>
name: default
node: node-a
type: local_dir
path: /opt/otherix/pools/default
available_bytes: 80.0GiB
is_cluster_default: true
...

# Register a pool on a node (admin-only)
$ otherix pool create fast-ssd --node node-a --path /opt/otherix/pools/fast
pool fast-ssd created on node node-a
type: local_dir
path: /opt/otherix/pools/fast

# Delete a pool (refuses когда vm_disks или storage_images reference it)
$ otherix pool delete fast-ssd --force
pool fast-ssd deleted
```

One `pool create` invocation registers one (name, node) row — re-run
the command per node к fan а pool name out across the cluster.
Deletion has no `--force-cascade`; the operator must remove
dependent disks/images first.

**Transitional note:** `seed-mvp.sh` continues к use direct SQL INSERT
для initial pool registration — public pool-registration API is not
yet exposed (tracked в ROADMAP Planned). The agent's pool registry
is name-keyed и populated through reconciliation от CP (the `pools:`
block в `agent.yaml` is eliminated), so CLI-created pools work
end-to-end including agent-side image import + vm-disk allocation.

### Nodes

```bash
$ otherix node list
NAME      ARCHITECTURE  STATUS  CORDONED  AGE
node-a    arm64         ready   no        7d
node-b    arm64         ready   no        7d
node-c    amd64         ready   yes       5d

$ otherix node list --architecture arm64 --status ready

$ otherix node get node-a
name: node-a
architecture: arm64
status: ready
migration_host: 10.0.0.10
migration_port_range: 49152-49251
cpu_cores_total: 32
memory_total_mib: 65536
...
```

admin / operator callers see the full projection (above); developer /
viewer callers see а reduced shape (no migration capability, no
hardware inventory) — the CLI text renderer only prints fields the
wire envelope populated, so the reduced shape is clean.

## Cluster configuration

Phase 1.5 surfaced а cluster-wide default-pool reference (the pool
name VM create falls back to когда the request omits `--pool`).
Three commands manage it. The mutating verbs require admin
(`cluster:manage`); operators / others receive `403 permission_denied`
verbatim.

```bash
# Inspect the current default
$ otherix cluster get-default-pool
default-pool: default

# When unset, exit 0 with а parseable informational line
$ otherix cluster get-default-pool
no default pool configured (run 'otherix cluster set-default-pool <name>' to configure)

# Promote a different pool (server validates the name exists)
$ otherix cluster set-default-pool fast-ssd
default pool set to 'fast-ssd'

# Clear the default — interactive prompt unless --force
$ otherix cluster unset-default-pool --force
default pool cleared
```

## VM operations with names

Every VM command takes the VM name in positional / flag inputs; UUID
literals are rejected by the server with 400 validation_failed.

```bash
# Create — uses cluster default pool, scheduler picks node
$ otherix vm create \
    --name demo-vm \
    --template ubuntu-jammy \
    --vcpus 2 --memory-mb 2048 \
    --wait

# Create — explicit pool + node placement hint
$ otherix vm create \
    --name pinned-vm \
    --template ubuntu-jammy \
    --pool fast-ssd \
    --node node-a \
    --vcpus 4 --memory-mb 4096

# Inspect by name
$ otherix vm get demo-vm

# List — UUIDs hidden by default
$ otherix vm list
NAME       STATUS   POOL      TEMPLATE
demo-vm    running  default   ubuntu-jammy
pinned-vm  running  fast-ssd  ubuntu-jammy

# List with UUIDs (--show-ids)
$ otherix vm list --show-ids
ID                                    NAME       STATUS   POOL      TEMPLATE
<uuid>                                demo-vm    running  default   ubuntu-jammy
<uuid>                                pinned-vm  running  fast-ssd  ubuntu-jammy

# Delete by name (interactive prompt без --force)
$ otherix vm delete demo-vm --wait --force
```

## VM placement scheduler

`otherix vm create` reaches the api-server's vm.create handler, which
runs `internal/scheduler.SchedulePlacement` к pick the (node, pool
instance) target. Two algorithms ship:

- **`resource_aware`** (default) — kubernetes-style LeastAllocated
  scoring across `cpu_cores_available` и `memory_available_mib` от
  heartbeat. Lower post-placement utilization wins.
- **`least_vm_count`** — Phase 1.5 fallback. Picks the node с the
  fewest pinned VMs. Operator opt-out.

Both algorithms break ties by node name lowercase lexicographic.

Switch via api config:

```yaml
# dev/config/api.yaml or deploy/config/api.example.yaml
placement:
  algorithm: "resource_aware"   # or "least_vm_count"
```

Invalid values fail at startup. Operator visibility into placement
inputs:

```bash
# Heartbeat metrics underpin the fit check / scoring.
$ otherix node list
NAME      ARCH    STATUS  CORDONED  LAST_HEARTBEAT  AGE
node-dev  arm64   ready   false     8s ago          1h

# `node get` shows per-node CPU / memory totals и live availability —
# the same view the scheduler sees.
$ otherix node get node-dev
...
hardware:
  cpu_cores: used 2/4 cores
  memory:    used 6144/16384 MiB
agent:
  last_heartbeat_at: 2026-05-12T19:42:11Z (8s ago)
```

When no node has sufficient resources, vm create returns 409 с а
structured payload (`details.reason="insufficient_resources"`) listing
each candidate's utilization by name — actionable for capacity
diagnostics.

Disk-aware filtering shipped as three sub-iterations on 2026-05-12:
A (periodic pool-scan worker; default 15 min, dev compressed к 1 min);
B (`pool_effective_capacity` view с pending-disk subtraction);
C (scheduler integration). Placement decisions now consider CPU +
memory + disk all с effective accounting. `otherix vm create` against
а pool that lacks free disk space returns 409 `no_eligible_nodes`
с the per-pool (`pool`) name и `disk_used_bytes` / `disk_total_bytes`
populated в `details.node_utilization`. On-demand
`otherix pool scan <name>` is still available for immediate refresh;
Tunable via `workers.storage_pool_scan` в the api config.
`otherix pool {get,list}` continues к render а "(effective N free)"
suffix on the available column when pending VM disks have not yet
been observed by а scan, parallel к the node CLI's
"(effective N free)" rendering.

Per-resource placement settings (cpu / memory / disk × `enabled` +
`overcommit_ratio`) сидят под `placement.resources` в api config —
strict no-overcommit defaults в dev, same shape annotated с safety
notes в `deploy/config/api.example.yaml`. Memory overcommit risks,
host configuration prerequisites (`vm.overcommit_memory` sysctl, swap
sizing), recommended ratios per use case, и disk overcommit
considerations are documented в `docs/scheduler-configuration.md`.
The api binary emits slog Warn lines at startup for each overcommit-
enabled resource (`placement.resources.memory.overcommit_ratio=1.50
— overcommit enabled (OOM kill risk under memory pressure); see
docs/scheduler-configuration.md`) и для the all-disabled fallback
case — surfaced on each restart as а reminder of the trade-off.

Node-pressure detection layers on top of the capacity / overcommit
settings: pressured nodes (or pools, для disk pressure) are excluded
от placement entirely. Three pressure types operational:

- **memory** — per-node, heartbeat-driven (10%, 3 heartbeats default).
- **system_disk** — per-node, heartbeat-driven (10%, 3 heartbeats
  default). Agent reads root filesystem via `syscall.Statfs("/")`.
- **disk** — per-pool, scan-driven (15%, single scan default).

Tunable via `placement.pressure.{memory,system_disk,disk}.{enabled,
threshold_percent,consecutive_required}`. The api binary emits slog
lines on set / clear transitions for all three (Warn on set, Info on
clear). CLI surfaces conditions в three places:

- `otherix node list` STATUS column combines raw status с node-scoped
  pressure (memory + system_disk) into `ready` / `under_pressure` /
  `cordoned, under_pressure` / `unreachable`.
- `otherix node get <node>` renders а `pressure:` section с per-
  condition state (memory + system_disk).
- `otherix pool list` adds an independent pool STATUS column;
  `otherix pool get <pool>` renders а `pressure:` section с the
  `disk:` condition.

Shared filesystem (typical homelab — pool на same FS as `/`) causes
both `system_disk_pressure` и pool `disk_pressure` к fire on the same
condition. Both surface — this is accurate, не duplication. Operator
guide at `docs/scheduler-configuration.md`.

## Iteration 3 Phase D — MVP end-to-end smoke test

Phase D ships the operator workflow that lets you create a real
Ubuntu VM end-to-end on real hardware: Lima VM (the agent host) →
`otherix vm create` → qemu boots → SSH login. This section walks
through the full sequence.

### Prerequisites

1. Steps 0+1+2 of this doc completed (Lima VM running, agent built,
   mTLS certs generated).
2. Local Postgres up and migrated:
   ```bash
   make dev-up
   make migrate-up
   ```
3. Bootstrap admin seeded — set the env vars BEFORE the first api
   start, then start the api с the dev config:
   ```bash
   export OTHERIX_BOOTSTRAP_ADMIN_EMAIL=admin@otherix.local
   export OTHERIX_BOOTSTRAP_ADMIN_PASSWORD='correct-horse-battery-staple'
   make run-api-dev
   ```
   `run-api-dev` uses `dev/config/api.yaml` — the workers (vm.create
   / vm.delete / storage_pool.scan / storage_image.import) dispatch
   end-to-end against the real agent (mTLS material auto-managed via
   the api config's `cp_cert` block). The production `make run-api` target keeps
   `agent_client.enabled: false` by default and refuses к start с
   `workers.enabled: true` until operators provision real mTLS material
   (Phase 2 lock).

   The admin user lives in the `users` table with `role='admin'` so
   subsequent API calls authenticate.

### Step 1 — run seed-mvp

```bash
make seed-mvp
```

The target executes `dev/scripts/seed-mvp.sh`, which orchestrates the
bootstrap flow end-to-end:

1. Configures the CLI cluster — calls `otherix config add cluster
   --force` using the bootstrap admin credentials (same env vars the
   CP boot hook consumes). Persists а long-lived API token into
   `~/.otherix/config`.
2. Mints а join token via `otherix node join-token create
   --node-name node-mvp --ttl 10m --output json`. Captures the token
   plaintext + active cluster CA fingerprint.
3. Provisions the agent host: writes `/etc/otherix/bootstrap-token`
   (mode 0600) + `/etc/otherix/bootstrap.env` (mode 0644, contains
   `OTHERIX_BOOTSTRAP__*` koanf env-var overrides). Linux native
   user-mode lays these out under `~/.config/otherix/` instead.
4. Starts the agent — `systemctl restart otherix-agent` (Lima) или
   `systemctl --user start otherix-agent` (Linux native).
5. Polls для `nodes.id WHERE name='node-mvp'` for up к 60s. The row
   appears once the CSR redemption commits at the CP side.
6. Inserts the `storage_pools` row directly via psql (no public
   registration API yet — tracked в ROADMAP Planned). FK к the
   bootstrapped `nodes.id`.
7. Calls `otherix cluster set-default-pool pool-mvp` so subsequent
   `vm create` invocations resolve без `--pool`.
8. Calls `otherix template create ubuntu-noble-<arch>-mvp --pool
   pool-mvp --wait` which both registers the template и triggers
   the agent-side image import (compute-mode SHA — no pre-computed
   checksum required from the operator).

The node row arrives в `pending` status и flips к `ready` once the
first heartbeat lands. The dev heartbeat cadence (15s interval / 45s
stale threshold) makes the transition operator-friendly. Watch with
`./bin/otherix node list`.

Sample output:
```
>> seed-mvp complete
   node     : node-mvp (id=<uuid>)
   pool     : pool-mvp (id=00000000-…-000000000001, default=yes)
   template : ubuntu-noble-arm64-mvp
```

### Step 2 — create a VM

The CLI flags are `--template` / `--pool` (renamed from
`--template-id` / `--pool-id` by the name-based references series).
After Phase 3 the values accept resource names only — UUID literals
surface as `400 validation_failed` с an actionable hint pointing к
`otherix <resource> list`. Storage pool addressing retains а UUID
escape hatch для multi-instance per-node row addressing. Phase 1.5
made `--pool` optional — when omitted, the server resolves the cluster
default-pool reference held in `cluster_settings`. The dev seed
configures `pool-mvp` as the cluster default, so the command below
works without `--pool`:

```bash
./bin/otherix vm create \
    --name demo-vm \
    --template ubuntu-noble-arm64-mvp \
    --vcpus 2 --memory-mb 2048 --wait
# created task=<task-uuid> status=pending
# .....
# vm running task=<task-uuid>
```

To target a specific node explicitly:

```bash
./bin/otherix vm create \
    --name demo-vm \
    --template ubuntu-jammy-arm64-mvp \
    --pool pool-mvp \
    --node node-mvp \
    --vcpus 2 --memory-mb 2048 --wait
```

The scheduler picks among ready, uncordoned nodes hosting the pool
name and uses Least-VM-count tie-breaking. A `--node` hint pins
placement к exactly that node; mismatch (pool not on hinted node)
surfaces as 409 `pool_not_on_node`.

Behind the scenes: handler enqueues `vm.create` task → river worker
loads the template/pool/node → `agentclient.PostVMCreate` over mTLS
→ agent validates the template file is on disk → agent spawns qemu →
agent's task surface flips к terminal-success → CP polls → CP
projects vm_runtime row with phase=running.

### Step 4 — observe the VM

```bash
./bin/otherix vm list
# NAME      STATUS   POOL       TEMPLATE
# demo-vm   running  pool-mvp   ubuntu-jammy-arm64-mvp

./bin/otherix vm get demo-vm
# id: <vm-uuid>
# name: demo-vm
# owner_id: <user-uuid>
# template: ubuntu-jammy-arm64-mvp
# pool: pool-mvp
# node: node-mvp
# architecture: arm64
# vcpus: 2
# memory_mb: 2048
# status: running
# desired_phase: running
# created_at: ...
# updated_at: ...
```

Pass `--show-ids` к `vm list` if the UUIDs are useful for scripting.

### Step 5 — console access

The Iteration 1 agent exposes the qemu serial console as a Unix
socket inside the Lima VM:

```bash
limactl shell otherix-dev sudo socat - UNIX-CONNECT:/opt/otherix/vms/<vm-uuid>/console.sock
```

The Ubuntu cloud image boots; press Enter to get a login prompt.
(Cloud-init authentication setup arrives in Iteration 5 — for the
MVP smoke test, the boot reaching the login prompt confirms the
chain works.)

### Step 6 — cleanup

```bash
./bin/otherix vm delete demo-vm --wait --force
# deleted task=<task-uuid> status=pending
# .....
# vm deleted task=<task-uuid>
```

Verify:
```bash
limactl shell otherix-dev pgrep -af qemu-system   # nothing
limactl shell otherix-dev ls /opt/otherix/vms/    # empty
```

### Cluster default-pool configuration

Phase 1.5 introduced a cluster-wide default-pool reference held in the
`cluster_settings` singleton — VM create requests без `--pool` resolve
through it. The seed script wires `pool-mvp` as the default; the
`otherix cluster` subcommand group exposes inspect / set / unset
verbs (Phase 2 added these — see "Cluster configuration" above for the
broader walkthrough).

```bash
# Inspect current default
./bin/otherix cluster get-default-pool
# default-pool: pool-mvp

# Promote a different pool (admin only)
./bin/otherix cluster set-default-pool fast-ssd

# Clear the default — subsequent vm create без --pool returns
# 400 default_pool_not_set until a new default is configured
./bin/otherix cluster unset-default-pool --force
```

Pool selection determines node placement — each pool instance lives
on exactly one node, so promoting `fast-ssd` to default targets
whichever node hosts the `fast-ssd` instance. To list available
instances, run `./bin/otherix pool list --node <name>` and pick the
desired identifier.

### Failure modes

| Symptom | Likely cause |
| ------- | ------------ |
| `vm get` shows `status: creating` indefinitely | agent endpoint unreachable; check Lima port-forward |
| `task.error.code = qemu_spawn_failed` | KVM unavailable inside the Lima VM (Apple Silicon vz quirk); the Iteration 1 agent automatically falls back к TCG, but a misconfigured cmdline can still fail |
| `task.error.code = template_not_found` | the `seed-mvp.sh` SHA does not match the file on disk; rerun the seed step or `limactl shell otherix-dev ls /opt/otherix/pools/default/templates/` |
| `task.error.code = node_not_ready` | a fresh `make clean-dev` flipped the node row К pending; rerun `seed-mvp.sh` |
| `api_error: unauthenticated` from the CLI | `OTHERIX_API_TOKEN` expired (JWTs are 15-min by default); re-login или use a long-lived `otx_*` API token |
| `default_pool_not_set` on `vm create` без `--pool` | cluster default-pool unset; configure via `PUT /v1/cluster/default-pool` or pass `--pool` explicitly |
| `pool_not_on_node` on `vm create --node X` | the requested pool name has no instance on node `X`; pick а different node or remove the `--node` hint |
| `no_eligible_nodes` on `vm create` | the pool exists but every hosting node is cordoned, unreachable, or not yet `ready` |

### MVP target achieved

Reaching the login prompt в Step 5 means the full chain works end-to-
end: CLI → cpclient → CP HTTP → river worker → agentclient mTLS →
agent → qemu → Ubuntu boot. Phase D's integration tests cover the
machine-checkable invariants (handler envelopes, RBAC, task
projection, idempotency, resumption); this smoke test covers the
human-facing UX и the real-hardware surfaces the integration tests
cannot reach (real qemu, real KVM/TCG fallback, real serial console).

Post-MVP iterations layer in:
- agent storage_image.import endpoints (eliminates the manual
  image-copy step);
- cloud-init с NoCloud ISO (Iteration 5);
- VM lifecycle ops beyond create/delete;
- live migration, snapshots, multi-disk;
- WebSocket console proxy (replaces the `socat` tunnel).

## Running the agent against a remote control plane

If the control plane runs somewhere other than the macOS host (a
remote dev box, a staging cluster), override the bootstrap CP URL
when running seed-mvp.sh. The agent picks it up via the koanf
`OTHERIX_BOOTSTRAP__CP_URL` env var that seed-mvp writes к
bootstrap.env on the Lima VM:

```bash
# In your shell before running seed-mvp:
export OTHERIX_CP_URL="https://<reachable-cp-host>:8443"
make seed-mvp
```

The CP server cert SAN must include the hostname or IP the agent
dials. The per-replica cert auto-detects `localhost`, `127.0.0.1`,
`os.Hostname()`, и the non-wildcard listen address; operators extend
the set via `cp_cert.additional_sans` в `dev/config/api.yaml` (or the
production config). The dev config
ships `host.lima.internal` pre-registered.

## Limitations

- **Networking.** Lima's networking model differs from native Linux
  in subtle ways. Bridges and VLANs work *inside* the Lima VM, but
  exposing them to the macOS host or to other LAN hosts requires
  additional Lima configuration (`vmnet` networks). Consult Lima's
  [networking documentation](https://lima-vm.io/docs/config/network/).
- **Performance.** VMs running under Otherix-under-Lima are three
  virtualization layers deep (macOS hypervisor → Lima VM → guest VM).
  This is fine for development; do not expect bare-metal-equivalent
  numbers.
- **Cross-platform live migration.** Migrating VMs between a
  Lima-hosted agent and a bare-metal Linux agent is technically
  possible (both sides speak the same agent protocol) but is not
  officially supported and may have edge cases that the test matrix
  does not cover.

## Alternatives

If Lima does not fit your workflow, two other ways to work with
Otherix from a Mac:

- **Run only the control plane locally; agents remotely.** The
  control plane runs natively on macOS (it is a standard REST API
  server with PostgreSQL); only the agent needs Linux. Connect to
  agents running on remote test hosts over the network.
- **Run everything in a remote dev environment.** Tools like a
  Linux dev box over SSH, GitHub Codespaces, or any Linux cloud VM
  remove the question entirely.

## Troubleshooting

### Bootstrap failures

Symptoms surface в `journalctl -u otherix-agent` after `make seed-mvp`.

| Symptom | Cause | Recovery |
|---|---|---|
| `bootstrap: CA fingerprint mismatch (expected sha256:… got sha256:…)` | Operator typo OR active MITM | Re-check the fingerprint в `~/.otherix/config` cluster CA against `bootstrap.env`. If correct, escalate к network team. |
| `bootstrap: CSR submission rejected by CP: HTTP 401 token_expired` | Token TTL elapsed (default 10m) | Re-run `make seed-mvp` — mints а fresh token. |
| `bootstrap: CSR submission rejected by CP: HTTP 401 token_exhausted` | Multi-use token cap reached | Re-run `make seed-mvp` — mints а fresh token. |
| `bootstrap: fetch /v1/ca: dial tcp …: connection refused` | CP not running OR unreachable от Lima VM | Verify `make run-api-dev` is still alive; inside Lima, `curl -k https://host.lima.internal:8443/healthz`. |
| `cert <path> exists but key <path> missing` (or vice-versa) | Partial-state bootstrap (mid-flight crash, manual file deletion) | Manual cleanup — delete cert + key + CA (`/opt/otherix/certs/agent.{crt,key}` + `ca.crt` on Lima, or `~/.config/otherix/certs/agent.*` + `ca.crt` on Linux native), then `make seed-mvp` again. Agent identity is derived от the cert CN — no node-id sidecar к clean up. |
| Node lingers в `pending` past 60s | Agent reachable but heartbeat не arriving | `limactl shell otherix-dev sudo journalctl -u otherix-agent -f` — look для `heartbeat` lines OR mTLS handshake failures. |

### Lifecycle failures (after bootstrap)

| Symptom | Likely cause |
| ------- | ------------ |
| `vm get` shows `status: creating` indefinitely | agent endpoint unreachable; check Lima port-forward |
| `task.error.code = qemu_spawn_failed` | KVM unavailable inside the Lima VM (Apple Silicon vz quirk); the agent automatically falls back к TCG, but а misconfigured cmdline can still fail |
| `task.error.code = template_not_found` | `seed-mvp` did not finish materialising the template image (`otherix template create --wait` aborted); re-run `make seed-mvp` |
| `task.error.code = node_not_ready` | а fresh `make clean-dev` removed the agent; re-run `make bootstrap-dev` + `make seed-mvp` |
| `api_error: unauthenticated` from the CLI | stored API token revoked OR cluster CA rotated; re-run `make seed-mvp` к refresh the cluster credential |
| `default_pool_not_set` on `vm create` без `--pool` | cluster default-pool unset; `otherix cluster set-default-pool <name>` или pass `--pool` explicitly |
