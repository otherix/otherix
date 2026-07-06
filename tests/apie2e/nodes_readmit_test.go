// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// seedNodeStatus inserts a node row directly with the given name and status and
// returns the created row. The HTTP create path always lands a node in
// `pending`, so terminal / operator-pinned states are seeded at the store layer.
func seedNodeStatus(t *testing.T, h *harness, name string, status store.NodeStatus) store.Node {
	t.Helper()
	n, err := h.store.CreateNode(context.Background(), store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  status,
	})
	if err != nil {
		t.Fatalf("CreateNode(%s): %v", status, err)
	}
	return n
}

// stampFreshHeartbeat refreshes a node's last_heartbeat_at to now via the real
// heartbeat projection, mirroring what a live agent heartbeat does.
func stampFreshHeartbeat(t *testing.T, h *harness, nodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	cores := int32(8)
	mem := int64(16384)
	if err := h.store.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpdateNodeHeartbeat(ctx, store.UpdateNodeHeartbeatParams{
			ID:                      nodeID,
			MigrationHost:           "10.0.0.1",
			MigrationPortRangeStart: 49152,
			MigrationPortRangeEnd:   49251,
			CPUCoresTotal:           &cores,
			CPUCoresAvailable:       &cores,
			MemoryTotalMib:          &mem,
			MemoryAvailableMib:      &mem,
		})
	}); err != nil {
		t.Fatalf("stampFreshHeartbeat: %v", err)
	}
}

func TestNodeReadmit(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// gone -> 200 pending
	gone := seedNodeStatus(t, h, "gone-1", store.NodeStatusGone)
	resp := h.post(t, "/v1/nodes/"+gone.Name+"/readmit", nil, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readmit(gone) = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Status string `json:"status"`
	}
	decodeJSON(t, resp, &view)
	if view.Status != "pending" {
		t.Errorf("readmit(gone).status = %q, want pending", view.Status)
	}

	// pending -> 200 no-op
	if r := h.post(t, "/v1/nodes/"+gone.Name+"/readmit", nil, admin); r.StatusCode != http.StatusOK {
		t.Fatalf("readmit(pending) = %d, want 200 no-op", r.StatusCode)
	}

	// other states -> 409 conflict with current_status
	for _, status := range []store.NodeStatus{
		store.NodeStatusReady, store.NodeStatusCordoned,
		store.NodeStatusDraining, store.NodeStatusUnreachable,
	} {
		n := seedNodeStatus(t, h, "n-"+string(status), status)
		r := h.post(t, "/v1/nodes/"+n.Name+"/readmit", nil, admin)
		if r.StatusCode != http.StatusConflict {
			t.Fatalf("readmit(%s) = %d, want 409", status, r.StatusCode)
		}
		var env errorEnvelope
		decodeJSON(t, r, &env)
		if env.Error.Code != "conflict" {
			t.Errorf("readmit(%s) code = %q, want conflict", status, env.Error.Code)
		}
		if got := env.Error.Details["current_status"]; got != string(status) {
			t.Errorf("readmit(%s) current_status = %v, want %s", status, got, status)
		}
	}

	// unknown node -> 404
	if r := h.post(t, "/v1/nodes/does-not-exist/readmit", nil, admin); r.StatusCode != http.StatusNotFound {
		t.Errorf("readmit(unknown) = %d, want 404", r.StatusCode)
	}

	// RBAC: developer lacks node:maintenance -> 403
	dev, _ := loginAs(t, h, auth.RoleDeveloper)
	g2 := seedNodeStatus(t, h, "gone-2", store.NodeStatusGone)
	if r := h.post(t, "/v1/nodes/"+g2.Name+"/readmit", nil, dev); r.StatusCode != http.StatusForbidden {
		t.Errorf("readmit(developer) = %d, want 403", r.StatusCode)
	}
}

// TestNodeReadmit_PromotesToReady is the seam test the spec requires: readmit
// takes a gone node to pending, and the real promotion path (not a re-read of
// the store flip) advances it back to ready on a fresh heartbeat. This drives
// the cross-component contract - readmit flips the row to pending and
// PromoteHealthyNodes then promotes it once a fresh heartbeat lands.
// Testing the store flip alone would re-test the ReadmitNode unit, not the seam.
func TestNodeReadmit_PromotesToReady(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	gone := seedNodeStatus(t, h, "gone-hb", store.NodeStatusGone)

	// Readmit via the real endpoint.
	if r := h.post(t, "/v1/nodes/"+gone.Name+"/readmit", nil, admin); r.StatusCode != http.StatusOK {
		t.Fatalf("readmit = %d, want 200", r.StatusCode)
	}
	n, err := h.store.NodeByID(ctx, gone.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if n.Status != store.NodeStatusPending {
		t.Fatalf("after readmit status = %v, want pending", n.Status)
	}

	// Stamp a fresh heartbeat and drive the real promotion sweep: pending -> ready.
	// PromoteHealthyNodes promotes pending/unreachable nodes whose heartbeat is at
	// or after the freshAfter cutoff, so a cutoff just in the past qualifies the
	// heartbeat we just stamped.
	stampFreshHeartbeat(t, h, gone.ID)
	freshAfter := time.Now().UTC().Add(-time.Minute)
	if _, err := h.store.PromoteHealthyNodes(ctx, freshAfter); err != nil {
		t.Fatalf("PromoteHealthyNodes: %v", err)
	}
	promoted, err := h.store.NodeByID(ctx, gone.ID)
	if err != nil {
		t.Fatalf("NodeByID after promote: %v", err)
	}
	if promoted.Status != store.NodeStatusReady {
		t.Errorf("after promotion status = %v, want ready", promoted.Status)
	}
}
