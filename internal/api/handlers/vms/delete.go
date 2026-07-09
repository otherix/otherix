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

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// stampIdempotency threads the request's idempotency descriptor (key + body
// hash) and the caller id into params so store.EnqueueTask commits the task
// exactly-once. It is a no-op when the request carried no descriptor (the
// Idempotency-Key middleware only sets one on the actionProceed path) or no
// authenticated principal, leaving the blind-commit path unchanged.
func stampIdempotency(ctx context.Context, params *store.CreateTaskParams) {
	d := middleware.IdempotencyFromContext(ctx)
	if d == nil {
		return
	}
	u := auth.UserFromContext(ctx)
	if u == nil {
		return
	}
	params.IdempotencyUserID = &u.ID
	params.IdempotencyKey = &d.Key
	params.IdempotencyHash = d.Hash
}

// Delete implements DELETE /v1/vms/{id}. Required permission:
// `vm:delete` (admin / operator: any; developer: own; viewer: none).
// Cross-user developer attempts surface as 404 (no leak) per CLAUDE.md
// "403 vs 404" rule. {id} is a VM name; UUID literals are rejected
// with 400 validation_failed at the resolver.
//
// Atomic enqueue: tasks row + job land in one atomic enqueue. The
// vms / vm_disks / vm_runtime rows are NOT
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

	vm, err := resolver.VM(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeResolveError(w, r, err)
		return
	}

	taskID, err := h.runDelete(r.Context(), vm, caller)
	if err != nil {
		// An unscheduled (pending) VM is deleted CP-side without an agent task:
		// runDelete reports it via errDeletedSync and the response is 204.
		if errors.Is(err, errDeletedSync) {
			response.WriteNoContent(w)
			return
		}
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
// caller may not see it (developer scope=own + cross-user). Mapped to
// 404 (no leak), same as a missing row.
var errVMNotVisible = errors.New("vm not visible to caller")

// errDeletedSync is the in-flight signal that an unscheduled (pending) VM was
// deleted CP-side without an agent task. Delete maps it to 204 No Content (there
// is no async task to poll), distinct from the scheduled-VM 202 + vm.delete path.
var errDeletedSync = errors.New("vm deleted synchronously")

// errVMNoNode is the in-flight signal that the vm has no resolvable
// agent endpoint at delete time — vm_disks row missing, pool deleted,
// pinned node missing. Surfaces as 409 node_not_found because the
// async pipeline cannot proceed without a target.
var errVMNoNode = errors.New("vm has no resolvable node")

// runDelete runs the ownership check, resolves the owning node from
// vm.pinned_node_id (or vm_disks → pool fallback), and atomically
// inserts the tasks row + job in one atomic enqueue. Returns
// the task id on success.
func (h *Handler) runDelete(ctx context.Context, vm store.VM, caller *auth.User) (uuid.UUID, error) {
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMDelete); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			return uuid.Nil, errVMNotVisible
		}
		return uuid.Nil, err
	}

	// An unscheduled (pending) VM has no node and no agent task: there is nothing
	// for an agent to tear down. Delete it CP-side and signal a 204. A concurrent
	// bind (the vms.schedule loop) that won the race surfaces as
	// ErrVMNotUnscheduled - fall through to the async agent-delete path below so
	// the now-scheduled VM's agent-side resources are reclaimed.
	if vm.SchedulingStatus == store.VMSchedulingUnscheduled {
		switch err := h.store.DeleteUnscheduledVM(ctx, vm.ID); {
		case err == nil:
			return uuid.Nil, errDeletedSync
		case errors.Is(err, store.ErrVMNotUnscheduled):
			// Raced a bind - fall through to async delete.
		default:
			return uuid.Nil, err
		}
	}

	// Lifecycle precedence: `vm delete` supersedes an in-flight
	// migration. The desired phase (deleted) outranks "I want it on another
	// node", so cancel any active migration before enqueuing the delete.
	// Non-destructive (cancel is fail-safe-to-source pre-cutover, a no-op
	// post-cutover); see cancelActiveMigration.
	h.cancelActiveMigration(ctx, vm.ID, "superseded by vm delete")

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

	params := store.CreateTaskParams{
		ID:           taskID,
		Type:         "vm.delete",
		Status:       store.TaskStatusPending,
		ResourceType: "vm",
		ResourceID:   &resID,
		Args:         argsJSON,
		MaxAttempts:  25,
		CreatedBy:    &createdBy,
	}
	stampIdempotency(ctx, &params)
	return h.store.EnqueueTask(ctx, params, VMDeleteArgs{
		TaskID: taskID,
		VMID:   vm.ID,
		NodeID: nodeID,
	})
}

