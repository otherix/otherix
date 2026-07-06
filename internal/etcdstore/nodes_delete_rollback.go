// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// unionVMIDsForNode returns the set of VM ids that make a node non-empty for
// delete gating and force-evacuation: the UNION of the OBSERVED set (vm_runtime
// rows homed here, via the by-node index) and the DECLARED set (VMs pinned here,
// via ListVMRefsForNodeDeclared - which crucially includes a pinned-but-unobserved
// VM the agent never reported). Deduped by id. Counting only the observed side
// (the pre-fix gate) let a non-force delete succeed while a committed bind's state
// still referenced the node, leaking the boot disk + NIC indexes.
func (s *Store) unionVMIDsForNode(ctx context.Context, nodeID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	set := make(map[uuid.UUID]struct{})
	items, err := s.c.Range(ctx, vmRuntimeNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, err
	}
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt vm_runtime node index %q: %v", kv.Key, perr)
		}
		set[id] = struct{}{}
	}
	refs, err := s.ListVMRefsForNodeDeclared(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		set[r.ID] = struct{}{}
	}
	return set, nil
}

// evacuateNodeVMs empties a node of its VMs during a force-delete, routing each
// distinct VM in the union by RUNTIME EXISTENCE (not by which index surfaced it):
//
//   - a VM the agent has reported (vm_runtime row present) is ORPHANED
//     (orphanOneVMRuntime: phase=orphaned, current_node_id cleared) and KEEPS its
//     boot disk - a running VM's disk must never be destroyed;
//   - a pinned-but-unobserved VM (no runtime row) is ROLLED BACK - the whole bind
//     is undone and the VM returns to unscheduled, so nothing leaks.
//
// A rollback that discovers a runtime row appeared mid-flight re-routes to orphan.
// Each per-VM step is CAS-guarded and fails toward inaction; on CAS exhaustion the
// error aborts the whole delete before the node soft-delete (the operator retries).
func (s *Store) evacuateNodeVMs(ctx context.Context, nodeID uuid.UUID, union map[uuid.UUID]struct{}) (orphaned, rolledBack int64, err error) {
	for vmID := range union {
		hasRT, herr := s.vmRuntimeExists(ctx, vmID)
		if herr != nil {
			return orphaned, rolledBack, herr
		}
		if hasRT {
			ok, oerr := s.orphanOneVMRuntime(ctx, vmID, nodeID, vmRuntimeNodeIndexKey(nodeID, vmID))
			if oerr != nil {
				return orphaned, rolledBack, oerr
			}
			if ok {
				orphaned++
			}
			continue
		}
		rb, orphanInstead, rerr := s.rollbackOnePinnedVM(ctx, vmID, nodeID)
		if rerr != nil {
			return orphaned, rolledBack, rerr
		}
		if orphanInstead {
			ok, oerr := s.orphanOneVMRuntime(ctx, vmID, nodeID, vmRuntimeNodeIndexKey(nodeID, vmID))
			if oerr != nil {
				return orphaned, rolledBack, oerr
			}
			if ok {
				orphaned++
			}
			continue
		}
		if rb {
			rolledBack++
		}
	}
	return orphaned, rolledBack, nil
}

// vmRuntimeExists reports whether a vm_runtime primary row is present.
func (s *Store) vmRuntimeExists(ctx context.Context, vmID uuid.UUID) (bool, error) {
	resp, err := s.c.Raw().Get(ctx, vmRuntimeKey(vmID), clientv3.WithCountOnly())
	if err != nil {
		return false, err
	}
	return resp.Count > 0, nil
}

