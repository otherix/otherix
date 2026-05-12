// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/vms/{id}. Required permission:
// `vm:delete` (admin / operator: any; developer: own; viewer: none).
// Cross-user developer attempts surface as 404 (no leak) per CLAUDE.md
// "403 vs 404" rule. {id} is a VM name; UUID literals are rejected
// with 400 validation_failed at the resolver.
//
// Atomic enqueue: tasks row + river job + UpdateTaskRiverJobID land
// in one transaction. The vms / vm_disks / vm_runtime rows are NOT
// touched at handler time — the worker drives the soft-delete chain
// inside its own InTx after the agent confirms teardown. Rationale:
// a partial failure between handler-side soft-delete and worker
// completion would leave the row marked deleted but qemu still
// running on the agent, observable to operators as a phantom VM.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vm, err := resolver.VM(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writeResolveError(w, r, err)
		return
	}

	taskID, err := h.runDelete(r.Context(), vm, caller)
	if err != nil {
		writeDeleteError(w, r, h.log, err)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// errVMNotVisible is the in-flight signal that the row exists but the
// caller may not see it (developer scope=own + cross-user). Mapped к
// 404 (no leak), same as a missing row.
var errVMNotVisible = errors.New("vm not visible to caller")

// errVMNoNode is the in-flight signal that the vm has no resolvable
// agent endpoint at delete time — vm_disks row missing, pool deleted,
// pinned node missing. Surfaces as 409 node_not_found так как the
// async pipeline cannot proceed without a target.
var errVMNoNode = errors.New("vm has no resolvable node")

// runDelete runs the ownership check, resolves the owning node from
// vm.pinned_node_id (or vm_disks → pool fallback), and atomically
// inserts the tasks row + river job inside one transaction. Returns
// the task id on success.
func (h *Handler) runDelete(ctx context.Context, vm store.Vm, caller *auth.User) (uuid.UUID, error) {
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMDelete); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			return uuid.Nil, errVMNotVisible
		}
		return uuid.Nil, err
	}

	nodeID, err := h.resolveNodeForVM(ctx, vm)
	if err != nil {
		return uuid.Nil, err
	}

	taskID := uuid.New()
	resID := vm.ID
	createdBy := caller.ID
	argsJSON, err := json.Marshal(map[string]any{
		"vm_id":   vm.ID.String(),
		"node_id": nodeID.String(),
	})
	if err != nil {
		return uuid.Nil, err
	}

	err = h.store.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		if _, err := q.CreateTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.delete",
			Status:       store.TaskStatusPending,
			ResourceType: "vm",
			ResourceID:   &resID,
			Args:         argsJSON,
			MaxAttempts:  25,
			CreatedBy:    &createdBy,
		}); err != nil {
			return err
		}
		insertResult, err := h.riverClient.InsertTx(ctx, tx, VMDeleteArgs{
			TaskID: taskID,
			VMID:   vm.ID,
			NodeID: nodeID,
		}, nil)
		if err != nil {
			return err
		}
		jobID := insertResult.Job.ID
		return q.UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
			ID:         taskID,
			RiverJobID: &jobID,
		})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return taskID, nil
}

// resolveNodeForVM walks vm_disks → storage_pools → nodes to find the
// node currently owning this vm's storage. Prefers vms.pinned_node_id
// when set (the create handler always pins for MVP); falls back to
// the pool's node otherwise. Returns errVMNoNode when neither path
// yields a node.
func (h *Handler) resolveNodeForVM(ctx context.Context, vm store.Vm) (uuid.UUID, error) {
	if vm.PinnedNodeID != nil {
		return *vm.PinnedNodeID, nil
	}
	disks, err := h.store.Queries().ListVMDisksByVM(ctx, vm.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if len(disks) == 0 {
		return uuid.Nil, errVMNoNode
	}
	pool, err := h.store.Queries().GetStoragePoolByID(ctx, disks[0].StoragePoolID)
	if err != nil {
		return uuid.Nil, err
	}
	return pool.NodeID, nil
}

// writeDeleteError maps the in-flight error returned by runDelete к
// the standard envelope.
func writeDeleteError(w http.ResponseWriter, r *http.Request, log interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}, err error,
) {
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errVMNotVisible):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
	case errors.Is(err, errVMNoNode):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeNodeNotFound, "no node owns this vm's storage", nil)
	default:
		log.ErrorContext(r.Context(), "vms.delete enqueue failed", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete vm", nil)
	}
}
