# Scheduler configuration guide

Otherix's VM placement scheduler ranks candidate (node, pool) targets
by a kubernetes-style LeastAllocated score across the enabled resource
dimensions. This document covers the per-resource knobs operators
expose в `placement.resources` and the safety trade-offs of memory
and disk overcommit.

Companion references:

- `deploy/config/api.example.yaml` — annotated production config.
- `dev/config/api.yaml` — strict defaults, parallel structure.

## Per-resource placement settings

Each of `cpu`, `memory`, и `disk` is configured independently:

```yaml
placement:
  algorithm: "resource_aware"   # or "least_vm_count"
  resources:
    cpu:
      enabled: true
      overcommit_ratio: 1.0
    memory:
      enabled: true
      overcommit_ratio: 1.0
    disk:
      enabled: true
      overcommit_ratio: 1.0
```

- **`enabled`** (default `true`) — include the resource в the fit check
  AND scoring formula. Disabling drops the resource from both. The
  LeastAllocated denominator floats with the count of enabled
  dimensions, so disabling one does not bias the score scale.

- **`overcommit_ratio`** (default `1.0`) — multiplier on the per-
  resource effective availability:
  - `1.0` — strict, no overcommit (production-safe default).
  - `> 1.0` — overcommit, capacity inflated.
  - `< 1.0` — reserve headroom, capacity deflated.

  Validation rejects `overcommit_ratio <= 0` at startup. Validation
  warnings — not errors — are logged at startup for any `> 1.0`
  configuration so operators see the trade-off they opted into on each
  api-server restart.

## Startup warnings

`otherix-api` emits а `slog` Warn line at startup for each per-resource
overcommit and for the all-resources-disabled scenario:

```
WARN placement config warning="placement.resources.memory.overcommit_ratio=1.50 — overcommit enabled (OOM kill risk under memory pressure); see docs/scheduler-configuration.md"
```

Ratios above `2.0` use stronger "extreme overcommit" language. All
three resources disabled emits а separate warning that scoring degrades
к count-based fallback (Least-VM-count parity без switching the cluster
algorithm).

Default config (every resource enabled at ratio 1.0) emits zero
warnings — the absence on subsequent restarts confirms the cluster is
running с strict accounting.

## Memory overcommit safety

`memory.overcommit_ratio > 1.0` enables placement decisions that pack
VMs whose combined memory **requests** exceed physical host RAM. This
is dangerous:

- VMs allocate memory lazily. Actual usage trails allocation, so
  combined working sets often fit even при aggressive overcommit.
- BUT when cumulative actual usage exceeds (physical RAM + swap), the
  Linux OOM killer reaps а process — typically QEMU — to recover
  memory. Affected VMs crash abruptly.
- No graceful degradation. The VM's guest OS does not get notified;
  qemu just disappears.

### Host configuration before enabling memory overcommit

1. **Provision adequate swap.** Rule of thumb:
   `swap ≥ (overcommit_ratio - 1) × RAM`. Example: 16 GiB RAM с ratio
   1.5 → at least 8 GiB swap recommended. Swap I/O is far slower than
   RAM, so this is а safety net, not а performance feature.

2. **Review the `vm.overcommit_memory` sysctl** on each agent host:
   - `0` (default, heuristic) — kernel decides per allocation. Usually
     fine for moderate ratios (≤ 1.5) on well-provisioned hosts.
   - `1` (always allow) — kernel never refuses an allocation. Risky;
     defers OOM to actual fault time.
   - `2` (strict accounting) — kernel refuses allocations that would
     push commit charge past `swap + vm.overcommit_ratio% × RAM`.
     Predictable но requires careful sizing of the kernel
     `vm.overcommit_ratio` sysctl (distinct from Otherix's per-resource
     `overcommit_ratio` config).

   The conservative choice for production-ish Otherix homelabs is
   either `0` или `2` с the kernel sysctl deliberately tuned. Do not
   set `1`.

3. **Monitor swap usage** continuously. Frequent swap activity is а
   leading indicator of memory pressure approaching kernel-OOM
   thresholds. Plumb host swap into your metrics pipeline before
   enabling overcommit.

### Recommended ratios

| Use case                          | CPU | Memory | Disk |
|-----------------------------------|----:|-------:|-----:|
| Production-grade isolation        | 1.0 |    1.0 |  1.0 |
| Homelab dev VMs (idle-heavy)      | 2.0 |    1.2 |  1.1 |
| Testing / experiments / scratch   | 4.0 |    1.5 |  1.5 |

CPU overcommit at typical ratios (≤ 4.0) is benign — Linux schedulers
handle vCPU oversubscription gracefully. The performance trade-off
(context-switch overhead, noisy neighbour) is recoverable.

Memory overcommit above 1.5 is **not recommended** outside throwaway
test fleets. Disk overcommit above 1.5 is risky for any VM whose disks
may grow toward their allocated capacity.

