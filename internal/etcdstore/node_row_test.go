// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestCasNodeUpdateRetriesOnStaleModRevision proves the retry mechanism has
// teeth: a competing writer that lands between casNodeUpdate's read and its
// commit (here a gateway-role toggle bumping the row's ModRevision) must force
// the CAS to miss, drive a fresh re-read, and leave BOTH writes on the row - the
// capability field AND the gateway bit. With a blind put the toggle would be
// clobbered by the stale snapshot.
func TestCasNodeUpdateRetriesOnStaleModRevision(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()

	created, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    "cas-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node.example:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	// Inject a competing writer between casNodeUpdate's read and its commit: on
	// the first attempt only, toggle the gateway role. That bumps the row's
	// ModRevision, so the pending CAS misses and casNodeUpdate re-reads on the
	// next attempt (the idempotent toggle is a no-op the second time).
	version := "v-cas"
	var attempts int
	out, err := s.casNodeUpdate(ctx, created.ID, func(n *store.Node) {
		attempts++
		if attempts == 1 {
			if _, terr := s.SetNodeGatewayRole(ctx, created.ID, true); terr != nil {
				t.Fatalf("competing SetNodeGatewayRole: %v", terr)
			}
		}
		n.AgentVersion = &version
	})
	if err != nil {
		t.Fatalf("casNodeUpdate: %v", err)
	}
	if attempts < 2 {
		t.Errorf("casNodeUpdate did not retry: attempts = %d, want >= 2", attempts)
	}
	if out.AgentVersion == nil || *out.AgentVersion != version {
		t.Errorf("returned AgentVersion = %v, want %q", out.AgentVersion, version)
	}
	if !out.HasRole(store.NodeRoleGateway) {
		t.Errorf("returned row dropped GatewayRole: HasRole(gateway) = false, want true")
	}

	// Re-read from the store: both writes must be durably persisted.
	got, err := s.NodeByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.AgentVersion == nil || *got.AgentVersion != version {
		t.Errorf("persisted AgentVersion = %v, want %q", got.AgentVersion, version)
	}
	if !got.HasRole(store.NodeRoleGateway) {
		t.Errorf("persisted row dropped GatewayRole: HasRole(gateway) = false, want true")
	}
}
