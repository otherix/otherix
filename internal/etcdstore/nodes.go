// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Nodes are a bounded, cluster-wide collection addressed by UUID with a
// case-insensitive name guard (uq_nodes_name) that doubles as the NodeByName
// index. The status/cordoned_at columns live on the row itself (no separate
// node_status table). NodeEffectiveByID / ListNodesEffective project the SQL
// node_effective_availability view: effective CPU/memory is raw availability
// minus pending VM reservations (vms pinned to the node, created after the last
// heartbeat). DeleteNode mirrors the force-cascade: cancel active migrations +
// orphan vm_runtime, then soft-delete.

func nodeKey(id uuid.UUID) string { return etcd.Key("nodes", id.String()) }

func nodePrefix() string { return etcd.Key("nodes") + "/" }

func nodeNameGuard(name string) string {
	return etcd.Key("uniq", "nodes", "name", strings.ToLower(name))
}

// vmKey, vmRuntimeKey, and migrationKey are the canonical primary keys for the
// VM-domain resources. They are defined here because DeleteNode's effective-
// availability and force-cascade read them; the vms / migrations slices reuse
// these same helpers (one definition per package).
func vmKey(id uuid.UUID) string { return etcd.Key("vms", id.String()) }

func vmRuntimeKey(vmID uuid.UUID) string { return etcd.Key("vm_runtime", vmID.String()) }

func migrationKey(id uuid.UUID) string { return etcd.Key("migrations", id.String()) }

// vmsPinnedNodeIndexPrefix lists the vms pinned to a node (maintained by the vms
// slice) - consumed by the effective-availability pending term.
func vmsPinnedNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "vms", "pinned_node", nodeID.String()) + "/"
}

// vmRuntimeNodeIndexPrefix lists the vm_runtime rows currently bound to a node
// (maintained by the vms slice) - consumed by DeleteNode's vm count + orphan.
func vmRuntimeNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "vm_runtime", "node", nodeID.String()) + "/"
}

// migrationsNodeIndexPrefix lists every migration touching a node as source or
// target (maintained by the migrations slice) - consumed by DeleteNode's active
// migration count + cancel, which filter to non-terminal phases by reading the
// primary.
func migrationsNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "migrations", "node", nodeID.String()) + "/"
}

// NodeByID returns the bare node row, or store.ErrNotFound (soft-deleted rows
// are invisible).
func (s *Store) NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return s.nodeByIDAtRev(ctx, id, 0)
}

