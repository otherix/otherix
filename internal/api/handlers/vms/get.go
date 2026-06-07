// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/vms/{id}. Required permission: `vm:read`
// (held at scope=any by every authenticated role per the matrix; no
// ownership check). Status is projected from the (vms, vm_runtime)
// pair via projectStatus — the wire `status` field is computed, never
// stored.
//
// {id} is a case-insensitive VM name; UUID literals are rejected
// with 400 validation_failed at the resolver.
//
// Soft-deleted VMs surface as 404; the wire never exposes the
// deleted-at timestamp directly. (A future ?include_deleted listing
// would change this carve-out.)
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	vm, err := resolver.VM(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeResolveError(w, r, err)
		return
	}

	runtime, disk, err := h.loadVMProjection(r.Context(), vm)
	if err != nil {
		writeVMLoadError(w, r, err)
		return
	}
	names, err := h.resolveViewNames(r.Context(), vm, runtime, disk, callerCanReadUsers(r.Context()))
	if err != nil {
		writeVMLoadError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(vm, runtime, names))
}

// loadVMProjection fetches the vm_runtime row (if any) and the first
// vm_disks row. vm_runtime is allowed to be missing (worker has not
// upserted yet — projectStatus collapses that case to "creating").
//
// vm_disks is expected to be present once the create-handler bind
// committed; an unscheduled (pending) VM, however, has no disk row yet,
// so an absent disk on an unscheduled VM is the zero VMDisk (toView
// falls back to the SchedulingSpec for the display pool). An absent disk
// on an already-scheduled VM indicates a row created out-of-band and
// surfaces as 500, since the API never produces that state.
func (h *Handler) loadVMProjection(ctx context.Context, vm store.VM) (*store.VMRuntime, store.VMDisk, error) {
	var runtime *store.VMRuntime
	rt, err := h.store.VMRuntimeByID(ctx, vm.ID)
	switch {
	case err == nil:
		runtime = &rt
	case errors.Is(err, store.ErrNotFound):
		// runtime stays nil — projectStatus → "creating".
	default:
		return nil, store.VMDisk{}, err
	}
	disks, err := h.store.ListVMDisksByVM(ctx, vm.ID)
	if err != nil {
		return nil, store.VMDisk{}, err
	}
	if len(disks) == 0 {
		if vm.SchedulingStatus == store.VMSchedulingUnscheduled {
			return runtime, store.VMDisk{}, nil
		}
		return nil, store.VMDisk{}, errVMDiskMissing
	}
	return runtime, disks[0], nil
}

// errVMDiskMissing surfaces when a vms row exists without a paired
// vm_disks row. The CP never produces that state — it would only arise
// from a manual DB mutation — so the handler treats it as 500 internal.
var errVMDiskMissing = errors.New("vm has no disks")

// writeResolveError maps a resolver.Error (from Get / Delete) to the
// VM 404 / UUID-rejection / 500 envelopes. VM identifiers are
// name-only - UUID literals in the path return 400 validation_failed.
func writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	if resolver.IsUUIDInName(err) {
		response.WriteUUIDNotAllowedError(w, r, "vm", "id")
		return
	}
	if resolver.IsNotFound(err) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "load vm", nil)
}

// writeVMLoadError maps load errors to wire envelopes. Missing rows
// surface as 404; missing disks (non-API state) as 500 with the cause
// suppressed from the wire.
func writeVMLoadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
	case errors.Is(err, errVMDiskMissing):
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "vm projection inconsistent", nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load vm", nil)
	}
}
