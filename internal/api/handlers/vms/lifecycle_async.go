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
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// asyncOp enumerates the four async lifecycle actions. Drives
// agent dispatch, the job-args kind, and the success-path
// vms.desired_phase write.
type asyncOp int

const (
	asyncOpStart asyncOp = iota
	asyncOpStop
	asyncOpPoweroff
	asyncOpReboot
)

// label returns the action segment used in error envelopes + logs.
func (op asyncOp) label() string {
	switch op {
	case asyncOpStart:
		return "start"
	case asyncOpStop:
		return "stop"
	case asyncOpPoweroff:
		return "poweroff"
	case asyncOpReboot:
		return "reboot"
	default:
		return "unknown"
	}
}

// Start implements POST /v1/vms/{id}/start — async per spec.
// Returns 202 + AsyncTaskAccepted. Sets vms.desired_phase to
// 'running' on success (handled by the worker, not the handler, so
// a worker failure rolls the write back with the rest of the
// transaction). Required permission: `vm:lifecycle` (admin /
// operator: any; developer: own; viewer: none). Cross-user
// developer attempts surface as 404 (no leak).
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpStart)
}

// Stop implements POST /v1/vms/{id}/stop — async graceful ACPI
// shutdown. On agent-side timeout the task completes as failed with
// `stop_timeout` (no internal escalation).
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpStop)
}

// Poweroff implements POST /v1/vms/{id}/poweroff — async hard
// shutdown. Sets vms.desired_phase to 'stopped' on success.
func (h *Handler) Poweroff(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpPoweroff)
}

// Reboot implements POST /v1/vms/{id}/reboot — async stop+start
// cycle (distinct from Reset; the agent's QEMU PID
// changes). desired_phase stays 'running'.
func (h *Handler) Reboot(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpReboot)
}

// runAsyncLifecycle is the shared engine for Start / Stop /
// Poweroff / Reboot. Sequence: resolve VM → ownership check →
// resolve owning node → atomic enqueue (tasks row + job) → 202 +
// AsyncTaskAccepted.
// Mirrors the Delete handler's wire shape verbatim.
func (h *Handler) runAsyncLifecycle(w http.ResponseWriter, r *http.Request, op asyncOp) {
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
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMLifecycle); err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}

	// Lifecycle precedence: `vm stop` supersedes an in-flight
	// migration. The desired phase (stopped) outranks "I want it on another
	// node", so cancel any active migration before enqueuing the stop. Cancel
	// is non-destructive - pre-cutover the VM never left its source, post-cutover
	// cancel is a terminal-phase no-op - so this only ever fails safe. Only stop
	// (and delete, handled in runDelete) trigger this; the other lifecycle ops
	// (start/poweroff/reboot) leave a migration running.
	if op == asyncOpStop {
		h.cancelActiveMigration(r.Context(), vm.ID, "superseded by vm stop")
	}

	taskID, err := h.runAsyncLifecycleEnqueue(r.Context(), op, vm, caller)
	if err != nil {
		writeAsyncLifecycleError(w, r, h.log, op, err)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// runAsyncLifecycleEnqueue resolves the owning node and atomically
// inserts the tasks row + job + agent task id pointer in one atomic
// enqueue. Returns the freshly-minted task id on success.
// Shared by all four async lifecycle ops — the only difference is
// the job-args type, dispatched by jobArgsFor.
func (h *Handler) runAsyncLifecycleEnqueue(ctx context.Context, op asyncOp, vm store.VM, caller *auth.User) (uuid.UUID, error) {
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

	jobArgs, err := jobArgsForOp(op, taskID, vm.ID, nodeID)
	if err != nil {
		return uuid.Nil, err
	}

	return h.store.EnqueueTask(ctx, store.CreateTaskParams{
		ID:           taskID,
		Type:         "vm." + op.label(),
		Status:       store.TaskStatusPending,
		ResourceType: "vm",
		ResourceID:   &resID,
		Args:         argsJSON,
		MaxAttempts:  25,
		CreatedBy:    &createdBy,
	}, jobArgs)
}

// jobArgsForOp returns the matching job args type for op. Cannot be
// done via a map[op]args because each Args is a distinct type; the
// store enqueues whatever queue.JobArgs it receives.
func jobArgsForOp(op asyncOp, taskID, vmID, nodeID uuid.UUID) (queue.JobArgs, error) {
	switch op {
	case asyncOpStart:
		return VMStartArgs{TaskID: taskID, VMID: vmID, NodeID: nodeID}, nil
	case asyncOpStop:
		return VMStopArgs{TaskID: taskID, VMID: vmID, NodeID: nodeID}, nil
	case asyncOpPoweroff:
		return VMPoweroffArgs{TaskID: taskID, VMID: vmID, NodeID: nodeID}, nil
	case asyncOpReboot:
		return VMRebootArgs{TaskID: taskID, VMID: vmID, NodeID: nodeID}, nil
	default:
		return nil, errors.New("unknown async op")
	}
}

// writeAsyncLifecycleError maps the in-flight error from
// runAsyncLifecycleEnqueue to the standard envelope. Same template as
// writeDeleteError: errVMNotVisible → 404 (no leak), errVMNoNode →
// 409, default → 500. Op label is rendered into the log line for
// audit correlation.
func writeAsyncLifecycleError(w http.ResponseWriter, r *http.Request, log interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}, op asyncOp, err error,
) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errVMNotVisible):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
	case errors.Is(err, errVMNoNode):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeNodeNotFound, "no node owns this vm's storage", nil)
	default:
		log.ErrorContext(r.Context(), "vms.lifecycle async enqueue failed",
			"op", op.label(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, op.label()+" vm", nil)
	}
}