// resolveNodeForVM walks vm_disks → storage_pools → nodes to find the
// node currently owning this vm's storage. Prefers vms.pinned_node_id
// when set (the create handler always pins); falls back to
// the pool's node otherwise. Returns errVMNoNode when neither path
// yields a node.
func (h *Handler) resolveNodeForVM(ctx context.Context, vm store.VM) (uuid.UUID, error) {
	if vm.PinnedNodeID != nil {
		return *vm.PinnedNodeID, nil
	}
	disks, err := h.store.ListVMDisksByVM(ctx, vm.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if len(disks) == 0 {
		return uuid.Nil, errVMNoNode
	}
	pool, err := h.store.StoragePoolByID(ctx, disks[0].StoragePoolID)
	if err != nil {
		return uuid.Nil, err
	}
	return pool.NodeID, nil
}

// cancelActiveMigration cancels the VM's in-flight (non-terminal) migration, if
// any, with the audit reason - the authoritative lifecycle-precedence step for
// `vm stop` / `vm delete`. It is best-effort and fails SOFT: a lookup
// or cancel error is logged and swallowed so the stop/delete still enqueues. The
// migration worker also guards via redelivery, so a missed cancel here is
// recoverable, never a stuck VM; and cancel itself is non-destructive
// (fail-safe-to-source pre-cutover, a terminal-phase no-op post-cutover), so
// firing it can never trade a recoverable bug for an irreversible one. A
// concurrent terminal transition surfaces as ErrMigrationNotCancelable /
// ErrConcurrentUpdate and is treated as "already done".
func (h *Handler) cancelActiveMigration(ctx context.Context, vmID uuid.UUID, reason string) {
	m, ok, err := h.store.ActiveMigrationForVM(ctx, vmID)
	if err != nil {
		h.log.WarnContext(ctx, "active migration scan failed; proceeding without cancel",
			"vm_id", vmID, "error", err)
		return
	}
	if !ok {
		return
	}
	if _, err := h.store.CancelMigration(ctx, m.ID, reason); err != nil {
		if errors.Is(err, store.ErrMigrationNotCancelable) || errors.Is(err, store.ErrConcurrentUpdate) {
			// Raced a terminal transition - the migration is already done.
			return
		}
		h.log.WarnContext(ctx, "cancel active migration failed; proceeding with lifecycle op",
			"vm_id", vmID, "migration_id", m.ID, "error", err)
		return
	}
	h.log.InfoContext(ctx, "cancelled active migration superseded by lifecycle op",
		"vm_id", vmID, "migration_id", m.ID, "reason", reason)
}

// writeDeleteError maps the in-flight error returned by runDelete to
// the standard envelope.
func writeDeleteError(w http.ResponseWriter, r *http.Request, log interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}, err error,
) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errVMNotVisible):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
	case errors.Is(err, errVMNoNode):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeNodeNotFound, "no node owns this vm's storage", nil)
	case errors.Is(err, store.ErrIdempotencyKeyMismatch):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeIdempotencyMismatch, "idempotency key reused with different request", nil)
	default:
		log.ErrorContext(r.Context(), "vms.delete enqueue failed", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete vm", nil)
	}
}
