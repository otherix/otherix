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

// TestDrainAdmissionCapRejectsBeyondLimit proves the drain-admission cap: with
// the router built at cap=1, draining node-1 succeeds (202) and leaves it
// draining, but draining node-2 while node-1 is still draining is rejected with
// 409 and node-2 is NOT flipped (no task created, status unchanged). The
// idempotent replay on node-1 still returns 202 - the cap gates only the
// start-a-new-drain path. The apie2e harness runs no dispatcher, so the enqueued
// node.drain job sits pending and node-1 stays draining across the calls.
func TestDrainAdmissionCapRejectsBeyondLimit(t *testing.T) {
	h := newE2E(t, withMaxConcurrentDrains(1))
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	node1 := seedNodeWithStatus(t, h.store, store.NodeStatusReady)
	node2 := seedNodeWithStatus(t, h.store, store.NodeStatusReady)
	ctx := context.Background()

	first := h.post(t, "/v1/nodes/"+node1+"/drain", nil, admin)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("drain node-1 status = %d, want 202", first.StatusCode)
	}

	// node-2 is rejected because node-1 is already draining and the cap is 1.
	second := h.post(t, "/v1/nodes/"+node2+"/drain", nil, admin)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("drain node-2 status = %d, want 409 (cap reached)", second.StatusCode)
	}
	var conflict drainConflictView
	decodeJSON(t, second, &conflict)
	if conflict.Error.Code != "conflict" {
		t.Errorf("node-2 reject code = %q, want conflict", conflict.Error.Code)
	}

	// node-2 must be untouched: still ready, no drain task stamped.
	n2, err := h.store.NodeByName(ctx, node2)
	if err != nil {
		t.Fatalf("NodeByName(node2): %v", err)
	}
	if n2.Status != store.NodeStatusReady {
		t.Errorf("node-2 status = %v, want ready (not flipped by a rejected drain)", n2.Status)
	}
	if n2.DrainTaskID != nil {
		t.Errorf("node-2 DrainTaskID = %v, want nil (no task created)", n2.DrainTaskID)
	}

	// The idempotent replay on node-1 is NOT capped: it starts nothing, so it
	// still returns its in-flight task verbatim.
	replay := h.post(t, "/v1/nodes/"+node1+"/drain", nil, admin)
	if replay.StatusCode != http.StatusAccepted {
		t.Errorf("idempotent replay on node-1 status = %d, want 202 (replay is never capped)", replay.StatusCode)
	}
}

// TestCancelRunningDrainSetsMarker proves the cooperative cancel branch fires
// for a running node.drain: cancelling it returns 200 and sets the drain cancel
// marker the saga polls. The apie2e harness runs no worker dispatcher, so the
// enqueued drain stays pending and the saga never runs; the test forces the
// task to running at the store layer (the same UpdateTaskRunning the saga uses)
// to exercise the running branch deterministically, then asserts the marker
// directly rather than polling for a cancelled status that nothing would set.
func TestCancelRunningDrainSetsMarker(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	name := seedNodeWithStatus(t, h.store, store.NodeStatusReady)
	ctx := context.Background()

	resp := h.post(t, "/v1/nodes/"+name+"/drain", nil, admin)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("drain status = %d, want 202", resp.StatusCode)
	}
	var accepted asyncTaskAcceptedView
	decodeJSON(t, resp, &accepted)
	taskID, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("parse task id %q: %v", accepted.TaskID, err)
	}

	if _, err := h.store.UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}

	cancel := h.post(t, "/v1/tasks/"+accepted.TaskID+"/cancel", nil, admin)
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", cancel.StatusCode)
	}

	requested, err := h.store.DrainCancelRequested(ctx, taskID)
	if err != nil {
		t.Fatalf("DrainCancelRequested: %v", err)
	}
	if !requested {
		t.Errorf("DrainCancelRequested = false, want true after cancelling a running drain")
	}
}
