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
// Run-form workers call in place of the pgx InTx blocks. Where a write is
// non-idempotent (the template derived_vm_count bump) the projection commits
// under a compare-on-mod-revision retry loop; the idempotent runtime and task
// writes ride in the same transaction so a success is all-or-nothing.

// projectTemplateCASRetries bounds the compare-and-set loop that protects the
// non-idempotent derived_vm_count bump against a concurrent template mutation.
const projectTemplateCASRetries = 64

// ProjectVMCreateSuccess upserts the VM's runtime row (running), increments the
// source template's derived_vm_count, and finalizes the create task - all in one
// transaction. The derived_vm_count bump is non-idempotent, so the whole
// projection commits under a compare on the template's mod-revision (bounded
// retry); the runtime and task writes are idempotent blind puts. Mirrors the
// pgx VMCreateWorker.projectCreateSuccess InTx.
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
		resp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(templateKey(templateID)), "=", modRev)).
			Then(
				clientv3.OpPut(vmRuntimeKey(rt.VmID), string(runtimeVal)),
				clientv3.OpPut(templateKey(templateID), string(tmplVal)),
				clientv3.OpPut(taskKey(fin.ID), string(taskVal)),
			).Commit()
		if err != nil {
			return fmt.Errorf("project vm create txn: %v", err)
		}
		if resp.Succeeded {
			return nil
		}
	}
	return fmt.Errorf("project vm create: template derived-count CAS exhausted after %d tries", projectTemplateCASRetries)
}

// ProjectVMLifecycleSuccess writes the VM's desired_phase (when non-empty), its
// observed runtime phase, and finalizes the lifecycle task - all in one
// transaction. Every write here is idempotent, so no compare is needed; the
// transaction only groups them so a poll never sees a half-applied terminal.
// Mirrors the pgx vmLifecycleAsyncWorker.projectSuccess InTx. An empty
// desiredPhase skips the vms write (reboot leaves user intent unchanged).
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
