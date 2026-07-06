// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// livenessProjectionSpy fails an observed-report step (FilterExistingVMIDs) and
// records whether the liveness/capability write (UpdateNodeHeartbeat) ran. It
// embeds store.HeartbeatProjection (nil) so any unstubbed projection call
// panics - the desired failure mode for an unexpected step.
type livenessProjectionSpy struct {
	store.HeartbeatProjection
	nodeUpdateCalled bool
}

func (s *livenessProjectionSpy) NodeForHeartbeat(_ context.Context, id uuid.UUID) (store.GetNodeForHeartbeatRow, error) {
	return store.GetNodeForHeartbeatRow{ID: id, Architecture: store.CpuArchAmd64, Status: store.NodeStatusReady}, nil
}

func (s *livenessProjectionSpy) UpdateNodeMemoryPressure(context.Context, store.UpdateNodeMemoryPressureParams) error {
	return nil
}

func (s *livenessProjectionSpy) UpdateNodeSystemDiskPressure(context.Context, store.UpdateNodeSystemDiskPressureParams) error {
	return nil
}

func (s *livenessProjectionSpy) FilterExistingVMIDs(context.Context, []uuid.UUID) ([]uuid.UUID, error) {
	return nil, errors.New("simulated observed-report failure")
}

func (s *livenessProjectionSpy) UpdateNodeHeartbeat(context.Context, store.UpdateNodeHeartbeatParams) error {
	s.nodeUpdateCalled = true
	return nil
}

// livenessStoreFake hands the spy to project() through RunHeartbeatProjection.
type livenessStoreFake struct{ spy *livenessProjectionSpy }

func (f *livenessStoreFake) NodeByName(context.Context, string) (store.Node, error) {
	return store.Node{}, errors.New("unused")
}

func (f *livenessStoreFake) RunHeartbeatProjection(ctx context.Context, fn func(store.HeartbeatProjection) error) error {
	return fn(f.spy)
}

func (f *livenessStoreFake) ActiveSessionCA(context.Context) (store.SessionCA, error) {
	return store.SessionCA{}, errors.New("unused")
}

// TestProjectStampsLivenessLast proves last_heartbeat_at is stamped only after
// the observed/declared projection steps succeed. RunHeartbeatProjection is a
// sequence of independent writes, not one transaction, so if applyNodeUpdate
// (which writes last_heartbeat_at) ran first, a persistently-failing later step
// would keep refreshing liveness and the node would look alive forever while its
// observed state never landed. With applyNodeUpdate stamped LAST, an observed
// step failing must leave last_heartbeat_at untouched.
func TestProjectStampsLivenessLast(t *testing.T) {
	spy := &livenessProjectionSpy{}
	h := &Handler{
		store: &livenessStoreFake{spy: spy},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	agent := &auth.Agent{NodeID: uuid.New()}
	body := &requestBody{
		Architecture: string(store.CpuArchAmd64),
		// A Migration block makes resolveMigration self-contained (no NodeByID),
		// keeping the spy minimal; applyNodeUpdate is not reached anyway.
		Migration: &migrationCapability{Host: "10.0.0.1", PortRangeStart: 49152, PortRangeEnd: 49251},
		// A non-empty VM list drives applyVMs to FilterExistingVMIDs, the injected
		// failure point (the first observed-report write).
		VMs: []vmReport{{VMUUID: uuid.New(), Phase: "running"}},
	}

	if _, err := h.project(context.Background(), agent, body); err == nil {
		t.Fatalf("project() returned nil, want the injected observed-report failure")
	}
	if spy.nodeUpdateCalled {
		t.Errorf("UpdateNodeHeartbeat ran despite an observed-report failure; liveness must be stamped LAST")
	}
}
