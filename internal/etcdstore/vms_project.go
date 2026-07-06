// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// VM-domain worker projections. These are the named atomic mutators the
// Run-form workers call, each committing in a single etcd transaction. Every
// write is idempotent (the runtime put, the soft-delete puts, and the
// task-finalize put are all blind puts of self-describing values), so a success
// is all-or-nothing under one transaction and a worker redelivery re-applies
// identical bytes for the same end state.

// projectVMCreateCASAttempts bounds the vm-row ModRevision CAS retry in
// ProjectVMCreateSuccess. A racing `vm delete` terminates the loop early (the
// re-read observes the tombstone and skips), so this only caps benign contention
// from a concurrent VM-row writer.
const projectVMCreateCASAttempts = 5

// ProjectVMCreateSuccess upserts the VM's runtime row (running), stamps the
// agent-resolved image content digest onto the self-describing VM row, and
// finalizes the create task - all in one transaction, guarded by a CAS on the VM
// row's ModRevision. If a `vm delete` soft-deleted the VM while the create ran
// (the delete-during-create seam), the runtime row is NOT resurrected: the CAS
// loses and the re-read observes the tombstone, so only the create task is
// finalized. The runtime and task writes are otherwise idempotent (the digest
// write is skipped when the row already carries the same bytes), so a worker
// redelivery re-applies the same runtime + task value and re-reads an already-
// stamped digest, leaving the end state unchanged.
//
// imageSHA256 is the digest the agent computed for the materialized image
// (empty hints the caller had nothing to stamp). A create that pinned a digest
// up front already carries it, so the digest write only fires for compute-mode
// creates (no --image-sha256), surfacing the resolved digest on the VM view.
func (s *Store) ProjectVMCreateSuccess(ctx context.Context, rt store.UpsertVMRuntimeParams, fin store.UpdateTaskFinalizedParams, imageSHA256 []byte) error {
	for attempt := 0; attempt < projectVMCreateCASAttempts; attempt++ {
		taskVal, err := s.finalizedTaskValue(ctx, fin)
		if err != nil {
			return err
		}
		vm, vmRev, err := s.vmWithRev(ctx, rt.VmID)
		if errors.Is(err, store.ErrNotFound) {
			// A `vm delete` soft-deleted the VM while the create ran on the agent.
			// Do NOT resurrect the runtime row on a tombstoned VM - a dangling row
			// pollutes node-delete gating, scheduler capacity, and blob GC. Finalize
			// the create task only so its job settles. (The just-created qemu is
			// reclaimed separately: the agent does not auto-reap undeclared VMs, so
			// orphan-qemu teardown across this seam is a tracked follow-up.)
			if _, err := s.c.Raw().Txn(ctx).
				Then(clientv3.OpPut(taskKey(fin.ID), string(taskVal))).
				Commit(); err != nil {
				return fmt.Errorf("project vm create (tombstoned) txn: %v", err)
			}
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		runtimeVal, err := etcd.Marshal(vmRuntimeFromUpsert(rt, now))
		if err != nil {
			return err
		}
		ops := []clientv3.Op{
			clientv3.OpPut(vmRuntimeKey(rt.VmID), string(runtimeVal)),
			clientv3.OpPut(taskKey(fin.ID), string(taskVal)),
		}
		digestOp, err := digestOpFromVM(vm, imageSHA256, now)
		if err != nil {
			return err
		}
		if digestOp != nil {
			ops = append(ops, *digestOp)
		}
		// CAS on the VM row's ModRevision: a `vm delete` soft-deleting the VM
		// between the read and this commit bumps the rev, so the write loses and we
		// retry - the re-read then observes the tombstone and skips the runtime
		// resurrection. Closes the read-then-put TOCTOU.
		resp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(vmKey(rt.VmID)), "=", vmRev)).
			Then(ops...).
			Commit()
		if err != nil {
			return fmt.Errorf("project vm create txn: %v", err)
		}
		if resp.Succeeded {
			return nil
		}
		// Lost CAS: the VM row moved (a racing delete or a concurrent VM-row write).
		// Retry: the re-read observes the tombstone (skip) or the new rev.
	}
	return fmt.Errorf("project vm create: vm row CAS exhausted after %d attempts", projectVMCreateCASAttempts)
}

