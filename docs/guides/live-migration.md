# Live migration

Move a running VM from one physical node to another without shutting it down.
The guest keeps running through the move; its RAM and disks cross the wire while
it executes, and at the moment of switchover the cutover is measured in
milliseconds of pause, not a reboot.

Otherix migration is **peer-to-peer**: the two agents stream the VM's memory and
disks directly between hosts over mTLS, and the control plane orchestrates and
records the move without ever sitting in the data path. Nothing funnels through
the api-server, so a migration is bounded by the link between the two nodes, not
by the control plane.

```bash
otherix vm migrate web-1
```

That is the whole command. With no `--node`, the scheduler picks a different
eligible node for you; pass `--node <node>` to choose the target explicitly.

## What "live" means here

Every Otherix migration is a **full storage migration** - the VM's disks travel
with it, so there is no requirement for shared storage (no SAN, no NFS, no Ceph
in the path). The default is live:

- the guest keeps executing while its disk is mirrored block-by-block to the
  target and its RAM is pre-copied in iterative rounds;
- when the remaining dirty memory is small enough, the guest pauses for a brief
  switchover, the last bytes flush, and it resumes **on the target** from exactly
  where it left off;
- the source copy is torn down only after the target is confirmed running.

If you would rather take the simple route, `--offline` stops the VM, moves it
cold, and starts it on the target.

```bash
otherix vm migrate web-1 --node node-3     # live, to a specific node
otherix vm migrate web-1 --offline         # stop, move cold, start on target
```

`vm migrate` is asynchronous: it returns a task and a first-class **migration**
resource you can poll.

```bash
otherix migration list
otherix migration get <migration-id>
```

## The move is seamless - everything follows the VM

This is the part that matters in production: a migration is not a "drain the node
at 2am and hope nothing notices" operation. The things you have open against a VM,
and the things the VM depends on, follow it across the move.

### Your `logs -f` stream survives the move

A `vm logs --follow` stream does **not** break when the VM changes hosts. The
control plane notices the VM landed on a new node and transparently reconnects the
log stream to the new owning agent - you keep seeing output, uninterrupted,
through the cutover.

```bash
otherix vm logs web-1 --follow
# ... web-1 live-migrates node-1 -> node-2 ...
# ... output keeps streaming; no gap, no re-run ...
```

### Your interactive console survives the move

An open `vm console` session is **not** dropped at cutover either. You can be
typing at the guest's serial console, trigger a live migration, and keep working -
through the move and after the VM has landed on the other node - on the same
session. The control plane re-establishes the console bridge to the new owner
under the hood (re-minting the per-node console token), so keystrokes are
delivered at-least-once across the switchover.

```bash
otherix vm console web-1
# ... keep typing while web-1 moves to another node ...
# ... the session stays live the whole time ...
```

!!! tip "Why this is a big deal"
    On most self-hosted stacks, "live migration" only means the *guest* survives -
    your operator tools (the console you were in, the log tail you were watching)
    get cut off and you reconnect by hand to the new host. Otherix treats the
    operator session as a first-class thing that migrates too.

### The network follows the VM - no flood, no stale switch tables

The VM keeps its **MAC and IP** across the move, and open TCP connections survive,
because the L2 segment spans the whole cluster (see [Networking](../concepts/networking.md)).
The interesting part is *how* the fabric is told the VM moved:

- **Overlay networks (VXLAN).** The control plane is authoritative for the
  forwarding database and VXLAN address learning is **off**. When a VM migrates, the
  CP recomputes the FDB - "this MAC now sits behind the target node's VTEP" - and
  **fast-pushes** it: it nudges the overlay peers (and the target) to re-pull their
  FDB immediately, instead of waiting for the next heartbeat tick. The MAC re-points
  between VTEPs deterministically; the data plane is *told* where the VM went, never
  left to flood-and-learn it.
- **Bridge networks.** The moment the guest resumes on the target, its agent
  announces the VM to the segment - a QEMU self-announce (RARP, MAC-only) plus a
  **gratuitous ARP** for each IPv4 NIC - so the physical switches relearn the port
  immediately and traffic redirects to the new node without waiting out a MAC-aging
  timeout.

The result is the same in both cases: the VM is reachable at its new location
promptly after switchover, with no flood-and-learn cycle to get there.

## Observability: per-migration statistics

Once a live migration completes, `migration get` surfaces a `statistics:` section
captured from QEMU at cutover, so you can see exactly what the move cost:

```bash
otherix migration get <migration-id>
```

```text
statistics:
  ram:  transferred=470.1MiB total=2.1GiB dirty_pages_rate=0
  disk: transferred=847.6MiB total=847.6MiB
  total_time: 3.7s
  downtime: 26ms
  setup_time: 1ms
```

| Field | What it tells you |
|---|---|
| `ram.transferred` / `ram.total` | how much guest memory crossed the wire vs the VM's memory size |
| `ram.dirty_pages_rate` | how fast the guest was dirtying pages during pre-copy (a high rate is what makes a migration take longer to converge) |
| `disk.transferred` / `disk.total` | bytes mirrored vs total disk size |
| `total_time` | wall-clock duration of the whole migration |
| `downtime` | the brief switchover pause the guest actually experienced - the number you care about for SLAs |
| `setup_time` | time spent establishing the migration before data started flowing |

In the text output these are human-readable (`MiB`/`GiB`, `ms`/`s`); the JSON
output (`--output json`) keeps the raw bytes and milliseconds for scraping into
your own dashboards.

## Cancelling

A migration in flight can be cancelled; it is best-effort and leaves the VM safely
on one node or the other - never split-brained across both, never lost.

```bash
otherix migration cancel <migration-id>
```

## See also

- [Console and logs](console-and-logs.md) - the session tools that survive a move
- [Networking](../concepts/networking.md) - the cluster-wide L2 fabric and the FDB
- [Create and manage VMs](create-and-manage-vms.md)