// nodeByIDAtRev is NodeByID pinned to an MVCC revision (rev==0 reads latest).
// The FDB projection's gone-resolver reads it at the projection's snapshot
// revision so a node soft-delete or status flip mid-join cannot tear the read.
func (s *Store) nodeByIDAtRev(ctx context.Context, id uuid.UUID, rev int64) (store.Node, error) {
	var n store.Node
	found, err := s.c.GetJSONAtRev(ctx, nodeKey(id), rev, &n)
	if err != nil {
		return store.Node{}, err
	}
	if !found || n.DeletedAt != nil {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

// NodeByName returns the node with the given name (case-insensitive) via the
// name guard, or store.ErrNotFound.
func (s *Store) NodeByName(ctx context.Context, name string) (store.Node, error) {
	id, found, err := s.resolveGuard(ctx, nodeNameGuard(name))
	if err != nil {
		return store.Node{}, err
	}
	if !found {
		return store.Node{}, store.ErrNotFound
	}
	return s.NodeByID(ctx, id)
}

// NodeEffectiveByID returns the node joined with its effective availability, or
// store.ErrNotFound.
func (s *Store) NodeEffectiveByID(ctx context.Context, id uuid.UUID) (store.NodeEffectiveAvailability, error) {
	n, err := s.NodeByID(ctx, id)
	if err != nil {
		return store.NodeEffectiveAvailability{}, err
	}
	return s.nodeEffective(ctx, n)
}

// CreateNode inserts a node, stamping created_at/updated_at, writing the name
// guard + primary atomically. A name collision returns store.ErrNodeNameExists.
func (s *Store) CreateNode(ctx context.Context, arg store.CreateNodeParams) (store.Node, error) {
	now := time.Now().UTC()
	n := store.Node{
		ID:                      arg.ID,
		Name:                    arg.Name,
		Architecture:            arg.Architecture,
		AdvertisedEndpoint:      arg.AdvertisedEndpoint,
		MigrationHost:           arg.MigrationHost,
		MigrationPortRangeStart: arg.MigrationPortRangeStart,
		MigrationPortRangeEnd:   arg.MigrationPortRangeEnd,
		Status:                  arg.Status,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.Node{}, err
	}
	guard := nodeNameGuard(n.Name)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, n.ID.String()),
			clientv3.OpPut(nodeKey(n.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.Node{}, fmt.Errorf("create node txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Node{}, store.ErrNodeNameExists
	}
	return n, nil
}

// CordonNode marks the node cordoned, stamping cordoned_at and bumping
// updated_at. The handler gates valid transitions before calling.
func (s *Store) CordonNode(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return s.setNodeCordon(ctx, id, store.NodeStatusCordoned, true)
}

// UncordonNode returns the node to ready, clearing cordoned_at. The handler
// gates valid transitions before calling.
func (s *Store) UncordonNode(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return s.setNodeCordon(ctx, id, store.NodeStatusReady, false)
}

// setNodeCordon applies a cordon/uncordon status change, setting/clearing
// cordoned_at and bumping updated_at (the nodes_set_updated_at trigger).
func (s *Store) setNodeCordon(ctx context.Context, id uuid.UUID, status store.NodeStatus, cordon bool) (store.Node, error) {
	n, err := s.NodeByID(ctx, id)
	if err != nil {
		return store.Node{}, err
	}
	n.Status = status
	now := time.Now().UTC()
	if cordon {
		n.CordonedAt = &now
	} else {
		n.CordonedAt = nil
	}
	n.UpdatedAt = now
	if err := s.c.PutJSON(ctx, nodeKey(id), n); err != nil {
		return store.Node{}, err
	}
	return n, nil
}

// ListNodesEffective returns nodes joined with their effective availability,
// matching the optional architecture/status filters, ordered by (created_at,
// id) ascending, after the cursor, capped at LimitCount.
func (s *Store) ListNodesEffective(ctx context.Context, arg store.ListNodesEffectiveParams) ([]store.NodeEffectiveAvailability, error) {
	items, err := s.c.Range(ctx, nodePrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.NodeEffectiveAvailability, 0, len(items))
	for _, kv := range items {
		var n store.Node
		if err := json.Unmarshal(kv.Value, &n); err != nil {
			return nil, fmt.Errorf("unmarshal node %q: %v", kv.Key, err)
		}
		if n.DeletedAt != nil {
			continue
		}
		if arg.Architecture != nil && n.Architecture != *arg.Architecture {
			continue
		}
		if arg.Status != nil && n.Status != *arg.Status {
			continue
		}
		if !afterCursor(n.CreatedAt, n.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		eff, err := s.nodeEffective(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	sortByCreatedAtID(out, func(e store.NodeEffectiveAvailability) (time.Time, uuid.UUID) {
		return e.CreatedAt, e.ID
	})
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// DeleteNode soft-deletes the node. Without force it refuses with
// *store.ResourceInUseError (keys "vms" + "active_migrations", both always
// present) when the node still hosts vm_runtime rows or active migrations. With
// force it cancels every active migration touching the node and orphans every
// vm_runtime row on it (clearing current_node_id, leaving vms.desired_phase
// untouched), recording the counts in the outcome. callerID is recorded in the
// migration cancel reason for audit.
func (s *Store) DeleteNode(ctx context.Context, id uuid.UUID, force bool, callerID uuid.UUID) (store.NodeDeleteOutcome, error) {
	n, err := s.NodeByID(ctx, id)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	vmCount, err := s.countPrefix(ctx, vmRuntimeNodeIndexPrefix(id))
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	activeMigs, err := s.activeMigrationsOnNode(ctx, id)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	if !force && (vmCount > 0 || len(activeMigs) > 0) {
		return store.NodeDeleteOutcome{}, &store.ResourceInUseError{Resources: map[string]int64{
			"vms":               vmCount,
			"active_migrations": int64(len(activeMigs)),
		}}
	}

	var (
		out                  store.NodeDeleteOutcome
		cancelOps, orphanOps []clientv3.Op
	)
	if force {
		reason := fmt.Sprintf("source/target node %s force-deleted by user %s", id, callerID)
		cancelOps, out.MigrationsCancelled, err = cancelMigrationOps(activeMigs, reason)
		if err != nil {
			return store.NodeDeleteOutcome{}, err
		}
		orphanOps, out.VMsOrphaned, err = s.orphanVMRuntimeOps(ctx, id)
		if err != nil {
			return store.NodeDeleteOutcome{}, err
		}
	}

	// Load the node's WireGuard fabric record so its purge ops can be ordered
	// ahead of the node soft-delete (see nodeDeleteCascade). ErrNotFound means
	// the node never joined the mesh - nothing to purge; any other error aborts.
	var wgRec *store.AgentWireguard
	rec, wgErr := s.AgentWireguardByNodeID(ctx, id)
	switch {
	case wgErr == nil:
		wgRec = &rec
	case !errors.Is(wgErr, store.ErrNotFound):
		return store.NodeDeleteOutcome{}, fmt.Errorf("load agent_wireguard for node delete: %v", wgErr)
	}

	now := time.Now().UTC()
	n.DeletedAt = &now
	n.UpdatedAt = now
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}

	cascade := nodeDeleteCascade(id, n.Name, string(val), cancelOps, orphanOps, wgRec)
	if err := s.commitInChunks(ctx, cascade); err != nil {
		return store.NodeDeleteOutcome{}, fmt.Errorf("force-delete node cascade: %v", err)
	}
	return out, nil
}

// nodeDeleteCascade assembles the ordered force-delete op slice. The whole
// cascade routes through commitInChunks: each <=120-op chunk commits atomically,
// so a crash leaves a clean PREFIX of cancel/orphan/wg-purge ops. The
// node-soft-delete ops (name-guard delete + nodePut) are appended LAST, the
// nodePut genuinely last, so the node row disappears only after every other op,
// and a retry re-derives the
// remaining work (NodeByID at the top returns ErrNotFound once the node row is
// gone). Every preceding op is an idempotent put/delete, so re-running the whole
// cascade on retry is safe.
//
// Ordering is load-bearing: the WireGuard fabric purge (record + pubkey guard)
// MUST precede the node-soft-delete. Were it to trail, a chunk boundary falling
// between the node-delete and the wg-purge plus a crash would soft-delete the
// node while leaking the agent_wireguard record + pubkey guard, which the retry
// can never re-run (the gone node short-circuits at NodeByID). There is no
// backstop reaper for agent_wireguard, so the leaked pubkey guard would later
// fail a node re-bootstrap with ErrAgentWireguardPubkeyInUse.
func nodeDeleteCascade(nodeID uuid.UUID, nodeName, nodeVal string, cancelOps, orphanOps []clientv3.Op, wgRec *store.AgentWireguard) []clientv3.Op {
	cascade := make([]clientv3.Op, 0, len(cancelOps)+len(orphanOps)+4)
	cascade = append(cascade, cancelOps...)
	cascade = append(cascade, orphanOps...)
	// Purge the node's WireGuard fabric record + pubkey guard so the dead node
	// stops appearing in the mesh and its pubkey becomes reusable - before the
	// node soft-delete so a retry can re-run it.
	if wgRec != nil {
		cascade = append(cascade,
			clientv3.OpDelete(agentWireguardKey(nodeID)),
			clientv3.OpDelete(agentWireguardPubkeyGuard(wgRec.PublicKey)),
		)
	}
	// The name-guard delete precedes the nodePut so the nodePut (which flips the
	// row's DeletedAt and thus makes NodeByID short-circuit) is the genuine LAST
	// op: a crash before it leaves the node row present and a retry re-runs the
	// whole idempotent cascade.
	cascade = append(cascade,
		clientv3.OpDelete(nodeNameGuard(nodeName)),
		clientv3.OpPut(nodeKey(nodeID), nodeVal),
	)
	return cascade
}

// nodeEffective projects a node onto the effective-availability view, computing
// effective CPU/memory as raw availability minus pending reservations.
func (s *Store) nodeEffective(ctx context.Context, n store.Node) (store.NodeEffectiveAvailability, error) {
	e := store.NodeEffectiveAvailability{
		ID:                       n.ID,
		Name:                     n.Name,
		Architecture:             n.Architecture,
		AdvertisedEndpoint:       n.AdvertisedEndpoint,
		MigrationHost:            n.MigrationHost,
		MigrationPortRangeStart:  n.MigrationPortRangeStart,
		MigrationPortRangeEnd:    n.MigrationPortRangeEnd,
		Status:                   n.Status,
		CordonedAt:               n.CordonedAt,
		CPUCoresTotal:            n.CPUCoresTotal,
		CPUCoresAvailable:        n.CPUCoresAvailable,
		CPUModel:                 n.CPUModel,
		CpuFlags:                 n.CpuFlags,
		MemoryTotalMib:           n.MemoryTotalMib,
		MemoryAvailableMib:       n.MemoryAvailableMib,
		Hugepages2mibTotal:       n.Hugepages2mibTotal,
		Hugepages1gibTotal:       n.Hugepages1gibTotal,
		KernelVersion:            n.KernelVersion,
		QEMUVersion:              n.QEMUVersion,
		NumaTopology:             n.NumaTopology,
		Capabilities:             n.Capabilities,
		LastHeartbeatAt:          n.LastHeartbeatAt,
		AgentVersion:             n.AgentVersion,
		Labels:                   n.Labels,
		MemoryPressureSince:      n.MemoryPressureSince,
		MemoryPressureCount:      n.MemoryPressureCount,
		SystemDiskTotalBytes:     n.SystemDiskTotalBytes,
		SystemDiskAvailableBytes: n.SystemDiskAvailableBytes,
		SystemDiskPressureSince:  n.SystemDiskPressureSince,
		SystemDiskPressureCount:  n.SystemDiskPressureCount,
		CreatedAt:                n.CreatedAt,
		UpdatedAt:                n.UpdatedAt,
		DeletedAt:                n.DeletedAt,
	}
	pendCPU, pendMem, err := s.pendingReservations(ctx, n)
	if err != nil {
		return store.NodeEffectiveAvailability{}, err
	}
	// Reserve capacity for VMs being live-migrated INTO this node, for the
	// migration's duration (spec crash-semantics). During an active migration
	// the VM stays pinned to the SOURCE until cutover (CommitMigrationCutover
	// flips PinnedNodeID to the target), so on the target node it is NOT in the
	// pinned_node index pendingReservations ranges - this reservation is the only
	// subtraction here and does not double-count. When the migration goes
	// terminal its node-index entry is deleted and the reservation disappears;
	// after cutover the VM is pinned to the target and counted by the pending
	// path instead.
	resvCPU, resvMem, err := s.migrationTargetReservations(ctx, n.ID)
	if err != nil {
		return store.NodeEffectiveAvailability{}, err
	}
	if n.CPUCoresAvailable != nil {
		v := max(*n.CPUCoresAvailable-pendCPU-resvCPU, 0)
		e.CPUCoresEffective = &v
	}
	if n.MemoryAvailableMib != nil {
		v := max(*n.MemoryAvailableMib-pendMem-resvMem, 0)
		e.MemoryEffectiveMib = &v
	}
	return e, nil
}

// migrationTargetReservations sums the cpu/memory of VMs being live-migrated
// into the node by an active (non-terminal) migration. It reuses the per-node
// migration index activeMigrationsOnNode reads (which already excludes terminal
// migrations) and keeps only migrations whose target is this node.
func (s *Store) migrationTargetReservations(ctx context.Context, nodeID uuid.UUID) (cpu int32, mem int64, err error) {
	migs, err := s.activeMigrationsOnNode(ctx, nodeID)
	if err != nil {
		return 0, 0, err
	}
	for _, m := range migs {
		if m.TargetNodeID == nil || *m.TargetNodeID != nodeID {
			continue
		}
		vm, gerr := s.VMByID(ctx, m.VmID)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				continue
			}
			return 0, 0, gerr
		}
		cpu += vm.CpuCores
		mem += int64(vm.MemoryMib)
	}
	return cpu, mem, nil
}

// pendingReservations sums the cpu/memory of vms pinned to the node that the
// agent has not yet observed (created after the node's last heartbeat),
// matching the lateral subquery of node_effective_availability.
func (s *Store) pendingReservations(ctx context.Context, n store.Node) (cpu int32, mem int64, err error) {
	items, err := s.c.Range(ctx, vmsPinnedNodeIndexPrefix(n.ID))
	if err != nil {
		return 0, 0, err
	}
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return 0, 0, fmt.Errorf("corrupt pinned-node index %q: %v", kv.Key, perr)
		}
		var vm store.VM
		found, gerr := s.c.GetJSON(ctx, vmKey(id), &vm)
		if gerr != nil {
			return 0, 0, gerr
		}
		if !found || vm.DeletedAt != nil || vm.DesiredPhase == store.VmDesiredPhaseDeleted {
			continue
		}
		if n.LastHeartbeatAt != nil && !vm.CreatedAt.After(*n.LastHeartbeatAt) {
			continue
		}
		cpu += vm.CpuCores
		mem += int64(vm.MemoryMib)
	}
	return cpu, mem, nil
}

// activeMigrationsOnNode returns the non-terminal migrations touching the node,
// resolved from the node migration index by reading each primary.
func (s *Store) activeMigrationsOnNode(ctx context.Context, nodeID uuid.UUID) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, migrationsNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, err
	}
	var active []store.Migration
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt migration node index %q: %v", kv.Key, perr)
		}
		var m store.Migration
		found, gerr := s.c.GetJSON(ctx, migrationKey(id), &m)
		if gerr != nil {
			return nil, gerr
		}
		if !found || isTerminalMigration(m.Phase) {
			continue
		}
		active = append(active, m)
	}
	return active, nil
}

