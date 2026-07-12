# Scheduler configuration guide

Otherix's VM placement scheduler ranks candidate (node, pool) targets
by a kubernetes-style LeastAllocated score across the enabled resource
dimensions. This document covers the per-resource knobs operators
expose in `placement.resources` and the safety trade-offs of memory
and disk overcommit.

Companion references:

- `deploy/config/api.example.yaml` — annotated production config.
- `dev/config/api.yaml` — strict defaults, parallel structure.

## Per-resource placement settings

Each of `cpu`, `memory`, and `disk` is configured independently:

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
      overcommit_zram_floor_mib: 256    # min zram size for a node to be overcommit-eligible
      overcommit_zram_confidence: 0.5   # safe physical fraction of the zram size
    disk:
      enabled: true
      overcommit_ratio: 1.0
```

- **`enabled`** (default `true`) — include the resource in the fit check
  AND scoring formula. Disabling drops the resource from both. The
  LeastAllocated denominator floats with the count of enabled
  dimensions, so disabling one does not bias the score scale.

- **`overcommit_ratio`** (default `1.0`):
  - `1.0` — strict, no overcommit (production-safe default).
  - `> 1.0` — overcommit, capacity inflated.
  - `< 1.0` — reserve headroom, capacity deflated.

  For **CPU** and **disk** the ratio is a plain multiplier on the
  per-resource effective availability. For **memory** it behaves
  differently: the ratio is a *ceiling*, and the actual overcommit a
  node receives is bounded per-node by that node's zram compressed-swap
  safety net (see "Memory overcommit" below). A memory ratio above 1.0
  never overcommits a node that has no qualifying zram device. Because the
  zram-bounded model only adds headroom and cannot deflate capacity, a
  memory `overcommit_ratio` below 1.0 is rejected at startup (the `< 1.0`
  reserve-headroom form above applies to CPU and disk only); reserve memory
  slack with the memory pressure threshold instead.

  Validation rejects `overcommit_ratio <= 0` at startup. Validation
  warnings — not errors — are logged at startup for any `> 1.0`
  configuration so operators see the trade-off they opted into on each
  api-server restart.

## Startup warnings

`otherix-api` emits a `slog` Warn line at startup for each per-resource
overcommit and for the all-resources-disabled scenario:

```
WARN placement config warning="placement.resources.memory.overcommit_ratio=1.50 — overcommit enabled (OOM kill risk under memory pressure); see docs/scheduler-configuration.md"
```

Ratios above `2.0` use stronger "extreme overcommit" language. All
three resources disabled emits a separate warning that scoring degrades
to count-based fallback (Least-VM-count parity without switching the cluster
algorithm).

Default config (every resource enabled at ratio 1.0) emits zero
warnings — the absence on subsequent restarts confirms the cluster is
running with strict accounting.

## Memory overcommit

`memory.overcommit_ratio > 1.0` lets the scheduler place VMs whose
combined memory **requests** exceed a node's physical RAM. Unbounded,
that is dangerous: when cumulative actual usage exceeds physical RAM,
the Linux OOM killer reaps a process - typically QEMU - and the
affected VM crashes abruptly, with no notice to its guest.

Otherix bounds this per-node against the zram compressed-swap safety
net, so overcommit can never exceed what the node can actually absorb:

- **Overcommit is per-node and requires a qualifying zram device.** A
  node is overcommit-eligible only when it reports a zram
  compressed-swap device of at least `overcommit_zram_floor_mib`
  (default 256). A node without one, or with a zram below the floor, is
  fit-checked **strictly** regardless of the cluster ratio. Provision
  the zram device on the host first - see
  `deploy/config/zram-generator.conf.example` (copy to
  `/etc/systemd/zram-generator.conf`); the agent observes `/proc/swaps`
  and reports the active device, which the scheduler reads.

- **The ratio is a ceiling; the zram size is the lever.** The extra
  memory headroom the scheduler grants an eligible node is:

  ```
  headroom = min( total_ram × (overcommit_ratio - 1),   operator ceiling
                  zram_size × overcommit_zram_confidence ) physical ceiling
  ```

  The smaller term wins, so a large ratio never overcommits a node
  beyond what its zram can absorb - a ratio of 10.0 on a node with a
  1 GiB zram still yields only ~`1 GiB × confidence` of headroom. To
  raise density, **grow the node's zram device**, not the ratio.

- **`overcommit_zram_confidence`** (default `0.5`) is the physically
  safe fraction of the zram size to grant as headroom. It is **not** a
  compression multiplier. zram stores swapped pages *compressed*, but
  those compressed bytes still occupy physical RAM: holding a full
  disksize `D` of logical pages at compression ratio `r` costs `D/r`
  physical RAM out of the same total, so the physically safe extra
  headroom is `D × (r-1)/r`, never the full disksize. Keep
  `confidence ≤ (r-1)/r` (zstd ~0.67, lz4 ~0.5); the default `0.5` is
  safe for any `r ≥ 2`. Granting the full disksize (`confidence = 1.0`)
  would OOM a node under simultaneous full residency.

- **`overcommit_zram_floor_mib`** (default `256`) is the minimum zram
  size that makes a node overcommit-eligible. Below it, the node is
  strict.

Effective overcommit at `confidence 0.5`, sizing the zram as a fraction
of node RAM (the `overcommit_ratio` ceiling must be at or above the
listed figure for the zram term to bind):

| zram size | headroom | effective overcommit |
|-----------|----------|----------------------|
| `ram / 4` | `ram / 8` | ~1.125x |
| `ram / 2` | `ram / 4` | ~1.25x  |
| `ram`     | `ram / 2` | ~1.5x   |

At `overcommit_ratio: 1.0` (the default) the headroom is zero on every
node: memory placement is byte-identical to strict accounting.
Overcommit activates only when the operator raises the ratio **and**
the node carries a qualifying zram device.

### Host configuration before enabling memory overcommit

1. **Provision the zram compressed-swap device** on each agent host you
   want to overcommit (see above). This is the safety net the
   per-node bound reads; without it a node is never overcommitted.

2. **Provision adequate swap** as a secondary backstop. Rule of thumb:
   `swap ≥ (overcommit_ratio - 1) × RAM`. Example: 16 GiB RAM with ratio
   1.5 → at least 8 GiB swap recommended. Swap I/O is far slower than
   RAM, so this is a safety net, not a performance feature.

3. **Review the `vm.overcommit_memory` sysctl** on each agent host:
   - `0` (default, heuristic) — kernel decides per allocation. Usually
     fine for moderate ratios (≤ 1.5) on well-provisioned hosts.
   - `1` (always allow) — kernel never refuses an allocation. Risky;
     defers OOM to actual fault time.
   - `2` (strict accounting) — kernel refuses allocations that would
     push commit charge past `swap + vm.overcommit_ratio% × RAM`.
     Predictable but requires careful sizing of the kernel
     `vm.overcommit_ratio` sysctl (distinct from Otherix's per-resource
     `overcommit_ratio` config).

   The conservative choice for production-ish Otherix homelabs is
   either `0` or `2` with the kernel sysctl deliberately tuned. Do not
   set `1`.

4. **Monitor swap usage** continuously. Frequent swap activity is a
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

The Memory column is a *ceiling*, not the realised density: the actual
overcommit each node receives is bounded by its zram net (see "Memory
overcommit" above), so raising the ratio alone does nothing until the
node has a large enough zram device. Size the node's zram to the
density you want; the ratio just caps it. Disk overcommit above 1.5 is
risky for any VM whose disks may grow toward their allocated capacity.

## Disk overcommit considerations

`disk.overcommit_ratio > 1.0` lets Otherix over-subscribe pool capacity
based on the assumption that sparse qcow2 disks consume less than their
allocated size. Risks:

- VMs that write heavily can grow toward their allocated capacity.
  Multiple VMs hitting that limit simultaneously can fill the pool.
- Pool scans (default 15 min, configurable via
  `workers.storage_pool_scan`) detect filling pools but do not prevent
  placement decisions made before the scan lands.
- Cascading "no space left on device" errors corrupt guest filesystems
  and can wedge running VMs.

The `pool_effective_capacity` view already subtracts pending-VM disk
commitments from scan-reported availability, so the scheduler never
double-counts. Disk overcommit explicitly opts out of that safety and
trusts the operator's growth model.

Safe disk overcommit requires:

1. Sparse qcow2 disk format (Otherix default).
2. Pool monitoring with alerting before fill.
3. Awareness of typical workload growth patterns in the cluster.

## Disabling resources entirely

Setting `enabled: false` removes the resource from placement
consideration:

- Fit check skipped — no constraint on the dimension.
- Scoring formula adjusts — the denominator drops by one. Remaining
  enabled dimensions are scored at full magnitude (no scale shift).
- All three disabled → scoring degrades to count-based (LeastAllocated
  with zero enabled dimensions is undefined; the scheduler falls back
  to Least-VM-count parity, which is the same algorithm `least_vm_count`
  applies cluster-wide).

When to disable:

- **CPU `enabled: false`** — VMs are mostly idle and memory/disk are
  the binding constraints. Rare; CPU is usually the cheapest signal
  to keep.
- **Memory `enabled: false`** — almost never. Memory pressure is the
  most common failure mode on Linux VM hosts.
- **Disk `enabled: false`** — shared network storage where per-pool
  capacity reflects a pooled remote resource rather than node-local
  bytes. Pool capacity tracking still happens; just doesn't influence
  placement.

## Node-pressure detection

Independent of the capacity / overcommit knobs above, the scheduler
honours pressure conditions surfaced under `placement.pressure.*`.
Pressure detection excludes stressed nodes (or pools, for disk
pressure) from placement — a hard constraint, no operator override.
Three pressure types land together with Sub-iteration B:

- **memory** — per-node, heartbeat-driven. Free memory below
  threshold. OOM-kill risk.
- **system_disk** — per-node, heartbeat-driven. Free root-filesystem
  bytes below threshold. Agent crash / log truncation risk.
- **disk** — per-pool, scan-driven. Free pool bytes below threshold.
  Pool-scoped — other pools on the same node remain eligible.

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
  `memory_available_mib / memory_total_mib` reported in every
  heartbeat against `threshold_percent`. The agent does not
  pre-compute pressure — operators tune one set of thresholds in the
  api config rather than per-agent.
- **Asymmetric debouncing.** Pressure is set after
  `consecutive_required` consecutive below-threshold observations
  (default ≈ 90 s at the 30 s heartbeat cadence). Recovery is
  immediate — the first at-or-above-threshold observation clears the
  flag and resets the counter. Slow-to-set / fast-to-clear prevents
  flapping on transient spikes while restoring eligibility promptly.
- **Hard exclusion.** Pressured nodes are filtered out of
  `ListEligiblePoolsByName`. The 409 `no_eligible_nodes` envelope
  carries `details.reason: "node_pressure"` and a
  `filtered_due_to_pressure` list when pressure accounts for the
  exclusion.

### System disk pressure

```yaml
placement:
  pressure:
    system_disk:
      enabled: true              # disable to skip detection entirely
      threshold_percent: 10      # flag when free root-fs bytes < 10% of total
      consecutive_required: 3    # ~90 s sustained at default heartbeat cadence