// digestOpFromVM returns the VM-row put that records the agent-resolved image
// digest, or nil when there is nothing to write: an empty digest, or a row that
// already carries the same bytes (a pinned create, or a redelivery of a compute-
// mode create). The no-op-on-equal guard keeps the projection idempotent and
// leaves a pinned VM's updated_at untouched. The caller supplies the already-read
// VM (held under the create projection's ModRevision CAS), so this does no read
// and cannot race the tombstone check.
func digestOpFromVM(vm store.VM, imageSHA256 []byte, now time.Time) (*clientv3.Op, error) {
	if len(imageSHA256) == 0 || bytes.Equal(vm.ImageSHA256, imageSHA256) {
		return nil, nil
	}
	vm.ImageSHA256 = imageSHA256
	vm.UpdatedAt = now
	val, err := etcd.Marshal(vm)
	if err != nil {
		return nil, err
	}
	op := clientv3.OpPut(vmKey(vm.ID), string(val))
	return &op, nil
}

// ProjectVMLifecycleSuccess writes the VM's desired_phase (when non-empty), its
// observed runtime phase, and finalizes the lifecycle task - all in one
// transaction. Every write here is idempotent, so no compare is needed; the
// transaction only groups them so a poll never sees a half-applied terminal.
// An empty desiredPhase skips the vms write (reboot leaves user intent unchanged).
func (s *Store) ProjectVMLifecycleSuccess(ctx context.Context, vmID uuid.UUID, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, fin store.UpdateTaskFinalizedParams) error {
	now := time.Now().UTC()
	var ops []clientv3.Op

	if desiredPhase != "" {
		vm, err := s.VMByID(ctx, vmID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Matches the SQL `where deleted_at is null` no-op: a deleted VM's
			// desired_phase is not rewritten.
		case err != nil:
			return err
		default:
			vm.DesiredPhase = desiredPhase
			vm.UpdatedAt = now
			val, merr := etcd.Marshal(vm)
			if merr != nil {
				return merr
			}
			ops = append(ops, clientv3.OpPut(vmKey(vmID), string(val)))
		}
	}

	// vm_runtime.phase: a missing runtime row is a no-op (the SQL update
	// affects zero rows), so the projection still finalizes the task.
	var runtime store.VMRuntime
	found, err := s.c.GetJSON(ctx, vmRuntimeKey(vmID), &runtime)
	if err != nil {
		return err
	}
	if found {
		runtime.Phase = runtimePhase
		runtime.LastObservedAt = &now
		val, merr := etcd.Marshal(runtime)
		if merr != nil {
			return merr
		}
		ops = append(ops, clientv3.OpPut(vmRuntimeKey(vmID), string(val)))
	}

	taskVal, err := s.finalizedTaskValue(ctx, fin)
	if err != nil {
		return err
	}
	ops = append(ops, clientv3.OpPut(taskKey(fin.ID), string(taskVal)))

	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("project vm lifecycle txn: %v", err)
	}
	return nil
}

// ProjectVMDeleteSuccess soft-deletes a VM and its disks, drops the observed
// runtime row, and finalizes the delete task - all in one transaction. Every
// write is an idempotent blind put or delete, so the projection commits
// unconditionally and a worker redelivery re-applies the same soft-delete for
// the same end state. Soft-delete drops the name guard (name reusable) and the
// owner/firmware/pinned-node indexes, and each disk's vm + pool indexes, so no
// index scan or blocking-count sees the deleted rows.
func (s *Store) ProjectVMDeleteSuccess(ctx context.Context, vm store.VM, fin store.UpdateTaskFinalizedParams) error {
	now := time.Now().UTC()
	taskVal, err := s.finalizedTaskValue(ctx, fin)
	if err != nil {
		return err
	}
	ops, err := s.vmDeleteBaseOps(ctx, vm, now, taskVal, fin.ID)
	if err != nil {
		return err
	}
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("project vm delete txn: %v", err)
	}
	return nil
}