## Disk overcommit considerations

`disk.overcommit_ratio > 1.0` lets Otherix over-subscribe pool capacity
based on the assumption that sparse qcow2 disks consume less than their
allocated size. Risks:

- VMs that write heavily can grow toward their allocated capacity.
  Multiple VMs hitting that limit simultaneously can fill the pool.
- Pool scans (default 15 min, configurable via
  `workers.storage_pool_scan`) detect filling pools но do not prevent
  placement decisions made before the scan lands.
- Cascading "no space left on device" errors corrupt guest filesystems
  and can wedge running VMs.

The `pool_effective_capacity` view already subtracts pending-VM disk
commitments from scan-reported availability, so the scheduler never
double-counts. Disk overcommit explicitly opts out of that safety и
trusts the operator's growth model.

Safe disk overcommit requires:

1. Sparse qcow2 disk format (Otherix default).
2. Pool monitoring с alerting before fill.
3. Awareness of typical workload growth patterns в the cluster.

## Disabling resources entirely

Setting `enabled: false` removes the resource from placement
consideration:

- Fit check skipped — no constraint on the dimension.
- Scoring formula adjusts — the denominator drops by one. Remaining
  enabled dimensions are scored at full magnitude (no scale shift).
- All three disabled → scoring degrades к count-based (LeastAllocated
  with zero enabled dimensions is undefined; the scheduler falls back
  к Least-VM-count parity, which is the same algorithm `least_vm_count`
  applies cluster-wide).

When to disable:

- **CPU `enabled: false`** — VMs are mostly idle and memory/disk are
  the binding constraints. Rare; CPU is usually the cheapest signal
  к keep.
- **Memory `enabled: false`** — almost never. Memory pressure is the
  most common failure mode на Linux VM hosts.
- **Disk `enabled: false`** — shared network storage where per-pool
  capacity reflects а pooled remote resource rather than node-local
  bytes. Pool capacity tracking still happens; just doesn't influence
  placement.

## Node-pressure detection

Independent of the capacity / overcommit knobs above, the scheduler
honours pressure conditions surfaced under `placement.pressure.*`.
Pressure detection excludes stressed nodes (or pools, для disk
pressure) от placement — а hard constraint, no operator override
в MVP. Three pressure types land together с Sub-iteration B:

- **memory** — per-node, heartbeat-driven. Free memory below
  threshold. OOM-kill risk.
- **system_disk** — per-node, heartbeat-driven. Free root-filesystem
  bytes below threshold. Agent crash / log truncation risk.
- **disk** — per-pool, scan-driven. Free pool bytes below threshold.
  Pool-scoped — other pools на the same node remain eligible.

### Memory pressure

```yaml
placement:
  pressure:
    memory:
      enabled: true              # disable to skip detection entirely
      threshold_percent: 10      # flag when free memory < 10% of total
      consecutive_required: 3    # ~90s sustained at default heartbeat cadence
```

Mechanics:

- **CP-side computation.** The api-server compares
  `memory_available_mib / memory_total_mib` reported в every
  heartbeat against `threshold_percent`. The agent does not
  pre-compute pressure — operators tune one set of thresholds в the
  api config rather than per-agent.
- **Asymmetric debouncing.** Pressure is set after
  `consecutive_required` consecutive below-threshold observations
  (default ≈ 90 s at the 30 s heartbeat cadence). Recovery is
  immediate — the first at-or-above-threshold observation clears the
  flag и resets the counter. Slow-to-set / fast-to-clear prevents
  flapping on transient spikes while restoring eligibility promptly.
- **Hard exclusion.** Pressured nodes are filtered out of
  `ListEligiblePoolsByName`. The 409 `no_eligible_nodes` envelope
  carries `details.reason: "node_pressure"` и а
  `filtered_due_to_pressure` list when pressure accounts for the
  exclusion.

### System disk pressure

```yaml
placement:
  pressure:
    system_disk:
      enabled: true              # disable to skip detection entirely
      threshold_percent: 10      # flag когда free root-fs bytes < 10% of total
      consecutive_required: 3    # ~90 s sustained at default heartbeat cadence
```

Mechanics mirror memory pressure exactly — same heartbeat-driven CP-
side computation, same asymmetric debouncing, same hard-exclusion
semantics. Only the raw metric differs: the agent reads
`syscall.Statfs("/")` on every heartbeat и reports
`system_disk_total_bytes` / `system_disk_available_bytes` (both
nullable когда the syscall fails — pressure state carries forward
across NULL observations). А system_disk-pressured node is excluded
от placement entirely; agent logs / NVRAM allocation / QMP sockets
all live on the root filesystem, so disk exhaustion threatens the
agent itself rather than just any individual VM.

### Pool disk pressure

```yaml
placement:
  pressure:
    disk:
      enabled: true              # disable to skip detection entirely
      threshold_percent: 15      # flag когда free pool bytes < 15% of capacity
      consecutive_required: 1    # single scan sets pressure (scans are 15 min apart)
```