```

Mechanics mirror memory pressure exactly — same heartbeat-driven CP-
side computation, same asymmetric debouncing, same hard-exclusion
semantics. Only the raw metric differs: the agent reads
`syscall.Statfs("/")` on every heartbeat and reports
`system_disk_total_bytes` / `system_disk_available_bytes` (both
nullable when the syscall fails — pressure state carries forward
across NULL observations). A system_disk-pressured node is excluded
from placement entirely; agent logs / NVRAM allocation / QMP sockets
all live on the root filesystem, so disk exhaustion threatens the
agent itself rather than just any individual VM.

### Pool disk pressure

```yaml
placement:
  pressure:
    disk:
      enabled: true              # disable to skip detection entirely
      threshold_percent: 15      # flag when free pool bytes < 15% of capacity
      consecutive_required: 1    # single scan sets pressure (scans are 15 min apart)
```

Mechanics:

- **Scan-driven CP computation.** The scan worker reads pool
  `available_bytes` / `capacity_bytes` from the agent's scan response
  and runs the same transition function inside the same transaction
  as `UpsertStoragePoolUsage`. Scan-completion-only — heartbeat-time
  observations are not relevant for pool metrics.
- **Single-observation default.** Scans are 15 min apart (default
  `workers.storage_pool_scan.interval`), so a single sub-threshold
  observation is a sufficient signal; operators can raise
  `consecutive_required` for multi-scan debouncing if their scan
  cadence is more aggressive.
- **Pool-scoped exclusion.** Pressured pools are filtered from
  `ListEligiblePoolsByName`; other pools on the same node remain
  eligible. The 409 envelope's `filtered_due_to_pressure` payload
  carries an explicit `pool` field for these entries.

### Operator visibility

`otherix node list` shows a computed STATUS column combining raw
status with active **node-scoped** pressure conditions (memory +
system_disk):

```
ready
under_pressure                # memory OR system_disk pressure set
cordoned, under_pressure
unreachable                   # pressure data suppressed (stale)
```

`otherix node get <node>` renders a `pressure:` section with one row per
node-scoped condition:

```
pressure:
  memory:      active since 5m ago (consecutive_count=5)
  system_disk: ok
