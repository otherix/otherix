// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package tasks hosts the /v1/tasks/* HTTP handlers for the user-facing
// async-operation surface.
//
// All routes are gated by `task:read` at the role level via
// middleware.RequirePermission. Per-row ownership is then enforced
// inside the handlers based on auth.ScopeFor:
//
//   - admin / operator hold task:read at ScopeAny: list returns every
//     task; get returns any task by id.
//   - developer / viewer hold task:read at ScopeOwn: list returns only
//     tasks whose created_by equals the principal's id; get returns
//     404 (NOT 403, per CLAUDE.md no-existence-leak rule) when the
//     row exists but belongs to another user.
//
// Internal fields (`args`, `job_id`, `created_by`) are not
// surfaced through the public Task projection — args is a worker
// payload, job_id is an ops drill-down weak ref, created_by is
// an RBAC scoping anchor. The view mirrors components/schemas/Task in
// api/openapi/control-plane.yaml verbatim.
package tasks

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the tasks handlers depend on. Cancel's
// transactional job-cancel is hidden inside store.CancelPendingTask
// (which uses the queue seam), so the handler no longer holds a queue
// or worker dispatcher. Depending on the interface narrows the handler's
// storage dependency to the methods it uses and lets tests substitute a
// fake. *etcdstore.Store satisfies it.
type Store interface {
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	CancelPendingTask(ctx context.Context, id uuid.UUID, jobRef *int64) (store.Task, error)
	ListTasksAny(ctx context.Context, arg store.ListTasksAnyParams) ([]store.Task, error)
	ListTasksOwn(ctx context.Context, arg store.ListTasksOwnParams) ([]store.Task, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the tasks routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// taskView mirrors components/schemas/Task in
// api/openapi/control-plane.yaml. Internal fields (args, job_id,
// created_by) are intentionally absent.
type taskView struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	Progress     *int            `json:"progress"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *string         `json:"resource_id"`
	Result       json.RawMessage `json:"result"`
	Error        json.RawMessage `json:"error"`
	Attempts     int32           `json:"attempts"`
	MaxAttempts  int32           `json:"max_attempts"`
	CreatedAt    string          `json:"created_at"`
	StartedAt    *string         `json:"started_at"`
	FinishedAt   *string         `json:"finished_at"`
}

// toView projects a store.Task onto its public taskView. nil JSONB
// payloads marshal as the JSON literal `null` (default behaviour of
// json.RawMessage when nil), matching the OpenAPI `[object, "null"]`
// declaration for `result` and `error`.
func toView(t store.Task) taskView {
	view := taskView{
		ID:           t.ID.String(),
		Type:         t.Type,
		Status:       string(t.Status),
		ResourceType: t.ResourceType,
		Result:       jsonOrNull(t.Result),
		Error:        jsonOrNull(t.Error),
		Attempts:     t.Attempts,
		MaxAttempts:  t.MaxAttempts,
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.Progress != nil {
		p := int(*t.Progress)
		view.Progress = &p
	}
	if t.ResourceID != nil {
		s := t.ResourceID.String()
		view.ResourceID = &s
	}
	if t.StartedAt != nil {
		s := t.StartedAt.UTC().Format(time.RFC3339Nano)
		view.StartedAt = &s
	}
	if t.FinishedAt != nil {
		s := t.FinishedAt.UTC().Format(time.RFC3339Nano)
		view.FinishedAt = &s
	}
	return view
}

// jsonOrNull returns raw verbatim when populated, or nil so the
// json.RawMessage marshals as JSON `null`. Distinguishes "absent
// column" (NULL JSONB) from "present empty object" — we want null,
// not `{}`, in the wire response when the column is unset.
func jsonOrNull(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}
