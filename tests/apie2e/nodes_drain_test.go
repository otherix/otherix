// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// asyncTaskAcceptedView mirrors the AsyncTaskAccepted wire schema returned by
// the 202 drain response.
type asyncTaskAcceptedView struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Links  struct {
		Self string `json:"self"`
	} `json:"links"`
}

// drainConflictView captures the conflict envelope's current_status detail.
type drainConflictView struct {
	Error struct {
		Code    string `json:"code"`
		Details struct {
			CurrentStatus string `json:"current_status"`
		} `json:"details"`
	} `json:"error"`
}

// seedNodeWithStatus inserts a node row directly with the given status and
// returns its name. The HTTP create path always lands a node in `pending`, so
// drainable states (ready / cordoned) are seeded at the store layer.
func seedNodeWithStatus(t *testing.T, s *etcdstore.Store, status store.NodeStatus) string {
	t.Helper()
	name := "drain-node-" + uuid.NewString()[:8]
	if _, err := s.CreateNode(context.Background(), store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  status,
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	return name
}

func TestDrainAcceptedOnReadyNode(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	name := seedNodeWithStatus(t, h.store, store.NodeStatusReady)

	resp := h.post(t, "/v1/nodes/"+name+"/drain", nil, admin)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("drain status = %d, want 202", resp.StatusCode)
	}
	var out asyncTaskAcceptedView
	decodeJSON(t, resp, &out)
	if out.TaskID == "" {
		t.Errorf("task_id empty: %+v", out)
	}
	if out.Status != string(store.TaskStatusPending) {
		t.Errorf("status = %q, want %q", out.Status, store.TaskStatusPending)
	}
	if out.Links.Self != "/v1/tasks/"+out.TaskID {
		t.Errorf("links.self = %q, want %q", out.Links.Self, "/v1/tasks/"+out.TaskID)
	}
}

func TestDrainConflictOnPendingNode(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	name := seedNodeWithStatus(t, h.store, store.NodeStatusPending)

	resp := h.post(t, "/v1/nodes/"+name+"/drain", nil, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("drain status = %d, want 409", resp.StatusCode)
	}
	var out drainConflictView
	decodeJSON(t, resp, &out)
	if out.Error.Code != "conflict" {
		t.Errorf("code = %q, want conflict", out.Error.Code)
	}
	if out.Error.Details.CurrentStatus != string(store.NodeStatusPending) {
		t.Errorf("current_status = %q, want %q", out.Error.Details.CurrentStatus, store.NodeStatusPending)
	}
}

// TestDrainIdempotentReturnsSameTask asserts a second drain POST against an
// already-draining node returns the original task rather than enqueuing a new
// one. The apie2e harness does not run the worker dispatcher, so the enqueued
// node.drain job sits pending and the node stays draining between the two
// POSTs - the second call therefore resolves the in-flight task verbatim.
func TestDrainIdempotentReturnsSameTask(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	name := seedNodeWithStatus(t, h.store, store.NodeStatusReady)

	first := h.post(t, "/v1/nodes/"+name+"/drain", nil, admin)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first drain status = %d, want 202", first.StatusCode)
	}
	var firstOut asyncTaskAcceptedView
	decodeJSON(t, first, &firstOut)
	if firstOut.TaskID == "" {
		t.Fatalf("first task_id empty: %+v", firstOut)
	}

	second := h.post(t, "/v1/nodes/"+name+"/drain", nil, admin)
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("second drain status = %d, want 202", second.StatusCode)
	}
	var secondOut asyncTaskAcceptedView
	decodeJSON(t, second, &secondOut)
	if secondOut.TaskID != firstOut.TaskID {
		t.Errorf("second task_id = %q, want %q (same in-flight task)", secondOut.TaskID, firstOut.TaskID)
	}
}
