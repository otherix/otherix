// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// VM-domain worker projections. These are the named atomic mutators the
// Run-form workers call, each committing in a single etcd transaction. Where a
// write is non-idempotent (the template derived_vm_count bump) the projection commits
// under a compare-on-mod-revision retry loop; the idempotent runtime and task
// writes ride in the same transaction so a success is all-or-nothing.

// projectTemplateCASRetries bounds the compare-and-set loop that protects the
// non-idempotent derived_vm_count bump against a concurrent template mutation.
const projectTemplateCASRetries = 64

// ProjectVMCreateSuccess upserts the VM's runtime row (running), increments the
// source template's derived_vm_count, and finalizes the create task - all in one
// transaction. The derived_vm_count bump is non-idempotent, so the whole
// projection commits under a compare on the template's mod-revision (bounded
// retry); the runtime and task writes are idempotent blind puts.
func (s *Store) ProjectVMCreateSuccess(ctx context.Context, rt store.UpsertVMRuntimeParams, templateID uuid.UUID, fin store.UpdateTaskFinalizedParams) error {
	now := time.Now().UTC()
	runtimeVal, err := etcd.Marshal(vmRuntimeFromUpsert(rt, now))
	if err != nil {
		return err
	}
	taskVal, err := s.finalizedTaskValue(ctx, fin)
	if err != nil {
		return err
	}

	for range projectTemplateCASRetries {
		// Worker redelivery: the task is already committed-terminal, so the
		// non-idempotent derived_vm_count bump has already been committed.
		// Skip re-projecting; the worker will CompleteJob.
		if done, err := s.projectionAlreadyCommitted(ctx, fin.ID); err != nil || done {
			return err
		}
		tmpl, modRev, found, err := s.templateWithRev(ctx, templateID)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrNotFound
		}
		tmpl.DerivedVmCount++
		tmplVal, err := etcd.Marshal(tmpl)
		if err != nil {
			return err
		}
		cmps := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(templateKey(templateID)), "=", modRev)}
		ops := []clientv3.Op{
			clientv3.OpPut(vmRuntimeKey(rt.VmID), string(runtimeVal)),
			clientv3.OpPut(templateKey(templateID), string(tmplVal)),
			clientv3.OpPut(taskKey(fin.ID), string(taskVal)),
		}
		committed, err := s.commitProjection(ctx, "project vm create txn", cmps, ops)
		if err != nil {
			return err
		}
		if committed {
			return nil
		}
	}
	return fmt.Errorf("project vm create: template derived-count CAS exhausted after %d tries", projectTemplateCASRetries)
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
// runtime row, decrements the source template's derived_vm_count, and finalizes
// the delete task - all in one transaction. The derived_vm_count decrement is
// non-idempotent, so when the VM carries a template the projection commits under
// a compare on the template mod-revision (bounded retry); a template-less VM
// commits without a compare. Soft-delete drops the name guard (name reusable)
// and the owner/template/firmware/pinned-node indexes, and each disk's vm + pool
// indexes, so no index scan or blocking-count sees the deleted rows.
func (s *Store) ProjectVMDeleteSuccess(ctx context.Context, vm store.VM, fin store.UpdateTaskFinalizedParams) error {
	now := time.Now().UTC()
	taskVal, err := s.finalizedTaskValue(ctx, fin)
	if err != nil {
		return err
	}
	base, err := s.vmDeleteBaseOps(ctx, vm, now, taskVal, fin.ID)
	if err != nil {
		return err
	}

	for range projectTemplateCASRetries {
		// Worker redelivery: the task is already committed-terminal, so the
		// non-idempotent derived_vm_count decrement has already been committed.
		// Skip re-projecting; the worker will CompleteJob.
		if done, err := s.projectionAlreadyCommitted(ctx, fin.ID); err != nil || done {
			return err
		}

		cmps, ops, err := s.vmDeleteCommitInputs(ctx, vm, base)
		if err != nil {
			return err
		}
		committed, err := s.commitProjection(ctx, "project vm delete txn", cmps, ops)
		if err != nil {
			return err
		}
		if committed {
			return nil
		}
	}
	return fmt.Errorf("project vm delete: template derived-count CAS exhausted after %d tries", projectTemplateCASRetries)
}

