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
	var n store.Node
	found, err := s.c.GetJSON(ctx, nodeKey(id), &n)
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

	var out store.NodeDeleteOutcome
	if force {
		reason := fmt.Sprintf("source/target node %s force-deleted by user %s", id, callerID)
		cancelled, err := s.cancelMigrations(ctx, activeMigs, reason)
		if err != nil {
			return store.NodeDeleteOutcome{}, err
		}
		out.MigrationsCancelled = cancelled
		orphaned, err := s.orphanVMRuntimeOnNode(ctx, id)
		if err != nil {
			return store.NodeDeleteOutcome{}, err
		}
		out.VMsOrphaned = orphaned
	}

	now := time.Now().UTC()
	n.DeletedAt = &now
	n.UpdatedAt = now
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	ops := []clientv3.Op{
		clientv3.OpPut(nodeKey(id), string(val)),
		clientv3.OpDelete(nodeNameGuard(n.Name)),
	}
	// Purge the node's WireGuard fabric record + pubkey guard in the same txn so
	// the dead node stops appearing in the mesh and its pubkey becomes reusable.
	wgRec, wgErr := s.AgentWireguardByNodeID(ctx, id)
	switch {
	case wgErr == nil:
		ops = append(ops,
			clientv3.OpDelete(agentWireguardKey(id)),
			clientv3.OpDelete(agentWireguardPubkeyGuard(wgRec.PublicKey)),
		)
	case !errors.Is(wgErr, store.ErrNotFound):
		return store.NodeDeleteOutcome{}, fmt.Errorf("load agent_wireguard for node delete: %v", wgErr)
	}
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return store.NodeDeleteOutcome{}, fmt.Errorf("soft-delete node txn: %v", err)
	}
	return out, nil
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
	if n.CPUCoresAvailable != nil {
		v := max(*n.CPUCoresAvailable-pendCPU, 0)
		e.CPUCoresEffective = &v
	}
	if n.MemoryAvailableMib != nil {
		v := max(*n.MemoryAvailableMib-pendMem, 0)
		e.MemoryEffectiveMib = &v
	}
	return e, nil
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

// cancelMigrations marks each migration cancelled with the audit reason and
// stamps completed_at, returning the count.
func (s *Store) cancelMigrations(ctx context.Context, migs []store.Migration, reason string) (int64, error) {
	now := time.Now().UTC()
	var n int64
	for _, m := range migs {
		m.Phase = store.MigrationPhaseCancelled
		r := reason
		m.ErrorMessage = &r
		m.CompletedAt = &now
		if err := s.c.PutJSON(ctx, migrationKey(m.ID), m); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// orphanVMRuntimeOnNode marks every vm_runtime row on the node orphaned, clears
// current_node_id, and removes the node index entry, returning the count.
func (s *Store) orphanVMRuntimeOnNode(ctx context.Context, nodeID uuid.UUID) (int64, error) {
	items, err := s.c.Range(ctx, vmRuntimeNodeIndexPrefix(nodeID))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return 0, fmt.Errorf("corrupt vm_runtime node index %q: %v", kv.Key, perr)
		}
		var rt store.VMRuntime
		found, gerr := s.c.GetJSON(ctx, vmRuntimeKey(id), &rt)
		if gerr != nil {
			return 0, gerr
		}
		if !found {
			continue
		}
		rt.Phase = store.VmPhaseOrphaned
		rt.CurrentNodeID = nil
		if err := s.c.PutJSON(ctx, vmRuntimeKey(id), rt); err != nil {
			return 0, err
		}
		if _, err := s.c.Delete(ctx, kv.Key); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
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
