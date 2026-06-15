// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/response"
)

// gibBytes is the GiB->bytes multiplier. The VMSpec boot disk advertises
// its virtual size as size_gib (an integer count of GiB); the destination
// disk on this node must be sized in bytes.
const gibBytes = 1 << 30

// StartIncoming handles POST /v1/vms/{vm_name}/migrations/incoming - the
// first step of a migration. The CP contacts this (target) node to
// reserve a port, create the destination disk, and start a TLS NBD
// server pinned to the source identity. Synchronous (200) per the agent
// contract: the source push cannot begin until it has the endpoint +
// token returned here.
//
// The destination disk is sized at max(expected_size_bytes,
// boot-disk-virtual-size). The boot disk's virtual size is the only
// authoritative figure on the wire (VMSpec.Disks[0].size_gib); under-sizing
// the destination makes the source-side `qemu-img convert -n` push fail,
// so a missing boot disk is a hard 400 rather than a silent zero.
func (h *Handler) StartIncoming(w http.ResponseWriter, r *http.Request) {
	var req agentapi.MigrationIncomingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON",
			map[string]any{"err": err.Error()})
		return
	}
	if deref(req.SourceNodeIdentity) == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "source_node_identity is required", nil)
		return
	}
	if len(req.VMSpec.Disks) == 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "vm_spec.disks must carry a boot disk", nil)
		return
	}
	boot := req.VMSpec.Disks[0]

	poolName, ok := h.poolNameForPath(boot.StoragePoolPath)
	if !ok {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"vm_spec boot disk storage_pool_path does not match a configured pool",
			map[string]any{"storage_pool_path": boot.StoragePoolPath})
		return
	}

	res, err := h.manager.StartIncoming(r.Context(), vm.IncomingSpec{
		MigrationID:    req.MigrationID,
		VMUUID:         req.VMSpec.VMUUID,
		VMName:         req.VMSpec.Name,
		VCPUs:          req.VMSpec.CPUCores,
		MemoryMB:       int(req.VMSpec.MemoryMib),
		PoolName:       poolName,
		Architecture:   qemu.Architecture(req.VMSpec.Architecture),
		Mode:           string(req.Mode),
		ExpectedSize:   deref64(req.ExpectedSizeBytes),
		DiskSizeBytes:  int64(boot.SizeGib) * gibBytes,
		Disks:          incomingDisks(req, int64(boot.SizeGib)*gibBytes),
		SourceIdentity: deref(req.SourceNodeIdentity),
		BindHost:       h.migrationHost,
	})
	if err != nil {
		h.log.ErrorContext(r.Context(), "start incoming migration failed",
			"migration_id", req.MigrationID.String(), "vm", req.VMSpec.Name, "error", err)
		mapIncomingError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, agentapi.MigrationIncomingResponse{
		MigrationID:    req.MigrationID,
		ListenEndpoint: res.ListenEndpoint,
		NbdEndpoint:    strPtrOrNil(res.NBDEndpoint),
		AuthToken:      res.AuthToken,
	})
}

// incomingDisks maps the wire disk manifest to the manager's disk list. When
// the CP sends no manifest (legacy or boot-only), it falls back to a single
// boot disk derived from the boot VMSpec disk so the target still replicates
// the boot device.
func incomingDisks(req agentapi.MigrationIncomingRequest, bootSizeBytes int64) []vm.MigrationDisk {
	if req.Disks == nil || len(*req.Disks) == 0 {
		return []vm.MigrationDisk{{
			Index:     0,
			SizeBytes: bootSizeBytes,
			Format:    "qcow2",
			ReadOnly:  false,
		}}
	}
	out := make([]vm.MigrationDisk, 0, len(*req.Disks))
	for _, d := range *req.Disks {
		out = append(out, vm.MigrationDisk{
			Index:     d.Index,
			SizeBytes: d.SizeBytes,
			Format:    string(d.Format),
			ReadOnly:  d.ReadOnly,
		})
	}
	return out
}

