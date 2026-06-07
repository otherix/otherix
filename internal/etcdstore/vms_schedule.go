// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
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
	// SchedulingSpec is already carried through by vmFromCreateParams; the
	// pinned node is explicitly nil until the scheduler binds the VM.
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

// UpdateVMSchedulingReason records the latest unschedulable reason on the VM.
// It is a single CAS on the VM row's ModRevision and a no-op (returns nil) when
// the VM is no longer "unscheduled" - the scheduler bound it concurrently, so
// the reason is stale and must not clobber the bound state. A missing VM
// returns store.ErrNotFound; a lost CAS (the row was deleted or bound between
// the read and the commit) also returns nil, since the stale reason is dropped.
func (s *Store) UpdateVMSchedulingReason(ctx context.Context, vmID uuid.UUID, reason, message string, details []byte) error {
	resp, err := s.c.Raw().Get(ctx, vmKey(vmID))
	if err != nil {
		return err
	}
	if len(resp.Kvs) == 0 {
		return store.ErrNotFound
	}
	rev := resp.Kvs[0].ModRevision
	var vm store.VM
	if err := json.Unmarshal(resp.Kvs[0].Value, &vm); err != nil {
		return err
	}
	if vm.SchedulingStatus != store.VMSchedulingUnscheduled {
		// Bound concurrently - the reason is stale, do not clobber bound state.
		return nil
	}

	now := time.Now().UTC()
	vm.SchedulingReason = &reason
	vm.SchedulingMessage = &message
	vm.SchedulingDetails = details
	vm.LastScheduleAttemptAt = &now
	vm.UpdatedAt = now

	val, err := etcd.Marshal(vm)
	if err != nil {
		return err
	}
	// A lost CAS (the row was deleted or bound concurrently) is fine: the stale
	// reason is simply dropped and the scheduler loop skips the VM cleanly, so
	// resp.Succeeded is not inspected - both outcomes return nil.
	if _, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(vmKey(vmID)), "=", rev)).
		Then(clientv3.OpPut(vmKey(vmID), string(val))).
		Commit(); err != nil {
		return err
	}
	return nil
}
