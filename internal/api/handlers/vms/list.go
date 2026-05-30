// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// vmListResponse is the envelope shape every list endpoint serves -
// {data, meta.next_cursor}.
type vmListResponse struct {
	Data []vmView   `json:"data"`
	Meta vmListMeta `json:"meta"`
}

type vmListMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// listCursor is the opaque pagination cursor base64'd onto the wire.
// Encodes the last-returned row's (created_at, id) pair — clients
// treat it as opaque per the spec.
type listCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

const (
	listDefaultLimit = 50
	listMaxLimit     = 200
)

// List implements GET /v1/vms. Cursor pagination with optional filters:
//
//   - ?status=        — projection filter (status is computed, not stored)
//   - ?pool=<name|id> — only VMs whose disk lives on the named pool
//   - ?node=<name|id> — only VMs currently located on the named node
//     (vm_runtime.current_node_id)
//
// Per F1: vm:read=any for every role, so this never falls into the
// owner-scoped path; ListVMsByOwner stays inactive.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	cursor, ok := parseCursor(w, r)
	if !ok {
		return
	}
	poolID, ok := h.resolveListFilter(w, r, "pool", resolvePoolID)
	if !ok {
		return
	}
	nodeID, ok := h.resolveListFilter(w, r, "node", resolveNodeID)
	if !ok {
		return
	}
	statusFilter := r.URL.Query().Get("status")

	rows, err := h.store.ListVMs(r.Context(), store.ListVMsParams{
		PoolIDFilter:    poolID,
		NodeIDFilter:    nodeID,
		CursorCreatedAt: cursor.cursorTs,
		CursorID:        cursor.cursorID,
		LimitCount:      int32(limit), //nolint:gosec // bounded to 1..200 by parseLimit
	})
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.list query failed", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list vms", nil)
		return
	}

	views, err := h.projectPage(r.Context(), rows, statusFilter)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.list projection failed", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list vms", nil)
		return
	}

	resp := vmListResponse{Data: views, Meta: vmListMeta{NextCursor: nextCursorFor(rows, limit)}}
	response.WriteJSON(w, r, http.StatusOK, resp)
}

// resolvePoolID / resolveNodeID return the underlying row ID for the
// matching resolver call. The list handler keys filter resolution off
// these adapters so the same generic resolveListFilter helper handles
// both pool and node — only the resolver function differs.
func resolvePoolID(ctx context.Context, q resolver.Querier, identifier string) (uuid.UUID, error) {
	row, err := resolver.Pool(ctx, q, identifier)
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

func resolveNodeID(ctx context.Context, q resolver.Querier, identifier string) (uuid.UUID, error) {
	row, err := resolver.Node(ctx, q, identifier)
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

// resolveListFilter parses an optional `?<param>=<identifier>` query
// arg and turns it into a UUID pointer suitable for the ListVMsParams
// optional-filter slots. Returns (nil, true) when the param is absent;
// writes a 400 / 404 envelope and returns (_, false) on resolution
// failure.
func (h *Handler) resolveListFilter(
	w http.ResponseWriter, r *http.Request,
	param string,
	resolve func(context.Context, resolver.Querier, string) (uuid.UUID, error),
) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get(param)
	if raw == "" {
		return nil, true
	}
	id, err := resolve(r.Context(), h.store, raw)
	if err != nil {
		// Node filter rejects UUID format (name-only); the pool filter
		// retains UUID acceptance for the multi-instance carve-out, so
		// Pool resolver never emits CodeUUIDInName.
		if resolver.IsUUIDInName(err) {
			response.WriteUUIDNotAllowedError(w, r, param, param)
			return nil, false
		}
		if resolver.IsNotFound(err) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed,
				"unknown "+param,
				map[string]any{param: raw})
			return nil, false
		}
		// A bare pool name may match multiple per-node instances; the
		// list-filter path cannot pick one without guessing. Surface
		// the operator-actionable pool_name_ambiguous code so the CLI
		// can prompt for a specific instance UUID instead of failing
		// with 500.
		var rerr *resolver.Error
		if errors.As(err, &rerr) && rerr.Code == resolver.CodeAmbiguous {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodePoolNameAmbiguous,
				"filter value resolves to multiple instances; pass an instance UUID",
				map[string]any{param: raw})
			return nil, false
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve list filter", nil)
		return nil, false
	}
	return &id, true
}

// projectPage runs per-row sidecar queries (vm_runtime + first
// vm_disks + name resolution) against ctx and returns the projected
// views, filtered by statusFilter when non-empty. Inconsistent rows
// (no vm_disks) are skipped silently — the inventory listing must not
// abort because one row is in a transient state.
func (h *Handler) projectPage(ctx context.Context, rows []store.VM, statusFilter string) ([]vmView, error) {
	views := make([]vmView, 0, len(rows))
	for _, vm := range rows {
		runtime, err := h.loadRuntimeOrNil(ctx, vm.ID)
		if err != nil {
			return nil, err
		}
		disks, err := h.store.ListVMDisksByVM(ctx, vm.ID)
		if err != nil {
			return nil, err
		}
		if len(disks) == 0 {
			continue
		}
		names, err := h.resolveViewNames(ctx, vm, runtime, disks[0])
		if err != nil {
			return nil, err
		}
		view := toView(vm, runtime, names)
		if statusFilter != "" && view.Status != statusFilter {
			continue
		}
		views = append(views, view)
	}
	return views, nil
}

// loadRuntimeOrNil collapses pgx.ErrNoRows to (nil, nil). Used by Get
// and List so the "creating" state projection stays consistent.
func (h *Handler) loadRuntimeOrNil(ctx context.Context, vmID uuid.UUID) (*store.VMRuntime, error) {
	rt, err := h.store.VMRuntimeByID(ctx, vmID)
	switch {
	case err == nil:
		return &rt, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

// nextCursorFor returns the encoded cursor pointing to the last row in
// rows when the page is full (len == limit). nil otherwise — either
// the inventory ended or the post-projection filter dropped rows but
// the SQL pagination already advanced; the next call still resumes
// from rows[last].
func nextCursorFor(rows []store.VM, limit int) *string {
	if len(rows) < limit {
		return nil
	}
	last := rows[len(rows)-1]
	enc, err := encodeCursor(listCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil {
		// Encoding a (time, uuid) pair never fails in practice; if it
		// did, the worst case is the client missing a page boundary,
		// not data loss. Return nil and let the caller's logger pick
		// up the parent error if any.
		return nil
	}
	return &enc
}

// parseLimit decodes ?limit; defaults to listDefaultLimit, clamped to
// [1, listMaxLimit].
func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return listDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > listMaxLimit {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "limit must be an integer in [1, 200]", nil)
		return 0, false
	}
	return n, true
}

type cursorParams struct {
	cursorTs *time.Time
	cursorID *uuid.UUID
}

// parseCursor decodes the opaque cursor from ?cursor. Empty cursor
// returns zero-valued cursorParams (first page).
func parseCursor(w http.ResponseWriter, r *http.Request) (cursorParams, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return cursorParams{}, true
	}
	bytes, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "cursor is not valid base64", nil)
		return cursorParams{}, false
	}
	var c listCursor
	if err := json.Unmarshal(bytes, &c); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "cursor is malformed", nil)
		return cursorParams{}, false
	}
	ts := c.CreatedAt
	id := c.ID
	return cursorParams{cursorTs: &ts, cursorID: &id}, true
}

// encodeCursor base64-URL-encodes a JSON cursor.
func encodeCursor(c listCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}
