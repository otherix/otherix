// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// CreateScheduledVM runs the placement-locked critical section for a VM create:
// it hands a placement reader to plan (which scores candidates with the
// scheduler and builds the vms / vm_disks / tasks rows + job args), then
// persists those rows + the backing job in one transaction. A uq_vms_name
// violation surfaces as store.ErrVMNameInUse; plan's own errors propagate
// verbatim. Returns the task id.
//
// The production admission path no longer calls this: VM create persists an
// unscheduled VM (CreateUnscheduledVM) and the vms.schedule loop binds it
// (BindScheduledVM). CreateScheduledVM is retained as the single-shot
// direct-bind write path used by the etcdstore / apie2e tests to land a fully
// bound VM (row + disk + nic + task + job) without driving the two-phase
// reconcile loop. Do not wire it back into a handler.
//
// Unlike the SQL backend's pg_advisory_xact_lock, the placement read and the
// pinned-node write are not held under one lock across the plan callback (etcd
// has no cross-callback transaction). This is safe for the single-node default
// (linearizable) and CountRunningVMsByNode counts pinned intent so concurrent
// creates spread immediately; the HA path will gate plan behind an etcd lock
// (ROADMAP).
func (s *Store) CreateScheduledVM(ctx context.Context, plan func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error) {
	writes, err := plan(placementReader{s: s})
	if err != nil {
		return uuid.Nil, err
	}

	now := time.Now().UTC()
	vm := vmFromCreateParams(writes.VM, now)
	disk := vmDiskFromCreateParams(writes.Disk, now)

	seq, jobOp, err := s.enqueueJobOp(ctx, writes.Job)
	if err != nil {
		return uuid.Nil, err
	}
	task := taskFromParams(writes.Task, seq)

	vmVal, err := etcd.Marshal(vm)
	if err != nil {
		return uuid.Nil, err
	}
	diskVal, err := etcd.Marshal(disk)
	if err != nil {
		return uuid.Nil, err
	}
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return uuid.Nil, err
	}

	guard := vmNameGuard(vm.Name)
	ops := []clientv3.Op{
		clientv3.OpPut(guard, vm.ID.String()),
		clientv3.OpPut(vmKey(vm.ID), string(vmVal)),
		clientv3.OpPut(vmDiskKey(disk.ID), string(diskVal)),
		clientv3.OpPut(taskKey(task.ID), string(taskVal)),
		jobOp,
	}
	ops = append(ops, vmIndexOps(vm)...)
	ops = append(ops, vmDiskIndexOps(disk)...)
	ops = append(ops, taskIndexOps(task)...)

	if writes.Nic != nil {
		nicOps, err := vmNicCreateOps(vmNicFromCreateParams(*writes.Nic, now))
		if err != nil {
			return uuid.Nil, err
		}
		ops = append(ops, nicOps...)
	}

	conds := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)}
	var macGuard string
	if writes.Nic != nil {
		macGuard = vmNicMACGuard(writes.Nic.NetworkID, writes.Nic.MacAddress)
		conds = append(conds, clientv3.Compare(clientv3.CreateRevision(macGuard), "=", 0))
	}

	resp, err := s.c.Raw().Txn(ctx).If(conds...).Then(ops...).Commit()
	if err != nil {
		return uuid.Nil, err
	}
	if !resp.Succeeded {
		// Two compares can fail: the VM-name guard and (with a NIC) the
		// per-network MAC guard. Disambiguate by re-issuing the MAC-guard
		// compare alone - if it also fails the MAC key already exists, so the
		// collision was the MAC (which the handler re-mints); otherwise the
		// name guard fired.
		if macGuard != "" {
			// Best-effort: a concurrent delete of the MAC key could skew this to ErrVMNameInUse; both are 409 conflicts and the guard's atomicity already prevents any uniqueness violation.
			chk, cerr := s.c.Raw().Txn(ctx).
				If(clientv3.Compare(clientv3.CreateRevision(macGuard), "=", 0)).
				Commit()
			if cerr == nil && chk != nil && !chk.Succeeded {
				return uuid.Nil, store.ErrVMNicMACConflict
			}
		}
		return uuid.Nil, store.ErrVMNameInUse
	}
	return task.ID, nil
}

