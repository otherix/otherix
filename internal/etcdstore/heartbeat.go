// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// RunHeartbeatProjection runs the agent heartbeat projection against the etcd
// store, handing fn the projection surface.
//
// This is NOT a single isolated transaction: etcd has no equivalent of a
// multi-key read-write transaction spanning the whole projection, so each
// method applies its write directly. This is safe because the heartbeat
// projection is idempotent and retried forever: a partially applied heartbeat
// is re-applied on the next tick and converges. The projection's reads
// (NodeForHeartbeat, NodeByID, lookups) do not depend on its own writes, so
// direct application preserves the observable result.
func (s *Store) RunHeartbeatProjection(ctx context.Context, fn func(store.HeartbeatProjection) error) error {
	return fn(heartbeatProjection{s: s})
}

// heartbeatProjection is the etcd-backed projection surface handed to the heartbeat
// handler. Each method maps to the corresponding etcd read/write.
type heartbeatProjection struct{ s *Store }

// NodeForHeartbeat returns the pre-flight node fields the projection guards on.
func (h heartbeatProjection) NodeForHeartbeat(ctx context.Context, nodeID uuid.UUID) (store.GetNodeForHeartbeatRow, error) {
	n, err := h.s.NodeByID(ctx, nodeID)
	if err != nil {
		return store.GetNodeForHeartbeatRow{}, err
	}
	return store.GetNodeForHeartbeatRow{
		ID:                      n.ID,
		Architecture:            n.Architecture,
		Status:                  n.Status,
		MemoryPressureSince:     n.MemoryPressureSince,
		MemoryPressureCount:     n.MemoryPressureCount,
		SystemDiskPressureSince: n.SystemDiskPressureSince,
		SystemDiskPressureCount: n.SystemDiskPressureCount,
	}, nil
}

// NodeByID returns the bare node row.
func (h heartbeatProjection) NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return h.s.NodeByID(ctx, id)
}

// NodeByIDAtRev returns the bare node row pinned to an MVCC revision (rev==0
// reads latest). The FDB gone-resolver reads at the projection's snapshot
// revision so the join stays internally consistent.
func (h heartbeatProjection) NodeByIDAtRev(ctx context.Context, id uuid.UUID, rev int64) (store.Node, error) {
	return h.s.nodeByIDAtRev(ctx, id, rev)
}

// UpdateNodeHeartbeat refreshes the agent-reported capability + migration
// fields, bumping updated_at. Architecture, labels, and status are left
// untouched (the handler/reconciler own them).
func (h heartbeatProjection) UpdateNodeHeartbeat(ctx context.Context, arg store.UpdateNodeHeartbeatParams) error {
	n, err := h.s.NodeByID(ctx, arg.ID)
	if err != nil {
		return err
	}
	n.AgentVersion = arg.AgentVersion
	n.MigrationHost = arg.MigrationHost
	n.MigrationPortRangeStart = arg.MigrationPortRangeStart
	n.MigrationPortRangeEnd = arg.MigrationPortRangeEnd
	n.CPUCoresTotal = arg.CPUCoresTotal
	n.CPUCoresAvailable = arg.CPUCoresAvailable
	n.CPUModel = arg.CPUModel
	n.CpuFlags = arg.CpuFlags
	n.MemoryTotalMib = arg.MemoryTotalMib
	n.MemoryAvailableMib = arg.MemoryAvailableMib
	n.Hugepages2mibTotal = arg.Hugepages2mibTotal
	n.Hugepages1gibTotal = arg.Hugepages1gibTotal
	n.KernelVersion = arg.KernelVersion
	n.QEMUVersion = arg.QEMUVersion
	n.NumaTopology = arg.NumaTopology
	n.Capabilities = arg.Capabilities
	n.SystemDiskTotalBytes = arg.SystemDiskTotalBytes
	n.SystemDiskAvailableBytes = arg.SystemDiskAvailableBytes
	now := time.Now().UTC()
	n.LastHeartbeatAt = &now
	n.UpdatedAt = now
	return h.s.c.PutJSON(ctx, nodeKey(arg.ID), n)
}

// UpdateNodeMemoryPressure persists the memory-pressure transition.
func (h heartbeatProjection) UpdateNodeMemoryPressure(ctx context.Context, arg store.UpdateNodeMemoryPressureParams) error {
	n, err := h.s.NodeByID(ctx, arg.ID)
	if err != nil {
		return err
	}
	n.MemoryPressureSince = arg.MemoryPressureSince
	n.MemoryPressureCount = arg.MemoryPressureCount
	n.UpdatedAt = time.Now().UTC()
	return h.s.c.PutJSON(ctx, nodeKey(arg.ID), n)
}

