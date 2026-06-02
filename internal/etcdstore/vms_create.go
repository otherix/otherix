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
	"github.com/otherix/otherix/internal/store"
)

// CreateScheduledVM runs the placement-locked critical section for a VM create:
// it hands a placement reader to plan (which scores candidates with the
// scheduler and builds the vms / vm_disks / tasks rows + job args), then
// persists those rows + the backing job in one transaction. A uq_vms_name
// violation surfaces as store.ErrVMNameInUse; plan's own errors propagate
// verbatim. Returns the task id.
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

	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(ops...).
		Commit()
	if err != nil {
		return uuid.Nil, err
	}
	if !resp.Succeeded {
		return uuid.Nil, store.ErrVMNameInUse
	}
	return task.ID, nil
}

// vmFromCreateParams projects CreateVMParams onto a store.VM, defaulting
// desired_phase to running and generation to 1 (the SQL column defaults).
func vmFromCreateParams(p store.CreateVMParams, now time.Time) store.VM {
	return store.VM{
		ID:                p.ID,
		OwnerID:           p.OwnerID,
		Name:              p.Name,
		Description:       p.Description,
		DesiredPhase:      store.VmDesiredPhaseRunning,
		TemplateID:        p.TemplateID,
		Architecture:      p.Architecture,
		CpuCores:          p.CpuCores,
		MemoryMib:         p.MemoryMib,
		CPUModel:          p.CPUModel,
		MachineType:       p.MachineType,
		FirmwareID:        p.FirmwareID,
		PinnedNodeID:      p.PinnedNodeID,
		UserData:          p.UserData,
		CloudInitDisabled: p.CloudInitDisabled,
		Labels:            p.Labels,
		Generation:        1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

// vmDiskFromCreateParams projects CreateVMDiskParams onto a store.VMDisk,
// minting the disk id (the SQL default) and defaulting generation to 1.
func vmDiskFromCreateParams(p store.CreateVMDiskParams, now time.Time) store.VMDisk {
	return store.VMDisk{
		ID:               uuid.New(),
		VmID:             p.VmID,
		StoragePoolID:    p.StoragePoolID,
		DeviceOrder:      p.DeviceOrder,
		Bus:              p.Bus,
		SizeGib:          p.SizeGib,
		SourceKind:       p.SourceKind,
		SourceTemplateID: p.SourceTemplateID,
		Format:           p.Format,
		ReadOnly:         p.ReadOnly,
		CacheMode:        p.CacheMode,
		Discard:          p.Discard,
		BootOrder:        p.BootOrder,
		Generation:       1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// vmIndexOps returns the secondary-index writes that earlier slices consume:
// name->id (guard, written separately), owner, template, firmware, pinned-node.
func vmIndexOps(vm store.VM) []clientv3.Op {
	ops := []clientv3.Op{
		clientv3.OpPut(etcd.Key("index", "vms", "owner", vm.OwnerID.String(), vm.ID.String()), vm.ID.String()),
	}
	if vm.TemplateID != nil {
		ops = append(ops, clientv3.OpPut(etcd.Key("index", "vms", "template", vm.TemplateID.String(), vm.ID.String()), vm.ID.String()))
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
// records for the scheduler's network-aware placement filter (ADR 0034 NL18).
func (r placementReader) ListNetworkNodeStatusByNode(ctx context.Context, nodeID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	return r.s.ListNetworkNodeStatusByNode(ctx, nodeID)
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
