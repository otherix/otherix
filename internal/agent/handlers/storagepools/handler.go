// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package storagepools hosts the agent's /v1/storage-pools/* HTTP
// handlers. The Control Plane dials these endpoints over mTLS to learn
// the live capacity / available bytes of every pool the agent serves.
//
// The URL parameter is `{pool_name}` - the agent's pool registry is
// name-keyed; the CP-side per-instance UUID stays at the operator API
// edge.
package storagepools

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// Handler bundles the VM manager (which owns the pool registry +
// TaskStore) and the logger. The manager owns all state; the handler
// only translates wire shapes.
type Handler struct {
	manager *vm.Manager
	log     *slog.Logger
}

// New constructs a Handler.
func New(m *vm.Manager, log *slog.Logger) *Handler {
	return &Handler{manager: m, log: log}
}

// Mount registers /v1/storage-pools routes on r.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/{pool_name}/scan", h.Scan)
	r.Get("/{pool_name}/images", h.ListImages)
	r.Post("/{pool_name}/images", h.ImportImage)
	r.Delete("/{pool_name}/images/{checksum}", h.DeleteImage)
}

// asyncAccepted mirrors `AsyncTaskAccepted` in api/openapi/agent.yaml.
// Duplicated from handlers/vms/create.go pending a shared types
// package; status enum is constrained to [pending, running] by spec.
type asyncAccepted struct {
	TaskID string         `json:"task_id"`
	Status string         `json:"status"`
	Links  map[string]any `json:"links"`
}

// Scan handles POST /v1/storage-pools/{pool_name}/scan.
//
//   - Validates the pool_name URL parameter.
//   - Asks the manager to begin a scan; receives an agent task in
//     `pending` status.
//   - Returns 202 with the task_id immediately; the CP polls
//     /v1/tasks/{task_id} to observe terminal status.
//
// Errors:
//   - 400 validation_failed — pool_name is empty.
//   - 404 not_found — pool_name does not match a configured pool.
//   - 500 internal — unexpected manager failure.
func (h *Handler) Scan(w http.ResponseWriter, r *http.Request) {
	poolName := chi.URLParam(r, "pool_name")
	if poolName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool_name is required", nil)
		return
	}

	task, err := h.manager.ScanPool(r.Context(), poolName)
	if err != nil {
		if errors.Is(err, vm.ErrPoolUnknown) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "pool does not match a configured pool", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "scan pool failed",
			"pool_name", poolName, "err", err)
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