// UpdateNodeSystemDiskPressure persists the system-disk pressure transition.
func (h heartbeatProjection) UpdateNodeSystemDiskPressure(ctx context.Context, arg store.UpdateNodeSystemDiskPressureParams) error {
	n, err := h.s.NodeByID(ctx, arg.ID)
	if err != nil {
		return err
	}
	n.SystemDiskPressureSince = arg.SystemDiskPressureSince
	n.SystemDiskPressureCount = arg.SystemDiskPressureCount
	n.UpdatedAt = time.Now().UTC()
	return h.s.c.PutJSON(ctx, nodeKey(arg.ID), n)
}

// FilterExistingVMIDs returns the subset of ids that reference a live vms row.
func (h heartbeatProjection) FilterExistingVMIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, err := h.s.VMByID(ctx, id); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// FilterVMIDsPinnedToNode returns the subset of ids whose live vms row is
// pinned to nodeID, or which have an active (non-terminal) migration whose
// target is nodeID. The pin is read from the row itself (the field the
// scheduler writes), not the pinned-node index; ids with a missing row are
// skipped, mirroring FilterExistingVMIDs.
//
// The migration-target arm makes the placement gate migration-aware (migration
// design D3): during live migration the guest can be live on the target while
// the pin is still the source for a brief pre-cutover window, so both the source
// (via the unchanged pin) and the target (via the active migration) are
// admitted. A terminal migration does not admit its (former) target -
// activeMigrationsOnNode excludes terminal phases - and after cutover the pin
// has moved to the target, so the normal pin check carries the VM.
func (h heartbeatProjection) FilterVMIDsPinnedToNode(ctx context.Context, nodeID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	migs, err := h.s.activeMigrationsOnNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	target := make(map[uuid.UUID]struct{}, len(migs))
	for _, m := range migs {
		if m.TargetNodeID != nil && *m.TargetNodeID == nodeID {
			target[m.VmID] = struct{}{}
		}
	}

	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		vm, err := h.s.VMByID(ctx, id)
		if err != nil {
			continue
		}
		if vm.PinnedNodeID != nil && *vm.PinnedNodeID == nodeID {
			out = append(out, id)
			continue
		}
		if _, ok := target[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// UpsertVMRuntime projects a per-VM runtime snapshot, stamping last_observed_at
// and maintaining the vm_runtime-by-node index that DeleteNode consumes.
func (h heartbeatProjection) UpsertVMRuntime(ctx context.Context, arg store.UpsertVMRuntimeParams) error {
	var rt store.VMRuntime
	found, err := h.s.c.GetJSON(ctx, vmRuntimeKey(arg.VmID), &rt)
	if err != nil {
		return err
	}
	oldNode := rt.CurrentNodeID
	if !found {
		rt = store.VMRuntime{VmID: arg.VmID}
	}
	rt.CurrentNodeID = arg.CurrentNodeID
	rt.Phase = arg.Phase
	rt.ObservedGeneration = arg.ObservedGeneration
	rt.QEMUPID = arg.QEMUPID
	rt.LastStartedAt = arg.LastStartedAt
	rt.LastErrorMessage = arg.LastErrorMessage
	now := time.Now().UTC()
	rt.LastObservedAt = &now

	// Commit the runtime row and the (conditional) by-node index Delete/Put in
	// one Txn so a crash cannot leave the row's current_node_id pointing at a
	// node the index no longer (or does not yet) reference. DeleteNode consumes
	// this index, so a dangling entry would make a force-delete miss a VM
	// (audit R2-L9). Row + at most one Delete + at most one Put is <= 3 ops,
	// well under etcd's per-txn limit.
	val, err := etcd.Marshal(rt)
	if err != nil {
		return err
	}
	ops := []clientv3.Op{clientv3.OpPut(vmRuntimeKey(arg.VmID), string(val))}
	if oldNode != nil && (arg.CurrentNodeID == nil || *oldNode != *arg.CurrentNodeID) {
		ops = append(ops, clientv3.OpDelete(vmRuntimeNodeIndexKey(*oldNode, arg.VmID)))
	}
	if arg.CurrentNodeID != nil && (oldNode == nil || *oldNode != *arg.CurrentNodeID) {
		ops = append(ops, clientv3.OpPut(vmRuntimeNodeIndexKey(*arg.CurrentNodeID, arg.VmID), arg.VmID.String()))
	}
	if _, err := h.s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("upsert vm_runtime txn: %v", err)
	}
	return nil
}

// vmRuntimeNodeIndexKey is the by-node index entry for a vm_runtime row, the
// per-VM leaf under vmRuntimeNodeIndexPrefix that DeleteNode ranges to find the
// VMs to orphan.
func vmRuntimeNodeIndexKey(node, vmID uuid.UUID) string {
	return etcd.Key("index", "vm_runtime", "node", node.String(), vmID.String())
}

// UpdateStoragePoolReconciliation applies the agent's reconciliation report for
// a pool, matched by (node_id, lower(name)). Soft-deleted / missing pools are
// skipped silently.
func (h heartbeatProjection) UpdateStoragePoolReconciliation(ctx context.Context, arg store.UpdateStoragePoolReconciliationParams) error {
	id, found, err := h.s.resolveGuard(ctx, storagePoolNodeNameGuard(arg.NodeID, arg.Name))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	p, err := h.s.StoragePoolByID(ctx, id)
	if err != nil {
		return nil //nolint:nilerr // soft-deleted/missing pool is skipped silently per the SQL contract
	}
	p.ReconciliationStatus = arg.ReconciliationStatus
	p.ReconciliationError = arg.ReconciliationError
	now := time.Now().UTC()
	p.LastReconciledAt = &now
	p.UpdatedAt = now
	return h.s.c.PutJSON(ctx, storagePoolKey(id), p)
}

// StoragePoolIDByNodeName resolves a pool's UUID from the (node_id, lower(name))
// uniqueness guard — the same join key UpdateStoragePoolReconciliation matches
// on. found is false (nil error) when no guard exists (pool deleted mid-tick).
func (h heartbeatProjection) StoragePoolIDByNodeName(ctx context.Context, nodeID uuid.UUID, name string) (uuid.UUID, bool, error) {
	return h.s.resolveGuard(ctx, storagePoolNodeNameGuard(nodeID, name))
}

// UpsertPoolImageInventory persists the agent-reported observed image inventory
// for a pool, delegating to the store-level blind put (empty slice clears).
func (h heartbeatProjection) UpsertPoolImageInventory(ctx context.Context, poolID uuid.UUID, images []store.PoolImage) error {
	return h.s.UpsertPoolImageInventory(ctx, poolID, images)
}

// UpsertNodeBlobInventory persists the agent-reported observed blob inventory
// for a node, delegating to the store-level blind put (empty slice clears).
func (h heartbeatProjection) UpsertNodeBlobInventory(ctx context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error {
	return h.s.UpsertNodeBlobInventory(ctx, nodeID, blobs)
}

// ListStoragePoolsByNode returns the non-deleted pools on a node.
func (h heartbeatProjection) ListStoragePoolsByNode(ctx context.Context, nodeID uuid.UUID) ([]store.StoragePool, error) {
	items, err := h.s.c.Range(ctx, storagePoolPrefix())
	if err != nil {
		return nil, err
	}
	var out []store.StoragePool
	for _, kv := range items {
		var p store.StoragePool
		if err := json.Unmarshal(kv.Value, &p); err != nil {
			return nil, err
		}
		if p.DeletedAt == nil && p.NodeID == nodeID {
			out = append(out, p)
		}
	}
	return out, nil
}

// UpsertNetworkNodeStatus applies the agent's reconciliation report for a
// cluster-wide network on this node, keyed by (network_id, node_id).
func (h heartbeatProjection) UpsertNetworkNodeStatus(ctx context.Context, arg store.UpsertNetworkNodeStatusParams) error {
	return h.s.UpsertNetworkNodeStatus(ctx, arg)
}

// UpsertAgentWireguard ingests the agent's observed WG state, allocating its
// overlay identity on first report.
func (h heartbeatProjection) UpsertAgentWireguard(ctx context.Context, arg store.UpsertAgentWireguardParams) error {
	return h.s.UpsertAgentWireguard(ctx, arg)
}

// ListAgentWireguard returns every agent WG fabric record for the down-channel.
func (h heartbeatProjection) ListAgentWireguard(ctx context.Context) ([]store.AgentWireguard, error) {
	return h.s.ListAgentWireguard(ctx)
}

// ListAgentWireguardAtRev returns every agent WG fabric record pinned to an MVCC
// revision (rev==0 reads latest). The FDB projection reads at its snapshot
// revision so the VTEP-IP map matches the placement list.
func (h heartbeatProjection) ListAgentWireguardAtRev(ctx context.Context, rev int64) ([]store.AgentWireguard, error) {
	return h.s.listAgentWireguardAtRev(ctx, rev)
}

// AgentWireguardByNodeID returns the node's WG fabric record (or ErrNotFound)
// for the self-overlay-ip down-channel field.
func (h heartbeatProjection) AgentWireguardByNodeID(ctx context.Context, nodeID uuid.UUID) (store.AgentWireguard, error) {
	return h.s.AgentWireguardByNodeID(ctx, nodeID)
}

// OverlaySupernet returns the cluster overlay supernet so the handler can
// render self_overlay_ip with the supernet prefix length.
func (h heartbeatProjection) OverlaySupernet(ctx context.Context) (netip.Prefix, error) {
	return h.s.OverlaySupernet(ctx)
}

// UnderlayMTU returns the seeded physical underlay MTU so the handler can derive
// the otwg0 link MTU (underlay - store.WGEncapOverhead) for the down-channel.
func (h heartbeatProjection) UnderlayMTU(ctx context.Context) (int32, error) {
	return h.s.UnderlayMTU(ctx)
}

// ListNetworks returns every non-deleted network. Networks are cluster-wide (not
// node-scoped), so the projection hands the agent the full set to materialise
// on its node.
func (h heartbeatProjection) ListNetworks(ctx context.Context) ([]store.Network, error) {
	items, err := h.s.c.Range(ctx, networkPrefix())
	if err != nil {
		return nil, err
	}
	var out []store.Network
	for _, kv := range items {
		var n store.Network
		if err := json.Unmarshal(kv.Value, &n); err != nil {
			return nil, err
		}
		if n.DeletedAt == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// ListVMNicsByNetwork returns the non-deleted NIC rows attached to the network
// so the projection can build declared_networks[].reservations.
func (h heartbeatProjection) ListVMNicsByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.VMNic, error) {
	return h.s.ListVMNicsByNetwork(ctx, networkID)
}

// ListVMsForNodeDeclared returns the per-node VM desired-state inventory: live
// vms whose runtime current_node_id is the node and whose phase has not reached
// 'gone', sorted lower(name) ascending.
func (h heartbeatProjection) ListVMsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]store.ListVMsForNodeDeclaredRow, error) {
	items, err := h.s.c.Range(ctx, vmRuntimeNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, err
	}
	var out []store.ListVMsForNodeDeclaredRow
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			continue
		}
		var rt store.VMRuntime
		found, gerr := h.s.c.GetJSON(ctx, vmRuntimeKey(id), &rt)
		if gerr != nil {
			return nil, gerr
		}
		if !found || rt.Phase == store.VmPhaseGone {
			continue
		}
		vm, err := h.s.VMByID(ctx, id)
		if err != nil {
			continue
		}
		// Declared (desired) state follows the authoritative pin, not the
		// vm_runtime-by-node index. A VM present on this node only as the TARGET
		// of an in-flight migration is pinned to the source until cutover and
		// shows up in this index because the target reports it (the heartbeat
		// gate admits the migration target). It must NOT be declared
		// desired-running here yet, or this node's reconciler would Start it
		// mid-migration - while qemu-nbd is still receiving its disk - and
		// corrupt the transfer. After cutover the pin moves here and the VM is
		// declared normally. Symmetrically, the former source stops declaring it
		// once the pin leaves.
		if vm.PinnedNodeID == nil || *vm.PinnedNodeID != nodeID {
			continue
		}
		out = append(out, store.ListVMsForNodeDeclaredRow{
			Name:         vm.Name,
			DesiredPhase: vm.DesiredPhase,
			Generation:   vm.Generation,
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// ListOverlayNICPlacements returns the cluster-wide overlay NIC placements
// (MAC + owning node per overlay VNI) the FDB down-channel projection joins to
// each node's VTEP overlay IP.
func (h heartbeatProjection) ListOverlayNICPlacements(ctx context.Context) ([]store.OverlayNICPlacement, error) {
	return h.s.ListOverlayNICPlacements(ctx)
}

// ListOverlayNICPlacementsPinned returns the overlay NIC placements together
// with the MVCC revision the join is pinned to, so the FDB down-channel
// projection can read the WG list and the gone-resolver at the same snapshot.
func (h heartbeatProjection) ListOverlayNICPlacementsPinned(ctx context.Context) ([]store.OverlayNICPlacement, int64, error) {
	return h.s.ListOverlayNICPlacementsPinned(ctx)
}

// ActiveMigrationForVM returns the non-terminal migration for vmID, if any.
func (h heartbeatProjection) ActiveMigrationForVM(ctx context.Context, vmID uuid.UUID) (store.Migration, bool, error) {
	return h.s.ActiveMigrationForVM(ctx, vmID)
}