// rollbackOnePinnedVM undoes a committed-but-unobserved bind of a VM pinned to
// nodeID, returning it to unscheduled. It is the inverse of buildBindTxn: it tears
// down the boot disk (+ vm/pool indexes), the NIC (+ per-VM/per-network index, MAC
// guard, IPv4 reservation), cancels the pending create task and deletes its job,
// clears the pin, and re-adds the unscheduled index - all in ONE Txn under the VM
// row's ModRevision CAS, a "no runtime appeared" guard, and (when present) the
// create job's ModRevision CAS. desired_phase is untouched: the user still wants
// the VM, which the scheduler re-binds elsewhere (never back here - the node
// delete-intent blocks a re-bind onto this node).
//
// Returns orphanInstead=true (caller orphans) when a runtime row appeared since
// the routing read - a create landed, the VM is now running, and it must NOT be
// rolled back (its disk would be destroyed). Returns (false,false,nil) when the VM
// is gone or its pin already moved off nodeID (a concurrent re-bind won). Aborts
// with an error when the create job is not cleanly pending (the dispatcher claimed
// it) or on CAS exhaustion - fail toward inaction, so an in-flight create is never
// rewound out from under.
func (s *Store) rollbackOnePinnedVM(ctx context.Context, vmID, nodeID uuid.UUID) (rolledBack, orphanInstead bool, err error) {
	for attempt := 0; ; attempt++ {
		vmResp, gerr := s.c.Raw().Get(ctx, vmKey(vmID))
		if gerr != nil {
			return false, false, gerr
		}
		if len(vmResp.Kvs) == 0 {
			return false, false, nil // VM gone: nothing to roll back.
		}
		rev := vmResp.Kvs[0].ModRevision
		var vm store.VM
		if uerr := json.Unmarshal(vmResp.Kvs[0].Value, &vm); uerr != nil {
			return false, false, uerr
		}
		if vm.PinnedNodeID == nil || *vm.PinnedNodeID != nodeID {
			return false, false, nil // moved off this node: a re-bind won.
		}
		// A runtime row appeared since routing: the create landed. Do not roll back a
		// now-running VM; the caller orphans it instead.
		hasRT, herr := s.vmRuntimeExists(ctx, vmID)
		if herr != nil {
			return false, false, herr
		}
		if hasRT {
			return false, true, nil
		}

		now := time.Now().UTC()
		ops, conds, berr := s.buildRollbackOps(ctx, &vm, nodeID, rev, now)
		if berr != nil {
			return false, false, berr
		}

		txResp, terr := s.c.Raw().Txn(ctx).If(conds...).Then(ops...).Commit()
		if terr != nil {
			return false, false, fmt.Errorf("rollback vm %s txn: %v", vmID, terr)
		}
		if txResp.Succeeded {
			return true, false, nil
		}
		if attempt+1 >= nodeDeleteCASAttempts {
			return false, false, fmt.Errorf("rollback vm %s for node delete: %w", vmID, store.ErrConcurrentUpdate)
		}
		// Lost the CAS (VM row changed, a runtime appeared, or the job was claimed):
		// re-read and re-decide on the next iteration.
	}
}

// buildRollbackOps assembles the inverse-of-bind op list + guard compares for a
// single pinned-but-unobserved VM. See rollbackOnePinnedVM.
func (s *Store) buildRollbackOps(ctx context.Context, vm *store.VM, nodeID uuid.UUID, rev int64, now time.Time) ([]clientv3.Op, []clientv3.Cmp, error) {
	var ops []clientv3.Op

	// Boot disk(s): soft-delete the row and drop BOTH the vm-index and pool-index
	// (the inverse of vmDiskIndexOps), so no storage-pool-delete blocker and no
	// stale disk index leak. Mirrors the VM-delete projection's disk teardown.
	disks, err := s.disksOfVM(ctx, vm.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range disks {
		d.DeletedAt = &now
		d.UpdatedAt = now
		dVal, merr := etcd.Marshal(d)
		if merr != nil {
			return nil, nil, merr
		}
		ops = append(ops,
			clientv3.OpPut(vmDiskKey(d.ID), string(dVal)),
			clientv3.OpDelete(etcd.Key("index", "vm_disks", "vm", d.VmID.String(), d.ID.String())),
			clientv3.OpDelete(etcd.Key("index", "vm_disks", "pool", d.StoragePoolID.String(), d.ID.String())),
		)
	}

	// NIC(s): drop the row + per-VM/per-network index + MAC guard + IPv4 reservation
	// (vmNicDeleteOps), so the network-delete block (countVMNicsOnNetwork) clears and
	// neither the MAC guard nor the CP-IPAM reservation leaks. This is the
	// load-bearing teardown: a leaked per-network index wedges the network delete
	// forever and the reservation leaks an address monotonically.
	nicOps, err := s.vmNicDeleteOps(ctx, vm.ID, now)
	if err != nil {
		return nil, nil, err
	}
	ops = append(ops, nicOps...)

	conds := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(vmKey(vm.ID)), "=", rev),
		// The routing read saw no runtime row; guard that it stayed absent, so a
		// create landing mid-rollback loses the CAS and we re-route to orphan rather
		// than tear down a now-running VM's disk.
		clientv3.Compare(clientv3.CreateRevision(vmRuntimeKey(vm.ID)), "=", 0),
	}

	// Neutralize the ACTIVE (non-terminal) create so it never executes against the
	// dead node and does not double-create on re-bind. The routing already saw no
	// runtime row, but a create can be MID-EXECUTION on the agent with no runtime row
	// yet (the worker passed UpdateTaskRunning -> task=running, then sits in
	// exec.Execute; ProjectVMCreateSuccess writes the runtime row only afterward). A
	// pending-only lookup would miss that state and roll a running VM back, tearing
	// down its disk and stranding a runtime row on the deleted node. So we abort the
	// whole force-delete (fail toward inaction) whenever the create is executing -
	// task=running OR its job claimed=running - and neutralize only a create that is
	// provably not in flight (pending, or failed awaiting retry): cancel the task and
	// delete its job in this Txn, guarded on the job's ModRevision so a dispatcher
	// claim between our read and commit loses the CAS (we retry and re-check).
	task, found, err := s.activeCreateTaskForVM(ctx, vm.ID)
	if err != nil {
		return nil, nil, err
	}
	if found {
		if task.Status == store.TaskStatusRunning {
			return nil, nil, fmt.Errorf("rollback vm %s: create task %s is running: %w", vm.ID, task.ID, store.ErrConcurrentUpdate)
		}
		task.Status = store.TaskStatusCancelled
		task.FinishedAt = &now
		tVal, merr := etcd.Marshal(task)
		if merr != nil {
			return nil, nil, merr
		}
		ops = append(ops, clientv3.OpPut(taskKey(task.ID), string(tVal)))
		if task.JobID != nil {
			job, jobRev, jobFound, jerr := s.jobWithRev(ctx, *task.JobID)
			if jerr != nil {
				return nil, nil, jerr
			}
			if jobFound {
				if job.State == JobStateRunning {
					return nil, nil, fmt.Errorf("rollback vm %s: create job %d is running: %w", vm.ID, *task.JobID, store.ErrConcurrentUpdate)
				}
				ops = append(ops, clientv3.OpDelete(jobKey(*task.JobID)))
				conds = append(conds, clientv3.Compare(clientv3.ModRevision(jobKey(*task.JobID)), "=", jobRev))
			}
		}
	}

	// Return the VM to unscheduled (mirrors the shape CreateUnscheduledVM lands):
	// clear the pin, restore the pending-schedule reason, drop the pinned index,
	// re-add the unscheduled index. desired_phase is deliberately untouched.
	vm.SchedulingStatus = store.VMSchedulingUnscheduled
	reason := store.SchedReasonPendingSchedule
	vm.SchedulingReason = &reason
	vm.SchedulingMessage = nil
	vm.SchedulingDetails = nil
	vm.PinnedNodeID = nil
	vm.UpdatedAt = now
	vmVal, err := etcd.Marshal(*vm)
	if err != nil {
		return nil, nil, err
	}
	ops = append(ops,
		clientv3.OpPut(vmKey(vm.ID), string(vmVal)),
		clientv3.OpDelete(etcd.Key("index", "vms", "pinned_node", nodeID.String(), vm.ID.String())),
		clientv3.OpPut(vmUnscheduledIndexKey(vm.ID), vm.ID.String()),
	)
	return ops, conds, nil
}