// vmDeleteBaseOps builds the soft-delete operations shared by the templated and
// template-less delete paths: the VM row + guard/index removals, the runtime
// delete, the per-disk soft-delete + index removals, and the task finalize.
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

// vmDeleteCommitInputs derives the compare set and the put/delete ops for one
// delete-projection commit attempt over base. A template-less VM and a VM whose
// template is hard-gone commit base unguarded (no derived_vm_count decrement);
// a templated VM appends the decremented template put guarded by a compare on
// the template's mod-revision, so a concurrent template mutation loses the txn
// and the caller retries.
func (s *Store) vmDeleteCommitInputs(ctx context.Context, vm store.VM, base []clientv3.Op) ([]clientv3.Cmp, []clientv3.Op, error) {
	if vm.TemplateID == nil {
		return nil, base, nil
	}
	tmpl, modRev, found, err := s.templateWithRev(ctx, *vm.TemplateID)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		// Template hard-gone (should not happen - templates soft-delete):
		// commit the rest without a decrement rather than wedge the delete.
		return nil, base, nil
	}
	tmpl.DerivedVmCount--
	tmplVal, err := etcd.Marshal(tmpl)
	if err != nil {
		return nil, nil, err
	}
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(templateKey(*vm.TemplateID)), "=", modRev)}
	ops := append(append([]clientv3.Op{}, base...), clientv3.OpPut(templateKey(*vm.TemplateID), string(tmplVal)))
	return cmps, ops, nil
}

// commitProjection commits one projection attempt: a single etcd transaction
// gating ops on cmps (nil cmps = unconditional commit). It reports whether the
// txn succeeded so the caller's CAS loop can retry on a lost compare; label
// names the txn in the wrap of any transport error.
func (s *Store) commitProjection(ctx context.Context, label string, cmps []clientv3.Cmp, ops []clientv3.Op) (bool, error) {
	txn := s.c.Raw().Txn(ctx)
	if len(cmps) > 0 {
		txn = txn.If(cmps...)
	}
	resp, err := txn.Then(ops...).Commit()
	if err != nil {
		return false, fmt.Errorf("%s: %v", label, err)
	}
	return resp.Succeeded, nil
}

// projectionAlreadyCommitted reports whether the task is already
// committed-terminal, meaning a prior delivery committed the projection's
// non-idempotent template write. A true return lets the worker skip
// re-projecting and proceed to CompleteJob.
func (s *Store) projectionAlreadyCommitted(ctx context.Context, taskID uuid.UUID) (bool, error) {
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		return false, err
	}
	return isCommittedTerminal(task.Status), nil
}

// vmIndexDeleteOps returns the secondary-index removals matching vmIndexOps:
// owner, and (when set) template / firmware / pinned-node.
func vmIndexDeleteOps(vm store.VM) []clientv3.Op {
	ops := []clientv3.Op{
		clientv3.OpDelete(etcd.Key("index", "vms", "owner", vm.OwnerID.String(), vm.ID.String())),
	}
	if vm.TemplateID != nil {
		ops = append(ops, clientv3.OpDelete(etcd.Key("index", "vms", "template", vm.TemplateID.String(), vm.ID.String())))
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

// isCommittedTerminal reports whether the projection's non-idempotent write has
// already committed. Only success and cancelled qualify; failed is retryable
// (failRun finalizes failed but the dispatcher requeues the job), so a failed
// task must still re-run.
func isCommittedTerminal(s store.TaskStatus) bool {
	return s == store.TaskStatusSuccess || s == store.TaskStatusCancelled
}

// templateWithRev reads a template row and its current mod-revision, the compare
// target for the derived_vm_count CAS. found is false when the key is absent.
func (s *Store) templateWithRev(ctx context.Context, id uuid.UUID) (store.Template, int64, bool, error) {
	resp, err := s.c.Raw().Get(ctx, templateKey(id))
	if err != nil {
		return store.Template{}, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return store.Template{}, 0, false, nil
	}
	var t store.Template
	if err := json.Unmarshal(resp.Kvs[0].Value, &t); err != nil {
		return store.Template{}, 0, false, fmt.Errorf("unmarshal template %q: %v", id, err)
	}
	return t, resp.Kvs[0].ModRevision, true, nil
}
