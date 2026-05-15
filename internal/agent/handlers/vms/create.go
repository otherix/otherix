// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

type createRequest struct {
	// UUID is optional. When supplied it is used as the agent-side VM
	// id (unified UUID model: CP mints, agent uses). When absent the
	// agent generates a fresh uuid - backward compatible с pre-Phase A
	// callers.
	//
	// `pool` is the pool **name**. The agent's local registry is
	// name-keyed; the cluster-wide UUID polymorphism stays an
	// operator-edge concern.
	UUID             string `json:"uuid,omitempty"`
	Name             string `json:"name"`
	VCPUs            int    `json:"vcpus"`
	MemoryMB         int    `json:"memory_mb"`
	Pool             string `json:"pool"`
	TemplateChecksum string `json:"template_checksum"`
	// UserData carries CP-resolved raw `#cloud-config` YAML (L3 Area
	// 3 lock). Optional — empty value skips cidata generation.
	// CP-side resolver merges vm.user_data ?:
	// template.cloud_init_user_data и injects а top-level `hostname:`
	// matching the VM name when missing.
	UserData string `json:"user_data,omitempty"`
}

type asyncAccepted struct {
	TaskID string         `json:"task_id"`
	Status string         `json:"status"`
	Links  map[string]any `json:"links"`
}

// Create handles POST /v1/vms.
//
//   - Validates the request body shape.
//   - Asks the manager to begin async creation; receives an agent task.
//   - Returns 202 with task_id immediately; clients poll /v1/tasks/{id}.
//
// The response envelope mirrors AsyncTaskAccepted from the CP-side
// spec: { task_id, status, links: { self } }. The agent has no SQL
// "tasks" table — task IDs are agent-local for the scope of one
// process run.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Pool == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool is required", nil)
		return
	}

	var (
		vmID uuid.UUID
		err  error
	)
	if req.UUID != "" {
		vmID, err = uuid.Parse(req.UUID)
		if err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "uuid is not a valid UUID", nil)
			return
		}
	}

	task, err := h.manager.Create(r.Context(), vm.CreateSpec{
		UUID:             vmID,
		Name:             req.Name,
		VCPUs:            req.VCPUs,
		MemoryMB:         req.MemoryMB,
		PoolName:         req.Pool,
		TemplateChecksum: req.TemplateChecksum,
		UserData:         []byte(req.UserData),
	})
	if err != nil {
		mapCreateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

func mapCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vm.ErrInvalidSpec):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
	case errors.Is(err, vm.ErrPoolUnknown):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool does not match a configured pool", nil)
	case errors.Is(err, vm.ErrTemplateMissing):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "template not found on pool", nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
	}
}
