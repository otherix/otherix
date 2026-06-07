// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// vmUnscheduledIndexKey is the secondary-index key marking a VM as awaiting
// placement. Written by CreateUnscheduledVM, deleted by BindScheduledVM and by
// the delete path. ListUnscheduledVMs ranges its prefix.
func vmUnscheduledIndexKey(id uuid.UUID) string {
	return etcd.Key("index", "vms", "unscheduled", id.String())
}

func vmUnscheduledIndexPrefix() string {
	return etcd.Key("index", "vms", "unscheduled") + "/"
}

// CreateUnscheduledVM persists a VM in the "unscheduled" state: the vms row
// (no pinned node, no disk/nic/task), the name guard, the owner / firmware
// indexes, and the unscheduled index. No placement happens here; the
// vms.schedule loop binds it later. A uq_vms_name collision surfaces as
// store.ErrVMNameInUse.
func (s *Store) CreateUnscheduledVM(ctx context.Context, p store.CreateVMParams) (uuid.UUID, error) {
	now := time.Now().UTC()
	vm := vmFromCreateParams(p, now)
	vm.SchedulingStatus = store.VMSchedulingUnscheduled
	reason := store.SchedReasonPendingSchedule
	vm.SchedulingReason = &reason
	vm.SchedulingSpec = p.SchedulingSpec
	vm.PinnedNodeID = nil

	vmVal, err := etcd.Marshal(vm)
	if err != nil {
		return uuid.Nil, err
	}
	guard := vmNameGuard(vm.Name)
	ops := []clientv3.Op{
		clientv3.OpPut(guard, vm.ID.String()),
		clientv3.OpPut(vmKey(vm.ID), string(vmVal)),
		clientv3.OpPut(vmUnscheduledIndexKey(vm.ID), vm.ID.String()),
	}
	ops = append(ops, vmIndexOps(vm)...)

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
	return vm.ID, nil
}

// ListUnscheduledVMs returns up to limit VMs awaiting placement, ordered by id
// (the index key order). limit <= 0 means no cap.
func (s *Store) ListUnscheduledVMs(ctx context.Context, limit int) ([]store.VM, error) {
	items, err := s.c.Range(ctx, vmUnscheduledIndexPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.VM, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			continue
		}
		vm, verr := s.VMByID(ctx, id)
		if verr != nil {
			// Index lag (row gone) - skip; the index is best-effort observable.
			continue
		}
		out = append(out, vm)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
