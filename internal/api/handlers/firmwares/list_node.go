// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// ListByNode implements GET /v1/nodes/{id}/firmwares. Required
// permission: firmware:read. {id} is name-only; UUID literals are
// rejected with 400 validation_failed at the resolver.
//
// Until the agent heartbeat receiver lands, the underlying
// node_firmwares table is always empty for any node and the response
// data array is consequently empty. The endpoint is wired today so
// clients can exercise the contract end-to-end ahead of the ingest
// path.
func (h *Handler) ListByNode(w http.ResponseWriter, r *http.Request) {
	node, err := resolver.Node(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		if resolver.IsUUIDInName(err) {
			response.WriteUUIDNotAllowedError(w, r, "node", "id")
			return
		}
		if resolver.IsNotFound(err) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "node not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return
	}

	q := r.URL.Query()
	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))
	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	params := store.ListNodeFirmwaresParams{
		NodeID:     node.ID,
		LimitCount: limit + 1,
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListNodeFirmwares(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list node firmwares", nil)
		return
	}

	limitN := int(limit)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{
			CreatedAt: last.Firmware.CreatedAt,
			ID:        last.Firmware.ID,
		})
		nextCursor = &s
		rows = rows[:limitN]
	}

	views := make([]nodeFirmwareView, 0, len(rows))
	for _, row := range rows {
		views = append(views, toNodeFirmwareView(row.NodeFirmware, row.Firmware))
	}
	response.WriteJSON(w, r, http.StatusOK, nodeFirmwareListResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