// vmFromCreateParams projects CreateVMParams onto a store.VM, defaulting
// desired_phase to running and generation to 1 (the SQL column defaults). The VM
// row carries the self-describing image fields (url/sha256/format) the create
// request resolved, replacing the former template reference.
func vmFromCreateParams(p store.CreateVMParams, now time.Time) store.VM {
	imageURL := p.ImageURL
	imageSHA := p.ImageSHA256
	if p.SourceSnapshotID != nil {
		// A snapshot-sourced VM has no image lineage - the disk is the snapshot
		// state, not a base image. Carrying the base image fields would be a lie,
		// so the store drops them regardless of what the params carry.
		imageURL = ""
		imageSHA = nil
	}
	return store.VM{
		ID:                p.ID,
		OwnerID:           p.OwnerID,
		Name:              p.Name,
		Description:       p.Description,
		DesiredPhase:      store.VmDesiredPhaseRunning,
		Architecture:      p.Architecture,
		ImageURL:          imageURL,
		ImageSHA256:       imageSHA,
		ImageFormat:       p.ImageFormat,
		CpuCores:          p.CpuCores,
		MemoryMib:         p.MemoryMib,
		CPUModel:          p.CPUModel,
		MachineType:       p.MachineType,
		FirmwareID:        p.FirmwareID,
		SourceSnapshotID:  p.SourceSnapshotID,
		PinnedNodeID:      p.PinnedNodeID,
		UserData:          p.UserData,
		CloudInitDisabled: p.CloudInitDisabled,
		NetworkConfig:     p.NetworkConfig,
		Labels:            p.Labels,
		SchedulingSpec:    p.SchedulingSpec,
		Generation:        1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

// vmDiskFromCreateParams projects CreateVMDiskParams onto a store.VMDisk,
// minting the disk id (the SQL default) and defaulting generation to 1. The root
// disk's source is always an image now (the template source kind is gone).
func vmDiskFromCreateParams(p store.CreateVMDiskParams, now time.Time) store.VMDisk {
	return store.VMDisk{
		ID:            uuid.New(),
		VmID:          p.VmID,
		StoragePoolID: p.StoragePoolID,
		DeviceOrder:   p.DeviceOrder,
		Bus:           p.Bus,
		SizeGib:       p.SizeGib,
		SourceKind:    "image",
		Format:        p.Format,
		ReadOnly:      p.ReadOnly,
		CacheMode:     p.CacheMode,
		Discard:       p.Discard,
		BootOrder:     p.BootOrder,
		Generation:    1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// vmIndexOps returns the secondary-index writes that earlier slices consume:
// name->id (guard, written separately), owner, firmware, pinned-node.
func vmIndexOps(vm store.VM) []clientv3.Op {
	ops := []clientv3.Op{
		clientv3.OpPut(etcd.Key("index", "vms", "owner", vm.OwnerID.String(), vm.ID.String()), vm.ID.String()),
	}
	if vm.FirmwareID != nil {
		ops = append(ops, clientv3.OpPut(etcd.Key("index", "vms", "firmware", vm.FirmwareID.String(), vm.ID.String()), vm.ID.String()))
	}
	if vm.PinnedNodeID != nil {
		ops = append(ops, clientv3.OpPut(etcd.Key("index", "vms", "pinned_node", vm.PinnedNodeID.String(), vm.ID.String()), vm.ID.String()))
	}
	return ops
}

// vmDiskIndexOps returns the vm and pool index writes for a disk (consumed by
// ListVMDisksByVM and the pool delete-block / effective-capacity).
func vmDiskIndexOps(d store.VMDisk) []clientv3.Op {
	return []clientv3.Op{
		clientv3.OpPut(etcd.Key("index", "vm_disks", "vm", d.VmID.String(), d.ID.String()), d.ID.String()),
		clientv3.OpPut(etcd.Key("index", "vm_disks", "pool", d.StoragePoolID.String(), d.ID.String()), d.ID.String()),
	}
}

// PlacementQuerier returns the store's scheduler read surface, so a caller that
// runs SchedulePlacement OUTSIDE the placement-locked bind transaction (the
// migration worker, which never re-pins the VM at placement time - spec D3) can
// score candidates against live cluster state. The returned Querier is a thin
// read view over the store; it holds no lock and commits nothing.
func (s *Store) PlacementQuerier() scheduler.Querier { return placementReader{s: s} }

// AcquirePlacementLock acquires the cluster-wide advisory lock named by lockKey.
// It is the standalone entry the migration worker takes around its placement ->
// bind window (placeAndBind), which spans two separate store calls (SchedulePlacement
// then BindMigrationTarget) rather than a single transaction the way vm.create does.
// On the single-node default this is a no-op (etcd writes are linearizable); the HA
// path will take an etcd lock keyed by lockKey. It delegates to the placementReader
// so create and migrate share one lock implementation.
func (s *Store) AcquirePlacementLock(ctx context.Context, lockKey int64) error {
	return placementReader{s: s}.AcquirePlacementLock(ctx, lockKey)
}

// placementReader is the etcd-backed scheduler read surface handed to the plan
// callback. It composes the per-pool/per-node effective views with the
// eligibility predicates the SQL placement queries encode.
type placementReader struct{ s *Store }

// AcquirePlacementLock is a no-op on the single-node default (etcd writes are
// linearizable). The HA path will take an etcd lock keyed by lockKey.
func (r placementReader) AcquirePlacementLock(ctx context.Context, lockKey int64) error {
	return nil
}

// ListStoragePoolsByName returns every per-node instance sharing the name.
func (r placementReader) ListStoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error) {
	return r.s.StoragePoolsByName(ctx, name)
}

// ListEligiblePoolsByName enumerates pool instances of the name on ready,
// uncordoned, unpressured nodes (and pools without their own disk pressure),
// each with the pool + node effective views.
func (r placementReader) ListEligiblePoolsByName(ctx context.Context, name string) ([]store.ListEligiblePoolsByNameRow, error) {
	pairs, err := r.s.poolNodePairs(ctx, name, func(p store.PoolEffectiveCapacity, n store.NodeEffectiveAvailability) bool {
		return nodeSchedulable(n) && n.MemoryPressureSince == nil && n.SystemDiskPressureSince == nil && p.DiskPressureSince == nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.ListEligiblePoolsByNameRow, 0, len(pairs))
	for _, pr := range pairs {
		out = append(out, store.ListEligiblePoolsByNameRow{PoolEffectiveCapacity: pr.pool, NodeEffectiveAvailability: pr.node})
	}
	return out, nil
}

// ListMemoryPressuredCandidatesByName is the diagnostic sibling: schedulable
// nodes whose memory pressure (only) excluded them.
func (r placementReader) ListMemoryPressuredCandidatesByName(ctx context.Context, name string) ([]store.ListMemoryPressuredCandidatesByNameRow, error) {
	pairs, err := r.s.poolNodePairs(ctx, name, func(p store.PoolEffectiveCapacity, n store.NodeEffectiveAvailability) bool {
		return nodeSchedulable(n) && n.MemoryPressureSince != nil && n.SystemDiskPressureSince == nil && p.DiskPressureSince == nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.ListMemoryPressuredCandidatesByNameRow, 0, len(pairs))
	for _, pr := range pairs {
		out = append(out, store.ListMemoryPressuredCandidatesByNameRow{PoolEffectiveCapacity: pr.pool, NodeEffectiveAvailability: pr.node})
	}
	return out, nil
}

// ListSystemDiskPressuredCandidatesByName is the diagnostic sibling for
// system-disk pressure.
func (r placementReader) ListSystemDiskPressuredCandidatesByName(ctx context.Context, name string) ([]store.ListSystemDiskPressuredCandidatesByNameRow, error) {
	pairs, err := r.s.poolNodePairs(ctx, name, func(p store.PoolEffectiveCapacity, n store.NodeEffectiveAvailability) bool {
		return nodeSchedulable(n) && n.SystemDiskPressureSince != nil && n.MemoryPressureSince == nil && p.DiskPressureSince == nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.ListSystemDiskPressuredCandidatesByNameRow, 0, len(pairs))
	for _, pr := range pairs {
		out = append(out, store.ListSystemDiskPressuredCandidatesByNameRow{PoolEffectiveCapacity: pr.pool, NodeEffectiveAvailability: pr.node})
	}
	return out, nil
}

// ListDiskPressuredPoolsByName returns pools of the name whose own disk-pressure
// flag is set, independent of the node's pressure status.
func (r placementReader) ListDiskPressuredPoolsByName(ctx context.Context, name string) ([]store.ListDiskPressuredPoolsByNameRow, error) {
	pairs, err := r.s.poolNodePairs(ctx, name, func(p store.PoolEffectiveCapacity, n store.NodeEffectiveAvailability) bool {
		return p.DiskPressureSince != nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.ListDiskPressuredPoolsByNameRow, 0, len(pairs))
	for _, pr := range pairs {
		out = append(out, store.ListDiskPressuredPoolsByNameRow{PoolEffectiveCapacity: pr.pool, NodeEffectiveAvailability: pr.node})
	}
	return out, nil
}

// ListNetworkNodeStatusByNode returns the node's per-network reconciliation
// records for the scheduler's network-aware placement filter.
func (r placementReader) ListNetworkNodeStatusByNode(ctx context.Context, nodeID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	return r.s.ListNetworkNodeStatusByNode(ctx, nodeID)
}

// NetworkByName resolves the deferred network name to its row so the bind can
// set the NIC's network id. Delegates to the store's name-guard lookup.
func (r placementReader) NetworkByName(ctx context.Context, name string) (store.Network, error) {
	return r.s.NetworkByName(ctx, name)
}

// CountRunningVMsByNode counts non-deleted VMs pinned to the node (intent),
// matching the SQL placement tie-break query.
func (r placementReader) CountRunningVMsByNode(ctx context.Context, nodeID *uuid.UUID) (int64, error) {
	if nodeID == nil {
		return 0, nil
	}
	items, err := r.s.c.Range(ctx, vmsPinnedNodeIndexPrefix(*nodeID))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			continue
		}
		vm, err := r.s.VMByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return 0, err
		}
		if vm.DesiredPhase != store.VmDesiredPhaseDeleted {
			n++
		}
	}
	return n, nil
}

// poolNodePair carries a pool/node effective pair for the placement predicates.
type poolNodePair struct {
	pool store.PoolEffectiveCapacity
	node store.NodeEffectiveAvailability
}

// poolNodePairs enumerates the (pool effective, node effective) pairs for a pool
// name, keeping those that pass the predicate, ordered by node id.
func (s *Store) poolNodePairs(ctx context.Context, name string, keep func(store.PoolEffectiveCapacity, store.NodeEffectiveAvailability) bool) ([]poolNodePair, error) {
	pools, err := s.StoragePoolsByName(ctx, name)
	if err != nil {
		return nil, err
	}
	var out []poolNodePair
	for _, p := range pools {
		node, err := s.NodeByID(ctx, p.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		poolEff, err := s.poolEffective(ctx, p)
		if err != nil {
			return nil, err
		}
		nodeEff, err := s.nodeEffective(ctx, node)
		if err != nil {
			return nil, err
		}
		if keep(poolEff, nodeEff) {
			out = append(out, poolNodePair{pool: poolEff, node: nodeEff})
		}
	}
	return out, nil
}

// nodeSchedulable reports whether a node is ready and not cordoned (the shared
// eligibility base every placement query enforces).
func nodeSchedulable(n store.NodeEffectiveAvailability) bool {
	return n.Status == store.NodeStatusReady && n.CordonedAt == nil
}
