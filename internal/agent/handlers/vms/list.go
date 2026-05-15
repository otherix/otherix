// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
)

type listResponse struct {
	Data []vmView       `json:"data"`
	Meta map[string]any `json:"meta"`
}

// List handles GET /v1/vms.
//
// Iteration 1 returns the full VM set without pagination — the agent
// holds at most a few dozen VMs per node. The response shape uses the
// standard envelope (data + meta.next_cursor) so future agents can
// paginate without changing the wire envelope.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	vms := h.manager.List()
	views := make([]vmView, 0, len(vms))
	for _, v := range vms {
		views = append(views, toView(v))
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: map[string]any{"next_cursor": nil},
	})
}
