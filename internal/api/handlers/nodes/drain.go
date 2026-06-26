// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// workerMaxDrainAttempts caps redeliveries of the node.drain job. A drain is a
// long-lived, retry-forever-shaped saga bounded by its own wall-clock deadline,
// so the attempt cap is generous - it exists only to stop a permanently-wedged
// job from being redelivered without end.
const workerMaxDrainAttempts = 50

// drainRequestBody is the optional JSON body of POST /v1/nodes/{id}/drain. A
// positive timeout_seconds overrides the cluster default for this drain; an
// omitted or non-positive value falls back to the cluster default.
type drainRequestBody struct {
	TimeoutSeconds *int32 `json:"timeout_seconds"`
}

// Drain implements POST /v1/nodes/{id}/drain - async per the AsyncTaskAccepted
// pattern (202). Required permission: node:maintenance (admin / operator), the
// same gate cordon / uncordon use. It flips a ready or cordoned node to draining
// and enqueues the node.drain saga, which evacuates the node's VMs and finalizes
// the node back to cordoned.
//
// Idempotency: a node already draining returns its in-flight task verbatim
// (202) rather than starting a second drain. A draining node with no
// drain_task_id is a torn row - a state the atomic StartNodeDrain never writes -
// so it is logged and surfaced as 500 rather than fallen through to a fresh
// enqueue. Any other status (pending / unreachable / gone) is 409 with the
// current_status detail.
func (h *Handler) Drain(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	node, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	switch node.Status {
	case store.NodeStatusDraining:
		if node.DrainTaskID == nil {
			h.log.ErrorContext(r.Context(), "node draining with no drain_task_id",
				"node_id", node.ID.String())
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "drain node", nil)
			return
		}
		response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
			TaskID: node.DrainTaskID.String(),
			Status: string(store.TaskStatusPending),
			Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + node.DrainTaskID.String()},
		})
		return
	case store.NodeStatusReady, store.NodeStatusCordoned:
		// drainable - fall through to enqueue below.
	default:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "node cannot be drained in its current state",
			map[string]any{"current_status": string(node.Status)})
		return
	}

	// Admission cap: a node in draining holds a worker slot for the whole drain
	// plus its migrate-job slots, so an uncapped fleet drain would exhaust the
	// bounded worker pool and self-deadlock (sagas fill every slot, their own
	// migrate jobs can no longer be dispatched, no progress). Reject a new drain
	// once the cap is reached WITHOUT flipping the node or enqueuing anything. The
	// idempotent already-draining replay above starts nothing, so it is never
	// capped.
	draining, err := h.store.CountDrainingNodes(r.Context())
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "drain node", nil)
		return
	}
	if draining >= h.maxConcurrentDrains {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "too many drains in progress, retry shortly", nil)
		return
	}

	body, ok := decodeDrainBody(w, r)
	if !ok {
		return
	}

	timeoutSeconds, err := h.store.DrainTimeoutSeconds(r.Context())
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve drain timeout", nil)
		return
	}
	if body.TimeoutSeconds != nil && *body.TimeoutSeconds > 0 {
		timeoutSeconds = *body.TimeoutSeconds
	}
	deadline := time.Now().UTC().Add(time.Duration(timeoutSeconds) * time.Second)

	taskID := uuid.New()
	nodeID := node.ID
	createdBy := caller.ID
	args := NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: deadline.Unix()}
	taskParams := store.CreateTaskParams{
		ID:           taskID,
		Type:         args.Kind(),
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &nodeID,
		MaxAttempts:  workerMaxDrainAttempts,
		CreatedBy:    &createdBy,
	}

	task, err := h.store.StartNodeDrain(r.Context(), nodeID, taskParams, args)
	if err != nil {
		if errors.Is(err, store.ErrNodeNotDrainable) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "node cannot be drained in its current state",
				map[string]any{"current_status": string(node.Status)})
			return
		}
		if errors.Is(err, store.ErrConcurrentUpdate) {
			// A concurrent writer (cordon, heartbeat, delete) won the CAS. This is a
			// transient, retryable conflict, not an internal fault - the client may
			// retry the drain.
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "node was modified concurrently, retry the drain", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "start node drain failed",
			"node_id", nodeID.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "drain node", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: task.ID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + task.ID.String()},
	})
}

// decodeDrainBody decodes the optional request body. An empty body decodes to
// the zero value (cluster-default timeout). A malformed body is 400; a negative
// timeout_seconds is rejected (zero is allowed and treated as "use the
// default").
func decodeDrainBody(w http.ResponseWriter, r *http.Request) (drainRequestBody, bool) {
	var body drainRequestBody
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid request body", nil)
			return drainRequestBody{}, false
		}
	}
	if body.TimeoutSeconds != nil && *body.TimeoutSeconds < 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "timeout_seconds must be non-negative", nil)
		return drainRequestBody{}, false
	}
	return body, true
}