// StartOutgoing handles POST /v1/vms/{vm_name}/migrations/outgoing -
// async (202). It resolves the VM by name on this (source) node, then
// kicks off the disk push in the background and returns a task the CP
// polls.
func (h *Handler) StartOutgoing(w http.ResponseWriter, r *http.Request) {
	var req agentapi.MigrationOutgoingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON",
			map[string]any{"err": err.Error()})
		return
	}
	if deref(req.TargetNodeIdentity) == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "target_node_identity is required", nil)
		return
	}

	name := chi.URLParam(r, "vm_name")
	v, err := h.manager.ByName(name)
	if err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "vm not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}

	task, err := h.manager.StartOutgoing(r.Context(), vm.OutgoingSpec{
		MigrationID:    req.MigrationID,
		VMUUID:         v.ID,
		VMName:         v.Name,
		Mode:           string(req.Mode),
		TargetEndpoint: req.TargetEndpoint,
		NBDEndpoint:    deref(req.NbdEndpoint),
		TargetIdentity: deref(req.TargetNodeIdentity),
		AuthToken:      req.AuthToken,
	})
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

// GetMigration handles GET /v1/vms/{vm_name}/migrations/{migration_id} -
// sync (200) status poll. A bad uuid or an unknown migration both surface
// as 404; the agent never leaks "exists but for another VM".
func (h *Handler) GetMigration(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "migration_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "migration not found", nil)
		return
	}
	view, ok := h.manager.GetMigration(id)
	if !ok {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "migration not found", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toMigrationAPI(view))
}

// CancelMigration handles POST /v1/vms/{vm_name}/migrations/{migration_id}/cancel -
// sync (200), best-effort. Returns the current migration view; an unknown
// migration is 404.
func (h *Handler) CancelMigration(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "migration_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "migration not found", nil)
		return
	}
	view, ok := h.manager.CancelMigration(id)
	if !ok {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "migration not found", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toMigrationAPI(view))
}

// poolNameForPath resolves an absolute storage-pool filesystem path (as
// carried on a VMSpec disk) back to the agent-local pool name. The pool
// registry is name-keyed but each entry records its root, so the handler
// reverse-maps via the ListPools snapshot. Returns false when no pool
// roots at the given path.
func (h *Handler) poolNameForPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	for _, p := range h.manager.ListPools() {
		if p.Root == path {
			return p.Name, true
		}
	}
	return "", false
}

// mapIncomingError translates StartIncoming failures to the standard
// envelope. ErrNoFreePort is a retryable capacity condition (every
// migration port in range is in use) and surfaces as 409 conflict; an
// unknown pool (TOCTOU: removed after the handler's path lookup) is a 400
// validation_failed; everything else is an internal failure.
func mapIncomingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, migration.ErrNoFreePort):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "no free migration port", nil)
	case errors.Is(err, vm.ErrPoolUnknown):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool does not match a configured pool", nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
	}
}

// toMigrationAPI projects the manager's MigrationView onto the agent-API
// Migration wire shape.
func toMigrationAPI(v vm.MigrationView) agentapi.Migration {
	return agentapi.Migration{
		MigrationID:      v.MigrationID,
		VMUUID:           v.VMUUID,
		Role:             agentapi.MigrationRole(v.Role),
		Mode:             agentapi.MigrationMode(v.Mode),
		Phase:            agentapi.MigrationPhase(v.Phase),
		ProgressPercent:  v.ProgressPercent,
		BytesTotal:       v.BytesTotal,
		BytesTransferred: v.BytesTransferred,
		PeerEndpoint:     v.PeerEndpoint,
		ErrorMessage:     v.ErrorMessage,
		CreatedAt:        v.CreatedAt,
	}
}

// strPtrOrNil returns a pointer to s, or nil when s is empty, for the
// optional *string wire fields.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// deref returns the pointed-to string or "" when the pointer is nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// deref64 returns the pointed-to int64 or 0 when the pointer is nil.
func deref64(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
