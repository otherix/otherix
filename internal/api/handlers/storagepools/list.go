// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/storage-pools. Required permission:
// storage_pool:read. Cursor pagination; optional
// `?node=<name|uuid>` and `?type=<value>` filters. The pre-Phase-1
// `?node_id=` spelling is gone — clients pass `?node=` carrying either
// a UUID literal or a node name; the resolver decides.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	params, ok := h.buildListParams(w, r)
	if !ok {
		return
	}

	rows, err := h.store.Queries().ListPoolsEffective(r.Context(), store.ListPoolsEffectiveParams(params))
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list storage pools", nil)
		return
	}

	limitN := int(params.LimitCount - 1)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		nextCursor = &s
		rows = rows[:limitN]
	}

	views, ok := h.projectListViews(w, r, rows)
	if !ok {
		return
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// buildListParams folds the query-parameter parsing (cursor, node
// resolver, type validation) into a single ListStoragePoolsParams
// struct. ok=false signals that the response envelope has already
// been written.
func (h *Handler) buildListParams(w http.ResponseWriter, r *http.Request) (store.ListStoragePoolsParams, bool) {
	q := r.URL.Query()
	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))
	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return store.ListStoragePoolsParams{}, false
	}
	params := store.ListStoragePoolsParams{LimitCount: limit + 1}
	if nodeRaw := q.Get("node"); nodeRaw != "" {
		node, err := resolver.Node(r.Context(), h.store.Queries(), nodeRaw)
		if err != nil {
			if resolver.IsUUIDInName(err) {
				response.WriteUUIDNotAllowedError(w, r, "node", "node")
				return store.ListStoragePoolsParams{}, false
			}
			if resolver.IsNotFound(err) {
				response.WriteError(w, r, http.StatusBadRequest,
					response.CodeValidationFailed,
					"unknown node",
					map[string]any{"node": nodeRaw})
				return store.ListStoragePoolsParams{}, false
			}
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "resolve node filter", nil)
			return store.ListStoragePoolsParams{}, false
		}
		params.NodeID = &node.ID
	}
	if t := q.Get("type"); t != "" {
		if err := validation.ValidateStoragePoolType(t); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return store.ListStoragePoolsParams{}, false
		}
		params.Type = &t
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}
	return params, true
}

// projectListViews loads the cluster default-pool name once and
// projects each pool_effective_capacity row into its public view. The
// owning node row gets a single round-trip per pool; a future revision
// may batch this query when fleet size makes the N+1 cost matter.
// ok=false signals that an internal envelope was already written.
func (h *Handler) projectListViews(w http.ResponseWriter, r *http.Request, rows []store.PoolEffectiveCapacity) ([]storagePoolView, bool) {
	defaultName, ok := h.loadDefaultPoolName(w, r)
	if !ok {
		return nil, false
	}
	views := make([]storagePoolView, 0, len(rows))
	for _, p := range rows {
		node, err := h.loadOwningNode(r.Context(), p.NodeID)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load owning node", nil)
			return nil, false
		}
		isDefault := defaultName != "" && strings.EqualFold(defaultName, p.Name)
		views = append(views, toViewEffective(p, node.Name, node.Status, isDefault))
	}
	return views, true
}

func (h *Handler) loadDefaultPoolName(w http.ResponseWriter, r *http.Request) (string, bool) {
	settings, err := h.store.Queries().GetClusterSettings(r.Context())
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load cluster settings", nil)
		return "", false
	}
	if settings.DefaultPoolName == nil {
		return "", true
	}
	return *settings.DefaultPoolName, true
}

// loadOwningNode resolves the storage pool's owning node row. A soft-
// deleted owner surfaces as a zero-value Node (well-typed but empty
// Name on the wire) so the pool view stays renderable; the operator
// can dig into the FK inconsistency via /v1/nodes if needed.
func (h *Handler) loadOwningNode(ctx context.Context, nodeID uuid.UUID) (store.Node, error) {
	row, err := h.store.Queries().GetNodeByID(ctx, nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Node{}, nil
		}
		return store.Node{}, err
	}
	return row, nil
}
