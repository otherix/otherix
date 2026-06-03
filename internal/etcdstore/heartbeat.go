// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

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

// LookupFirmwareByCatalog resolves a (name, architecture, type) triple to a
// firmware id via the name+arch guard, or store.ErrNotFound when the catalogue
// has no matching entry (the handler skips the entry with a WARN).
func (h heartbeatProjection) LookupFirmwareByCatalog(ctx context.Context, arg store.LookupFirmwareByCatalogParams) (uuid.UUID, error) {
	id, found, err := h.s.resolveGuard(ctx, firmwareNameArchGuard(arg.Architecture, arg.Name))
	if err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, store.ErrNotFound
	}
	fw, err := h.s.FirmwareByID(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	if fw.Type != arg.Type {
		return uuid.Nil, store.ErrNotFound
	}
	return id, nil
}

// UpsertNodeFirmware refreshes the (node, firmware) availability row, stamping
// reported_at.
func (h heartbeatProjection) UpsertNodeFirmware(ctx context.Context, arg store.UpsertNodeFirmwareParams) error {
	nf := store.NodeFirmware{
		NodeID:     arg.NodeID,
		FirmwareID: arg.FirmwareID,
		CodePath:   arg.CodePath,
		VarsPath:   arg.VarsPath,
		Available:  arg.Available,
		ReportedAt: time.Now().UTC(),
	}
	return h.s.c.PutJSON(ctx, etcd.Key("node_firmwares", arg.NodeID.String(), arg.FirmwareID.String()), nf)
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
	if err := h.s.c.PutJSON(ctx, vmRuntimeKey(arg.VmID), rt); err != nil {
		return err
	}
	return h.reindexRuntimeNode(ctx, arg.VmID, oldNode, arg.CurrentNodeID)
}

// reindexRuntimeNode keeps the vm_runtime-by-node index in step with a runtime's
// current_node_id transition.
func (h heartbeatProjection) reindexRuntimeNode(ctx context.Context, vmID uuid.UUID, oldNode, newNode *uuid.UUID) error {
	if oldNode != nil && (newNode == nil || *oldNode != *newNode) {
		if _, err := h.s.c.Delete(ctx, etcd.Key("index", "vm_runtime", "node", oldNode.String(), vmID.String())); err != nil {
			return err
		}
	}
	if newNode != nil && (oldNode == nil || *oldNode != *newNode) {
		if err := h.s.c.Put(ctx, etcd.Key("index", "vm_runtime", "node", newNode.String(), vmID.String()), []byte(vmID.String())); err != nil {
			return err
		}
	}
	return nil
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

// ListVMsForNodeDeclared returns the per-node VM desired-state inventory: live
// vms whose runtime current_node_id is the node and whose phase has not reached
// 'gone', sorted lower(name) ascending.
func (h heartbeatProjection) ListVMsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]store.ListVMsForNodeDeclaredRow, error) {
	// Revision-pin the whole join: capture the snapshot revision from the index
	// range and read every per-VM row (runtime + primary) at it, mirroring
	// ListOverlayNICPlacementsPinned. The index range and the per-VM reads then
	// form one consistent MVCC snapshot, so an index/primary race cannot
	// manufacture a phantom omission of a still-wanted, still-running VM.
	items, rev, err := h.s.c.RangeRev(ctx, vmRuntimeNodeIndexPrefix(nodeID), 0)
	if err != nil {
		return nil, err
	}
	var out []store.ListVMsForNodeDeclaredRow
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			// A malformed index VALUE is corruption (a phantom index entry). Log
			// it loudly and skip it: there is no real VM behind an unparseable id,
			// so it cannot correspond to a live VM that could be wrongly pruned,
			// and a single corrupt entry must not brick a node's heartbeat
			// forever. This is the ONLY safe silent-skip here - unlike a transient
			// primary-read error below, which CAN hide a live VM and so fails
			// closed.
			slog.ErrorContext(ctx, "corrupt vm_runtime-by-node index value, skipping",
				"node_id", nodeID, "raw_value", string(kv.Value), "parse_error", perr)
			continue
		}
		rt, rerr := h.s.vmRuntimeByIDAtRev(ctx, id, rev)
		if rerr != nil {
			if errors.Is(rerr, store.ErrNotFound) {
				// No runtime row at this snapshot - nothing to declare.
				continue
			}
			// Transient read failure: fail closed (see the primary-read note below).
			return nil, rerr
		}
		if rt.Phase == store.VmPhaseGone {
			continue
		}
		vm, verr := h.s.vmByIDAtRev(ctx, id, rev)
		if verr != nil {
			// Fail CLOSED on a transient primary-read failure. Only a genuine
			// ErrNotFound is a safe omission: the runtime-node index entry is
			// deleted atomically with the vms soft-delete, so at this pinned
			// revision an index entry present with the vms row absent means the VM
			// is genuinely gone (or a pre-A2 orphaned entry, also genuinely gone).
			// Any OTHER error (a transient etcd read blip) must abort the whole
			// declared-set load: a partial set is decoded by the agent as an
			// authoritative desired set and could drive the absence-debounce to
			// prune a VM the CP still wants and that is still running.
			if errors.Is(verr, store.ErrNotFound) {
				continue
			}
			return nil, verr
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