// activeCreateTaskForVM returns the ACTIVE (non-committed-terminal: pending,
// running, or failed-awaiting-retry) vm.create task for the VM, found by a bounded
// task-prefix scan. A VM has at most one active create task at a time: a bind
// enqueues one, and a prior rolled-back bind's task is cancelled (committed
// terminal, excluded here); success is excluded too (a succeeded create has a
// runtime row, so the VM routes to orphan, not rollback). Returning the running /
// failed states - not just pending - is load-bearing: it lets the rollback detect
// a create executing on the agent and abort rather than tear it down.
func (s *Store) activeCreateTaskForVM(ctx context.Context, vmID uuid.UUID) (store.Task, bool, error) {
	items, err := s.c.Range(ctx, taskPrefix())
	if err != nil {
		return store.Task{}, false, err
	}
	for _, kv := range items {
		var t store.Task
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &t, "task") {
			continue
		}
		if t.Type != "vm.create" || isCommittedTerminal(t.Status) {
			continue
		}
		if t.ResourceID == nil || *t.ResourceID != vmID {
			continue
		}
		return t, true, nil
	}
	return store.Task{}, false, nil
}

// commitCascadeWithNodeIntent commits the node force-delete cascade, making the
// node soft-delete conditional on the delete-intent still carrying this attempt's
// rev. The cascade's HEAD (WireGuard purge, cert-revoke, reap ops - all idempotent
// puts/deletes) commits unconditionally in <=maxTxnOps chunks; the TAIL (the
// node-name-guard delete + node put, which nodeDeleteCascade always appends as the
// final two ops) commits in one txn guarded on deleteIntentGuard, plus the intent
// delete. If a reaper or a racing second delete severed the intent, the tail CAS
// loses and the node is NOT soft-deleted (ErrConcurrentUpdate); the head chunks and
// the per-VM evacuations are safe to re-run on retry.
func (s *Store) commitCascadeWithNodeIntent(ctx context.Context, ops []clientv3.Op, intentKey string, myRev int64) error {
	if len(ops) < 2 {
		return fmt.Errorf("node delete cascade too short (%d ops); want >=2 (name-guard delete + node put)", len(ops))
	}
	head := ops[:len(ops)-2]
	tail := ops[len(ops)-2:] // [nodeNameGuard delete, node put], by nodeDeleteCascade construction.
	if err := s.commitInChunks(ctx, head); err != nil {
		return err
	}
	finalOps := make([]clientv3.Op, 0, len(tail)+1)
	finalOps = append(finalOps, tail...)
	finalOps = append(finalOps, clientv3.OpDelete(intentKey))
	resp, err := s.c.Raw().Txn(ctx).
		If(deleteIntentGuard(intentKey, myRev)).
		Then(finalOps...).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return store.ErrConcurrentUpdate
	}
	return nil
}