// vmDeleteBaseOps builds the soft-delete operations: the VM row + guard/index
// removals, the runtime delete, the per-disk soft-delete + index removals, the
// NIC removals, and the task finalize.
func (s *Store) vmDeleteBaseOps(ctx context.Context, vm store.VM, now time.Time, taskVal []byte, taskID uuid.UUID) ([]clientv3.Op, error) {
	vm.DeletedAt = &now
	vm.UpdatedAt = now
	vmVal, err := etcd.Marshal(vm)
	if err != nil {
		return nil, err
	}
	ops := []clientv3.Op{
		clientv3.OpPut(vmKey(vm.ID), string(vmVal)),
		clientv3.OpDelete(vmNameGuard(vm.Name)),
		clientv3.OpDelete(vmRuntimeKey(vm.ID)),
	}
	ops = append(ops, vmIndexDeleteOps(vm)...)

	// Release the vm_runtime-by-node secondary index the heartbeat wrote
	// (reindexRuntimeNode). It keys off the runtime's current_node_id, which the
	// vm row does not carry, so source the node from the runtime row before the
	// primary delete above takes effect. A missing runtime row means the VM was
	// never placed, so there is no index entry to drop.
	rt, err := s.VMRuntimeByID(ctx, vm.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	case rt.CurrentNodeID != nil:
		ops = append(ops, clientv3.OpDelete(etcd.Key("index", "vm_runtime", "node", rt.CurrentNodeID.String(), vm.ID.String())))
	}

	disks, err := s.disksOfVM(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	for _, d := range disks {
		d.DeletedAt = &now
		d.UpdatedAt = now
		dVal, err := etcd.Marshal(d)
		if err != nil {
			return nil, err
		}
		ops = append(ops,
			clientv3.OpPut(vmDiskKey(d.ID), string(dVal)),
			clientv3.OpDelete(etcd.Key("index", "vm_disks", "vm", d.VmID.String(), d.ID.String())),
			clientv3.OpDelete(etcd.Key("index", "vm_disks", "pool", d.StoragePoolID.String(), d.ID.String())),
		)
	}
	nicOps, err := s.vmNicDeleteOps(ctx, vm.ID, now)
	if err != nil {
		return nil, err
	}
	ops = append(ops, nicOps...)

	ops = append(ops, clientv3.OpPut(taskKey(taskID), string(taskVal)))
	return ops, nil
}

// vmIndexDeleteOps returns the secondary-index removals matching vmIndexOps:
// owner, and (when set) firmware / pinned-node.
func vmIndexDeleteOps(vm store.VM) []clientv3.Op {
	ops := []clientv3.Op{
		clientv3.OpDelete(etcd.Key("index", "vms", "owner", vm.OwnerID.String(), vm.ID.String())),
	}
	if vm.FirmwareID != nil {
		ops = append(ops, clientv3.OpDelete(etcd.Key("index", "vms", "firmware", vm.FirmwareID.String(), vm.ID.String())))
	}
	if vm.PinnedNodeID != nil {
		ops = append(ops, clientv3.OpDelete(etcd.Key("index", "vms", "pinned_node", vm.PinnedNodeID.String(), vm.ID.String())))
	}
	return ops
}

// vmRuntimeFromUpsert projects UpsertVMRuntimeParams onto a fresh runtime row,
// stamping last_observed_at and updated_at. Used by the create projection where
// the runtime row is written for the first time.
func vmRuntimeFromUpsert(p store.UpsertVMRuntimeParams, now time.Time) store.VMRuntime {
	return store.VMRuntime{
		VmID:               p.VmID,
		CurrentNodeID:      p.CurrentNodeID,
		Phase:              p.Phase,
		ObservedGeneration: p.ObservedGeneration,
		QEMUPID:            p.QEMUPID,
		LastStartedAt:      p.LastStartedAt,
		LastErrorMessage:   p.LastErrorMessage,
		LastObservedAt:     &now,
		UpdatedAt:          now,
	}
}

// finalizedTaskValue reads the task, applies the terminal status / result /
// error and a finished_at stamp, and returns the marshaled value ready to put
// inside a projection transaction.
func (s *Store) finalizedTaskValue(ctx context.Context, fin store.UpdateTaskFinalizedParams) ([]byte, error) {
	t, err := s.TaskByID(ctx, fin.ID)
	if err != nil {
		return nil, err
	}
	t.Status = fin.Status
	t.Result = fin.Result
	t.Error = fin.Error
	now := time.Now().UTC()
	t.FinishedAt = &now
	return etcd.Marshal(t)
}

// isCommittedTerminal reports whether a task has reached a committed-terminal
// status. Only success and cancelled qualify; failed is retryable (failRun
// finalizes failed but the dispatcher requeues the job), so a failed task must
// still re-run.
func isCommittedTerminal(s store.TaskStatus) bool {
	return s == store.TaskStatusSuccess || s == store.TaskStatusCancelled
}