// cancelMigrationOps builds the ops that mark each migration cancelled with the
// audit reason and stamp completed_at, plus the terminalCleanupOps that release
// the migration's transient state (the active-per-VM guard + per-node index
// entries) on the terminal transition - the same release every other terminal
// path (CommitMigrationCutover, UpdateMigrationProgress-to-terminal,
// CancelMigration) performs. Without it, force-deleting a node mid-migration
// would leave the per-VM active guard dangling and the VM permanently
// un-migratable (every future CreateMigration CAS would hit the guard and 409).
// Returns the ops + cancelled count. It does not commit - the caller appends the
// ops to the force-delete cascade so they commit (chunked) with the rest. All
// terminalCleanupOps are idempotent OpDeletes, safe under commitInChunks.
func cancelMigrationOps(migs []store.Migration, reason string) ([]clientv3.Op, int64, error) {
	now := time.Now().UTC()
	ops := make([]clientv3.Op, 0, len(migs))
	var n int64
	for _, m := range migs {
		m.Phase = store.MigrationPhaseCancelled
		r := reason
		m.ErrorMessage = &r
		m.CompletedAt = &now
		val, err := etcd.Marshal(m)
		if err != nil {
			return nil, 0, err
		}
		ops = append(ops, clientv3.OpPut(migrationKey(m.ID), string(val)))
		ops = append(ops, terminalCleanupOps(m)...)
		n++
	}
	return ops, n, nil
}