Mechanics:

- **Scan-driven CP computation.** The scan worker reads pool
  `available_bytes` / `capacity_bytes` from the agent's scan response
  и runs the same transition function inside the same transaction
  as `UpsertStoragePoolUsage`. Scan-completion-only — heartbeat-time
  observations are not relevant for pool metrics.
- **Single-observation default.** Scans are 15 min apart (default
  `workers.storage_pool_scan.interval`), so а single sub-threshold
  observation is а sufficient signal; operators can raise
  `consecutive_required` for multi-scan debouncing if their scan
  cadence is more aggressive.
- **Pool-scoped exclusion.** Pressured pools are filtered из
  `ListEligiblePoolsByName`; other pools на the same node remain
  eligible. The 409 envelope's `filtered_due_to_pressure` payload
  carries an explicit `pool` field for these entries.

### Operator visibility

`otherix node list` shows а computed STATUS column combining raw
status с active **node-scoped** pressure conditions (memory +
system_disk):

```
ready
under_pressure                # memory OR system_disk pressure set
cordoned, under_pressure
unreachable                   # pressure data suppressed (stale)
```

`otherix node get <node>` renders а `pressure:` section с one row per
node-scoped condition:

```
pressure:
  memory:      active since 5m ago (consecutive_count=5)
  system_disk: ok
```

`otherix pool list` shows pool STATUS independently — а pool с
`disk_pressure` reads `under_pressure` regardless of its owning node's
condition:

```
NAME     NODE    AVAILABLE                STATUS
default  node-a  100 GiB                  ready
default  node-b  2 GiB                    under_pressure
```

`otherix pool get <pool>` adds а `pressure:` section с the `disk:`
condition state.

### Shared filesystem behavior

In typical homelab setups the storage pool's `path` lives on the
same filesystem as `/` — а single physical disk hosts both the
agent runtime и VM images. Когда that filesystem fills, both
`system_disk_pressure` (heartbeat-driven, node-level) и
`disk_pressure` (scan-driven, pool-level) fire on the same physical
condition. Both surface в the CLI и the envelope. This is accurate —
the resource is genuinely shared — not duplication. Multi-mount
setups (advanced) detect independently per filesystem.

### When к tune

- **Lower `threshold_percent`** if the cluster runs hot и operators
  want later (less conservative) flagging. 5% is а reasonable floor;
  below that, OOM / disk-full becomes statistically imminent before
  pressure can fire.
- **Raise `threshold_percent`** for predictable workloads or
  conservative homelab setups. 15-20% memory / 20-25% disk is typical
  for clusters carrying interactive workloads.
- **Lower `consecutive_required` к 1** to react on first observation
  — useful for testing the path or clusters с long heartbeat
  intervals.
- **Set `enabled: false`** to disable detection entirely. Pressure
  columns still update on the row (the transition function is а
  no-op when disabled), но flags never set и the SQL filter never
  excludes anyone. Acceptable for homelab clusters where the operator
  prefers strict capacity-only scheduling.

## Algorithm selection

`placement.algorithm`:

- `resource_aware` (default) — kubernetes-style LeastAllocated across
  the enabled resources. Tie-break: node name lowercase lexicographic.
- `least_vm_count` — Phase 1.5 algorithm. Picks the node с the fewest
  pinned VMs. Same tie-break. Operator opt-out для clusters that
  cannot rely on heartbeat metrics or want pure count-based spread.

Per-resource configuration is meaningful only с `resource_aware`. Under
`least_vm_count`, the `resources` block is parsed and validated но does
not influence placement decisions. Validation warnings still fire,
which keeps the configuration honest if an operator flips back к
`resource_aware` later.

## Operator workflow when changing config

1. Edit `placement.resources` в your api yaml.
2. Restart `otherix-api`. Watch for startup warnings — each overcommit
   choice produces one line.
3. Validate placement behaviour: `otherix node list` shows raw vs
   effective free CPU / memory; `otherix pool list` shows raw vs
   effective free disk. The "(effective N free)" suffix renders when
   pending placements have not yet been observed by а heartbeat / scan.
4. Trigger а test VM create. The scheduler's decision should respect
   the new ratios. `vm create` against а saturated pool returns 409
   `no_eligible_nodes` с а structured `details.node_utilization`
   payload listing each candidate by name + per-resource usage.

There is currently no API or CLI endpoint to read or change the
placement config at runtime — it's а deploy-time decision. Restart
required to apply changes; the apartment graceful-shutdown path keeps
in-flight tasks alive до the next replica.

## See also

- Linux kernel: `vm.overcommit_memory`, `vm.overcommit_ratio` sysctls
  (man 5 proc).
- QEMU memory management: tcg / kvm allocation semantics, ballooning
  (not yet exposed by Otherix).
