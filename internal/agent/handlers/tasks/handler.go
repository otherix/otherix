// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package tasks hosts the agent's GET /v1/tasks/{task_id} handler.
// Agent task IDs are minted by the VM manager when an async operation
// (vm.create, vm.delete) starts; the surface here is read-only.
package tasks

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// Handler bundles the manager (which owns the TaskStore) and a logger.
type Handler struct {
	manager *vm.Manager
	log     *slog.Logger
}

// New constructs a Handler.
func New(m *vm.Manager, log *slog.Logger) *Handler {
	return &Handler{manager: m, log: log}
}

// Mount registers /v1/tasks routes on r.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/{task_id}", h.Get)
}

type taskView struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	VMID      string         `json:"vm_id"`
	Error     *taskErrorView `json:"error"`
	Result    any            `json:"result"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

type taskErrorView struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Get handles GET /v1/tasks/{task_id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "task not found", nil)
		return
	}
	task := h.manager.Task(id)
	if task == nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "task not found", nil)
		return
	}

	view := taskView{
		ID:        task.ID.String(),
		Kind:      string(task.Kind),
		Status:    string(task.Status),
		VMID:      task.VMID.String(),
		CreatedAt: task.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: task.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if task.Error != nil {
		view.Error = &taskErrorView{
			Code:    task.Error.Code,
			Message: task.Error.Message,
			Details: task.Error.Details,
		}
	}
	if len(task.Result) > 0 {
		view.Result = task.Result
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}
