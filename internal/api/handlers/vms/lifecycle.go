// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// syncLifecycleClient is the narrow agentclient surface the sync
// lifecycle handlers depend on. *agentclient.Client satisfies it
// structurally; tests substitute a stub by passing a handler
// constructed with the stub through the public Handler factory.
type syncLifecycleClient interface {
	PauseVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (agentclient.AgentVM, error)
	ResumeVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (agentclient.AgentVM, error)
	ResetVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (agentclient.AgentVM, error)
}

// LifecycleDeps is the dependency bundle the api binary supplies to
// the Handler so the sync lifecycle handlers (Pause / Resume /
// Reset) can dispatch to the pinned agent. Kept separate from the
// async-handler deps so test wiring can plug a fakeSyncLifecycle
// client without touching the river-backed create / delete seams.
type LifecycleDeps struct {
	AgentClient syncLifecycleClient
}

// Pause implements POST /v1/vms/{id}/pause — sync per spec. Issues
// QMP `stop` against the pinned agent and returns the refreshed
// `VM` view. Required permission: `vm:lifecycle` (admin / operator:
// any; developer: own; viewer: none). Cross-user developer attempts
// surface as 404 (no leak) per the project's "403 vs 404" rule.
func (h *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	h.runSyncLifecycle(w, r, syncOpPause)
}

// Resume implements POST /v1/vms/{id}/resume — sync per spec.
// Issues QMP `cont`. Same permission contract as Pause.
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	h.runSyncLifecycle(w, r, syncOpResume)
}

// Reset implements POST /v1/vms/{id}/reset — sync per the Pre-L1
// spec amendment. Issues QMP `system_reset`. Same permission
// contract as Pause.
func (h *Handler) Reset(w http.ResponseWriter, r *http.Request) {
	h.runSyncLifecycle(w, r, syncOpReset)
}

// syncOp enumerates the three sync lifecycle actions. Drives both
// the agentclient method dispatch and the post-success phase the
// CP writes to vm_runtime.
type syncOp int

const (
	syncOpPause syncOp = iota
	syncOpResume
	syncOpReset
)

// label returns the action segment used in error envelopes + logs.
func (op syncOp) label() string {
	switch op {
	case syncOpPause:
		return "pause"
	case syncOpResume:
		return "resume"
	case syncOpReset:
		return "reset"
	default:
		return "unknown"
	}
}

// resultPhase returns the vm_runtime.phase value the CP writes after
// a successful action. Reset is a runtime-identity-preserving reboot
// so phase stays running.
func (op syncOp) resultPhase() store.VMPhase {
	switch op {
	case syncOpPause:
		return store.VmPhasePaused
	case syncOpResume:
		return store.VmPhaseRunning
	case syncOpReset:
		return store.VmPhaseRunning
	default:
		return store.VmPhaseRunning
	}
}

// runSyncLifecycle is the shared engine for Pause / Resume / Reset.
// Sequence: resolve VM by name → ownership check → resolve owning
// node → POST to agent → update vm_runtime.phase → re-project the
// VM view from the freshly written runtime row. Returns 200 + VM.
func (h *Handler) runSyncLifecycle(w http.ResponseWriter, r *http.Request, op syncOp) {
	if h.lifecycle.AgentClient == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "sync lifecycle client not configured", nil)
		return
	}

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

	nodeID, err := h.resolveNodeForVM(r.Context(), vm)
	if err != nil {
		writeLifecycleError(w, r, h.log, op, err)
		return
	}
	node, err := h.store.NodeByID(r.Context(), nodeID)
	if err != nil {
		writeLifecycleError(w, r, h.log, op, err)
		return
	}

	idemKey := uuid.NewString()
	if _, err := h.dispatchSyncOp(r.Context(), op, node.AdvertisedEndpoint, vm.Name, idemKey); err != nil {
		writeLifecycleError(w, r, h.log, op, err)
		return
	}

	if err := h.store.UpdateVMRuntimePhase(r.Context(), store.UpdateVMRuntimePhaseParams{
		VmID:  vm.ID,
		Phase: op.resultPhase(),
	}); err != nil {
		h.log.ErrorContext(r.Context(), "vms.lifecycle update runtime phase failed",
			"op", op.label(), "vm_id", vm.ID, "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist runtime phase", nil)
		return
	}

	runtime, disk, err := h.loadVMProjection(r.Context(), vm)
	if err != nil {
		writeVMLoadError(w, r, err)
		return
	}
	activeMig := h.activeMigration(r.Context(), vm.ID)
	names, err := h.resolveViewNames(r.Context(), vm, runtime, disk, callerCanReadUsers(r.Context()), activeMig)
	if err != nil {
		writeVMLoadError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK,
		toView(vm, runtime, names, h.vmDeleting(r.Context(), vm.ID),
			callerCanReadVMSecrets(r.Context(), vm.OwnerID), activeMig))
}

// dispatchSyncOp dispatches to the correct agentclient method based
// on op. Centralised so writeLifecycleError can stay table-driven
// without a per-op switch.
func (h *Handler) dispatchSyncOp(
	ctx context.Context, op syncOp, endpoint, vmName, idemKey string,
) (agentclient.AgentVM, error) {
	switch op {
	case syncOpPause:
		return h.lifecycle.AgentClient.PauseVM(ctx, endpoint, vmName, idemKey)
	case syncOpResume:
		return h.lifecycle.AgentClient.ResumeVM(ctx, endpoint, vmName, idemKey)
	case syncOpReset:
		return h.lifecycle.AgentClient.ResetVM(ctx, endpoint, vmName, idemKey)
	default:
		return agentclient.AgentVM{}, fmt.Errorf("unknown sync op")
	}
}

// writeLifecycleError maps the in-flight error from runSyncLifecycle
// to the standard envelope. Agent 404 surfaces as 404 not_found, 409
// passes through verbatim (invalid-state preconditions), other
// agent codes collapse to 500.
func writeLifecycleError(w http.ResponseWriter, r *http.Request, log interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}, op syncOp, err error,
) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
	case errors.Is(err, errVMNoNode):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeNodeNotFound, "no node owns this vm's storage", nil)
	default:
		var ae *agentclient.AgentError
		if errors.As(err, &ae) {
			switch ae.Status {
			case http.StatusNotFound:
				response.WriteError(w, r, http.StatusNotFound,
					response.CodeVMNotFound, "vm not found on agent", nil)
				return
			case http.StatusConflict:
				response.WriteError(w, r, http.StatusConflict,
					response.CodeConflict, ae.Message, nil)
				return
			}
		}
		log.ErrorContext(r.Context(), "vms.lifecycle agent call failed",
			"op", op.label(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, op.label()+" vm", nil)
	}
}