```

`otherix pool list` shows pool STATUS independently — a pool with
`disk_pressure` reads `under_pressure` regardless of its owning node's
condition:

```
NAME     NODE    AVAILABLE                STATUS
default  node-a  100 GiB                  ready
default  node-b  2 GiB                    under_pressure
```

`otherix pool get <pool>` adds a `pressure:` section with the `disk:`
condition state.

### Shared filesystem behavior

In typical homelab setups the storage pool's `path` lives on the
same filesystem as `/` — a single physical disk hosts both the
agent runtime and VM images. When that filesystem fills, both
`system_disk_pressure` (heartbeat-driven, node-level) and
`disk_pressure` (scan-driven, pool-level) fire on the same physical
condition. Both surface in the CLI and the envelope. This is accurate —
the resource is genuinely shared — not duplication. Multi-mount
setups (advanced) detect independently per filesystem.

### When to tune

- **Lower `threshold_percent`** if the cluster runs hot and operators
  want later (less conservative) flagging. 5% is a reasonable floor;
  below that, OOM / disk-full becomes statistically imminent before
  pressure can fire.
- **Raise `threshold_percent`** for predictable workloads or
  conservative homelab setups. 15-20% memory / 20-25% disk is typical
  for clusters carrying interactive workloads.
- **Lower `consecutive_required` to 1** to react on first observation
  — useful for testing the path or clusters with long heartbeat
  intervals.
- **Set `enabled: false`** to disable detection entirely. Pressure
  columns still update on the row (the transition function is a
  no-op when disabled), but flags never set and the SQL filter never
  excludes anyone. Acceptable for homelab clusters where the operator
  prefers strict capacity-only scheduling.

## Algorithm selection

`placement.algorithm`:

- `resource_aware` (default) — kubernetes-style LeastAllocated across
  the enabled resources. Tie-break: node name lowercase lexicographic.
- `least_vm_count` — Phase 1.5 algorithm. Picks the node with the fewest
  pinned VMs. Same tie-break. Operator opt-out for clusters that
  cannot rely on heartbeat metrics or want pure count-based spread.

The `resources` block always gates eligibility: the resource fit check
(does the node have enough free CPU / memory / disk for the request,
accounting for overcommit) runs for **both** algorithms, so a node that
cannot fit the VM is never picked regardless of `algorithm`. What
`least_vm_count` ignores is the resource-aware **scoring** - it ranks the
remaining eligible nodes by pinned-VM count rather than LeastAllocated.
So per-resource tuning (overcommit ratios, enable/disable) still affects
which nodes are eligible under `least_vm_count`; it just does not affect
the final ranking among them. Validation warnings still fire, which keeps
the configuration honest if an operator flips back to `resource_aware`
later.

## Operator workflow when changing config

1. Edit `placement.resources` in your api yaml.
2. Restart `otherix-api`. Watch for startup warnings — each overcommit
   choice produces one line.
3. Validate placement behaviour: `otherix node list` shows raw vs
   effective free CPU / memory; `otherix pool list` shows raw vs
   effective free disk. The "(effective N free)" suffix renders when
   pending placements have not yet been observed by a heartbeat / scan.
4. Trigger a test VM create. `vm create` is async: it returns 202 with
   a task, and placement runs later in the background scheduling loop,
   not synchronously at the request. When no node fits, the reject is
   recorded as the VM's scheduling reason - `vm get` surfaces
   `status.reason` (`insufficient_resources` / `no_eligible_nodes`) plus
   a human `status.message`. The structured per-candidate detail is
   persisted on the VM (its `SchedulingDetails`), listing each rejected
   candidate by name + per-resource usage, and now also
   `overcommit_eligible` (true when the node has a qualifying zram net)
   and `mem_overcommit_headroom_mib` (the extra memory that net would
   grant), so a strict fit-reject shows where adding or enlarging a zram
   device could let the request fit. This structured detail is persisted
   only; it is not currently rendered into any API response.

There is currently no API or CLI endpoint to read or change the
placement config at runtime — it's a deploy-time decision. Restart
required to apply changes; the apartment graceful-shutdown path keeps
in-flight tasks alive to the next replica.

## See also

- Linux kernel: `vm.overcommit_memory`, `vm.overcommit_ratio` sysctls
  (man 5 proc).
- QEMU memory management: tcg / kvm allocation semantics, ballooning
  (not yet exposed by Otherix).
