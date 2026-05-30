// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// asyncOp enumerates the four L2 async lifecycle actions. Drives
// agent dispatch, river job-args kind, and the success-path
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
// `stop_timeout` (Area 4-II lock — no internal escalation).
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpStop)
}

// Poweroff implements POST /v1/vms/{id}/poweroff — async hard
// shutdown. Sets vms.desired_phase to 'stopped' on success.
func (h *Handler) Poweroff(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpPoweroff)
}

// Reboot implements POST /v1/vms/{id}/reboot — async stop+start
// cycle (Area 4-III lock — distinct from Reset; the agent's QEMU PID
// changes). desired_phase stays 'running'.
func (h *Handler) Reboot(w http.ResponseWriter, r *http.Request) {
	h.runAsyncLifecycle(w, r, asyncOpReboot)
}

// runAsyncLifecycle is the shared engine for Start / Stop /
// Poweroff / Reboot. Sequence: resolve VM → ownership check →
// resolve owning node → atomic insert (tasks + river job +
// UpdateTaskRiverJobID) inside InTx → 202 + AsyncTaskAccepted.
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
// inserts the tasks row + river job + agent task id pointer inside
// one transaction. Returns the freshly-minted task id on success.
// Shared by all four async lifecycle ops — the only difference is
// the river args type, dispatched by jobArgsFor.
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

	err = h.store.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		if _, err := q.CreateTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm." + op.label(),
			Status:       store.TaskStatusPending,
			ResourceType: "vm",
			ResourceID:   &resID,
			Args:         argsJSON,
			MaxAttempts:  25,
			CreatedBy:    &createdBy,
		}); err != nil {
			return err
		}
		insertResult, err := h.insertRiverJobForOp(ctx, tx, op, taskID, vm.ID, nodeID)
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

// insertRiverJobForOp dispatches to the matching river args type for
// op. Cannot be done via a map[op]args because each Args is a
// distinct type — river.InsertTx is generic over the args type.
func (h *Handler) insertRiverJobForOp(
	ctx context.Context, tx pgx.Tx, op asyncOp, taskID, vmID, nodeID uuid.UUID,
) (*rivertype.JobInsertResult, error) {
	switch op {
	case asyncOpStart:
		return h.riverClient.InsertTx(ctx, tx, VMStartArgs{
			TaskID: taskID, VMID: vmID, NodeID: nodeID,
		}, nil)
	case asyncOpStop:
		return h.riverClient.InsertTx(ctx, tx, VMStopArgs{
			TaskID: taskID, VMID: vmID, NodeID: nodeID,
		}, nil)
	case asyncOpPoweroff:
		return h.riverClient.InsertTx(ctx, tx, VMPoweroffArgs{
			TaskID: taskID, VMID: vmID, NodeID: nodeID,
		}, nil)
	case asyncOpReboot:
		return h.riverClient.InsertTx(ctx, tx, VMRebootArgs{
			TaskID: taskID, VMID: vmID, NodeID: nodeID,
		}, nil)
	default:
		return nil, fmt.Errorf("unknown async op")
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
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errVMNotVisible):
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