// orphanVMRuntimeOps builds the ops that mark every vm_runtime row on the node
// orphaned, clear current_node_id, and remove the node index entry, returning
// the ops + count. It reads each runtime primary inline (a missing row is
// skipped) but does not commit - the caller appends the ops to the force-delete
// cascade so they commit atomically with the rest.
func (s *Store) orphanVMRuntimeOps(ctx context.Context, nodeID uuid.UUID) ([]clientv3.Op, int64, error) {
	items, err := s.c.Range(ctx, vmRuntimeNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, 0, err
	}
	ops := make([]clientv3.Op, 0, len(items)*2)
	var n int64
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, 0, fmt.Errorf("corrupt vm_runtime node index %q: %v", kv.Key, perr)
		}
		var rt store.VMRuntime
		found, gerr := s.c.GetJSON(ctx, vmRuntimeKey(id), &rt)
		if gerr != nil {
			return nil, 0, gerr
		}
		if !found {
			continue
		}
		rt.Phase = store.VmPhaseOrphaned
		rt.CurrentNodeID = nil
		val, merr := etcd.Marshal(rt)
		if merr != nil {
			return nil, 0, merr
		}
		ops = append(ops,
			clientv3.OpPut(vmRuntimeKey(id), string(val)),
			clientv3.OpDelete(kv.Key),
		)
		n++
	}
	return ops, n, nil
}

// isTerminalMigration reports whether a migration phase is terminal (matches the
// SQL "phase not in ('completed','failed','cancelled')" active predicate).
func isTerminalMigration(p store.MigrationPhase) bool {
	switch p {
	case store.MigrationPhaseCompleted, store.MigrationPhaseFailed, store.MigrationPhaseCancelled:
		return true
	default:
		return false
	}
}
