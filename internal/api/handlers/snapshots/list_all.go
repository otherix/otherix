// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/replication"
	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// snapshotListItemView is the global-list projection: the per-VM vmSnapshotView
// plus the operator-facing vm_name + owner_display_name resolved server-side.
// Mirrors components/schemas/SnapshotListItem. A name is nil only when its
// referenced row is gone (force-deleted VM / user); the id field stays
// authoritative.
type snapshotListItemView struct {
	vmSnapshotView
	VMName           *string `json:"vm_name"`
	OwnerDisplayName *string `json:"owner_display_name"`
}

// listAllResponse is the cursor-list envelope for GET /v1/snapshots.
type listAllResponse struct {
	Data []snapshotListItemView `json:"data"`
	Meta paginationMeta         `json:"meta"`
}

// ListAll implements GET /v1/snapshots, the global snapshot collection.
// Required permission: snapshot:read. Cursor pagination newest-first by
// (created_at, id); optional ?vm= (VM id), ?status=, and (any-scope only)
// ?owner= filters.
//
// RBAC scope is the seam: a snapshot:read=any caller (admin, operator) lists
// every snapshot (OwnerID filter nil unless ?owner= narrows it); a
// snapshot:read=own caller (developer) is pinned to OwnerID = caller.ID by the
// store filter, so a foreign snapshot is never returned and existence is not
// leaked. An own-scope caller passing ?owner= cannot widen scope - it is
// rejected 403 permission_denied rather than silently ignored, so the
// can't-widen rule is explicit.
func (h *Handler) ListAll(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
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

	params := store.ListSnapshotsParams{LimitCount: limit + 1}
	if raw := q.Get("vm"); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "vm must be a uuid", nil)
			return
		}
		params.VmID = &id
	}
	if raw := q.Get("status"); raw != "" {
		st := store.SnapshotStatus(raw)
		params.Status = &st
	}

	scope := auth.ScopeFor(caller.Role, auth.PermSnapshotRead)
	switch scope {
	case auth.ScopeOwn:
		// An own-scope caller is pinned to their own snapshots. Passing
		// ?owner= can only ever name themselves; treat any ?owner= as an
		// attempt to widen and reject it (no silent narrowing surprise).
		if q.Get("owner") != "" {
			response.WriteError(w, r, http.StatusForbidden,
				response.CodePermissionDenied,
				"insufficient scope to filter by owner",
				map[string]any{"required_permission": string(auth.PermSnapshotRead)})
			return
		}
		owner := caller.ID
		params.OwnerID = &owner
	case auth.ScopeAny:
		if raw := q.Get("owner"); raw != "" {
			id, perr := uuid.Parse(raw)
			if perr != nil {
				response.WriteError(w, r, http.StatusBadRequest,
					response.CodeValidationFailed, "owner must be a uuid", nil)
				return
			}
			params.OwnerID = &id
		}
	default:
		// No snapshot:read at all - RequirePermission gates the route, so
		// this is unreachable in production; fail closed for safety.
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied, "permission denied",
			map[string]any{"required_permission": string(auth.PermSnapshotRead)})
		return
	}

	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListSnapshots(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list snapshots", nil)
		return
	}

	limitN := int(limit)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		nextCursor = &s
		rows = rows[:limitN]
	}

	views := h.resolveListItems(r.Context(), rows)

	response.WriteJSON(w, r, http.StatusOK, listAllResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// resolveListItems projects the page of snapshot rows onto the global-list view,
// resolving owner_display_name once per unique user id (a small per-page cache).
// vm_name prefers the live current VM name (so a rename is reflected) and falls
// back to the snapshot's stored SourceVMName when the source VM row is gone, so
// the column is always non-empty: a snapshot is a standalone artifact that
// remembers the VM it was captured from even after that VM is force-deleted. The
// live lookup is cached once per unique VM id (a page of N snapshots of one VM
// is one lookup, not N).
func (h *Handler) resolveListItems(ctx context.Context, rows []store.Snapshot) []snapshotListItemView {
	liveVMNames := map[uuid.UUID]*string{}
	ownerNames := map[uuid.UUID]*string{}

	// One page-scoped durability resolver for the whole page: the node inventory
	// is scanned once here, not once per row (see viewWithDurabilityPaged). On a
	// construction error every row renders "unknown" durability, best-effort.
	resolver, err := replication.NewDurabilityResolver(ctx, h.store)
	if err != nil {
		h.log.WarnContext(ctx, "build snapshot durability resolver", slog.Any("error", err))
	}

	views := make([]snapshotListItemView, 0, len(rows))
	for _, s := range rows {
		live, ok := liveVMNames[s.VmID]
		if !ok {
			live = h.vmName(ctx, s.VmID)
			liveVMNames[s.VmID] = live
		}
		// Prefer the live current name (reflects a rename); fall back to the
		// provenance-at-capture name stored on the snapshot when the VM is gone.
		vmName := live
		if vmName == nil {
			captured := s.SourceVMName
			vmName = &captured
		}

		ownerName, ok := ownerNames[s.OwnerID]
		if !ok {
			ownerName = h.ownerDisplayName(ctx, s.OwnerID)
			ownerNames[s.OwnerID] = ownerName
		}
		views = append(views, snapshotListItemView{
			vmSnapshotView:   h.viewWithDurabilityPaged(ctx, resolver, s),
			VMName:           vmName,
			OwnerDisplayName: ownerName,
		})
	}
	return views
}

// vmName resolves a VM id to its *string name, returning nil when the VM row is
// gone (force-deleted out from under a historical snapshot). A non-ErrNotFound
// store error is logged and rendered nil rather than failing the request.
func (h *Handler) vmName(ctx context.Context, id uuid.UUID) *string {
	vm, err := h.store.VMByID(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.log.WarnContext(ctx, "snapshots: resolve vm name",
				"vm_id", id.String(), "error", err)
		}
		return nil
	}
	n := vm.Name
	return &n
}

// ownerDisplayName resolves an owner id to its *string display name, returning
// nil when the user row is gone. A non-ErrNotFound store error is logged and
// rendered nil rather than failing the request.
func (h *Handler) ownerDisplayName(ctx context.Context, id uuid.UUID) *string {
	u, err := h.store.UserByID(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.log.WarnContext(ctx, "snapshots: resolve owner display name",
				"owner_id", id.String(), "error", err)
		}
		return nil
	}
	n := u.DisplayName
	return &n
}
