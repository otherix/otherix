// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// migrateRequest is the optional JSON body of POST /v1/vms/{id}/migrate.
// Every field is optional: an empty body requests a live, scheduler-placed
// migration. target_node names a node (omitted = scheduler picks per spec
// D2); target_pool names a pool on the target (omitted = the cluster default,
// resolved by the worker); live defaults to true; allow_postcopy defaults to
// false (fail-safe pre-copy). max_bandwidth_bytes / max_downtime_ms are
// ephemeral knobs echoed onto the migration row.
type migrateRequestBody struct {
	TargetNode        *string `json:"target_node"`
	TargetPool        *string `json:"target_pool"`
	Live              *bool   `json:"live"`
	AllowPostcopy     *bool   `json:"allow_postcopy"`
	MaxBandwidthBytes *int64  `json:"max_bandwidth_bytes"`
	MaxDowntimeMs     *int32  `json:"max_downtime_ms"`
}

// Start implements POST /v1/vms/{id}/migrate - async per spec (202 +
// AsyncTaskAccepted). Required permission: vm:migrate (admin / operator: any;
// developer: own; viewer: none). A developer migrating another user's VM gets
// 404 (no existence leak), the canonical 403-vs-404 rule.
//
// Sequence: resolve VM by name -> ownership check (404 on deny) -> parse body
// -> resolve target_node name to a node id (if given) -> no-op short-circuit
// when the explicit target equals the current pinned node (spec Resolved-
// decision 3) -> atomic CreateMigration (migration row + backing task) ->
// 202. A node-less migrate (no target_node) is NEVER a no-op: the intent is
// "leave this node" and the worker schedules a target.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vm, err := resolver.VM(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeVMResolveError(w, r, err)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMMigrate); err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}

	body, ok := decodeMigrateBody(w, r)
	if !ok {
		return
	}

	// Resolve an explicit target_node to a node id, short-circuiting the no-op
	// case. ok=false means a response was already written (unknown node, no-op,
	// or store error).
	targetNodeID, ok := h.resolveTargetNode(w, r, vm, body)
	if !ok {
		return
	}

	taskID := uuid.New()
	migrationID := uuid.New()
	resID := migrationID
	createdBy := caller.ID

	params := store.CreateMigrationParams{
		ID:                migrationID,
		VmID:              vm.ID,
		SourceNodeID:      vm.PinnedNodeID,
		TargetNodeID:      targetNodeID,
		TargetPoolName:    derefString(body.TargetPool),
		InitiatedByUserID: &caller.ID,
		Reason:            store.MigrationReasonManual,
		Live:              derefBoolDefault(body.Live, true),
		AllowPostcopy:     derefBoolDefault(body.AllowPostcopy, false),
		MaxBandwidthBytes: body.MaxBandwidthBytes,
		MaxDowntimeMs:     body.MaxDowntimeMs,
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.migrate",
			Status:       store.TaskStatusPending,
			ResourceType: "migration",
			ResourceID:   &resID,
			MaxAttempts:  25,
			CreatedBy:    &createdBy,
		},
	}

	if _, err := h.store.CreateMigration(r.Context(), params, MigrationRunArgs{TaskID: taskID, MigrationID: migrationID}); err != nil {
		if errors.Is(err, store.ErrMigrationActiveExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "a migration is already active for this vm", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "migrations.start create failed",
			"vm_id", vm.ID.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "create migration", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// decodeMigrateBody decodes and validates the optional request body. On a
// malformed body or an out-of-range knob it writes a 400 and returns ok=false;
// an empty body decodes to the zero value (all defaults).
func decodeMigrateBody(w http.ResponseWriter, r *http.Request) (migrateRequestBody, bool) {
	var body migrateRequestBody
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid request body", nil)
			return migrateRequestBody{}, false
		}
	}
	if body.MaxBandwidthBytes != nil && *body.MaxBandwidthBytes < 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "max_bandwidth_bytes must be non-negative", nil)
		return migrateRequestBody{}, false
	}
	if body.MaxDowntimeMs != nil && *body.MaxDowntimeMs < 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "max_downtime_ms must be non-negative", nil)
		return migrateRequestBody{}, false
	}
	return body, true
}

// resolveTargetNode resolves an explicit target_node name to a node id. A
// node-less request (no target_node) returns (nil, true) - the worker schedules
// a target. An unknown node name is 404; a store error is 500. When the explicit
// target equals the VM's current pinned node it writes the friendly no-op
// (200 already_on_target, spec Resolved-decision 3) and returns ok=false so the
// caller stops before creating a migration. ok=false always means a response was
// already written.
func (h *Handler) resolveTargetNode(w http.ResponseWriter, r *http.Request, vm store.VM, body migrateRequestBody) (*uuid.UUID, bool) {
	if body.TargetNode == nil || *body.TargetNode == "" {
		return nil, true
	}
	node, err := h.store.NodeByName(r.Context(), *body.TargetNode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNodeNotFound, "target node not found", nil)
			return nil, false
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve target node", nil)
		return nil, false
	}
	if vm.PinnedNodeID != nil && *vm.PinnedNodeID == node.ID {
		response.WriteJSON(w, r, http.StatusOK, map[string]string{
			"status":          "already_on_target",
			"current_node_id": node.ID.String(),
		})
		return nil, false
	}
	id := node.ID
	return &id, true
}

// writeVMResolveError maps a resolver.Error from the {id} path param to the
// VM 404 / UUID-rejection / 500 envelopes. VM identifiers are name-only - a
// UUID literal in the path is 400 validation_failed.
func writeVMResolveError(w http.ResponseWriter, r *http.Request, err error) {
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

// derefString returns *s or "" when nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefBoolDefault returns *b or def when nil.
func derefBoolDefault(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
