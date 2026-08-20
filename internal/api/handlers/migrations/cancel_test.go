// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/migrations"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// recordingCancelClient is a MigrationCancelClient double. It records every
// CancelMigration call so the propagation tests can assert which endpoints were
// told to abort, and can stage an error to prove the handler never fails on a
// propagation failure.
type recordingCancelClient struct {
	mu    sync.Mutex
	calls []cancelCall
	err   error
}

func (c *recordingCancelClient) CancelMigration(_ context.Context, endpoint, vmName, migrationID string) (agentapi.Migration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, cancelCall{endpoint, vmName, migrationID})
	return agentapi.Migration{}, c.err
}

func (c *recordingCancelClient) recorded() []cancelCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]cancelCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// newHandlerWithAgent builds a migrations.Handler over the real etcdstore with
// the given MigrationCancelClient seam.
func newHandlerWithAgent(s *etcdstore.Store, agent migrations.MigrationCancelClient) *migrations.Handler {
	return migrations.New(s, agent, discardLogger())
}

// seedBoundLiveMigration creates an explicit (bound) LIVE migration + backing
// task for vm with source/target pre-bound, returning the migration row.
func seedBoundLiveMigration(t *testing.T, s *etcdstore.Store, vmID, source, target uuid.UUID) store.Migration {
	t.Helper()
	src := source
	tgt := target
	taskID := uuid.New()
	migID := uuid.New()
	resID := migID
	p := store.CreateMigrationParams{
		ID:           migID,
		VmID:         vmID,
		SourceNodeID: &src,
		TargetNodeID: &tgt,
		Reason:       store.MigrationReasonManual,
		Live:         true,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.migrate", Status: store.TaskStatusPending,
			ResourceType: "migration", ResourceID: &resID, MaxAttempts: 25,
		},
	}
	m, err := s.CreateMigration(context.Background(), p, migrationJobArgsStub{TaskID: taskID, MigrationID: migID})
	if err != nil {
		t.Fatalf("CreateMigration(bound-live): %v", err)
	}
	return m
}

// TestMigrationCancel_PropagatesToSourceAndTarget pins the feature: a fresh
// cancel of a bound migration tells BOTH the source and the target agent to
// abort, with the right (vmName, migrationID). The migration is already
// cancelled in the store (fail-safe state) before the agents are ever contacted.
//
// Both modes propagate. An earlier revision skipped the target for offline
// migrations on the premise that an offline target has nothing to reap; that is
// false - it holds a qemu-nbd owning the destination disk's write lock, a
// reserved migration port, and a migration record nothing else ever makes
// terminal, which blocks the agent's tombstone teardown of that VM.
func TestMigrationCancel_PropagatesToSourceAndTarget(t *testing.T) {
	tests := []struct {
		name string
		live bool
	}{
		{name: "live", live: true},
		{name: "offline", live: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, cli := freshStore(t)
			const srcEndpoint = "https://node-a:9443"
			const tgtEndpoint = "https://node-b:9443"
			nodeA := seedReadyNode(t, s, "node-a", srcEndpoint)
			nodeB := seedReadyNode(t, s, "node-b", tgtEndpoint)
			owner := uuid.New()
			vm := ownedVM(t, cli, nodeA.ID, owner)
			m, _ := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", tc.live)

			agent := &recordingCancelClient{}
			h := newHandlerWithAgent(s, agent)
			rec := httptest.NewRecorder()
			h.Cancel(rec, cancelRequest(operator(owner), m.ID.String()))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}

			// Migration is cancelled (authoritative state set first).
			got, err := s.MigrationByID(context.Background(), m.ID)
			if err != nil {
				t.Fatalf("MigrationByID: %v", err)
			}
			if got.Phase != store.MigrationPhaseCancelled {
				t.Errorf("migration phase = %q, want cancelled", got.Phase)
			}

			calls := agent.recorded()
			if len(calls) != 2 {
				t.Fatalf("cancel calls = %v, want 2 (source + target)", calls)
			}
			byEndpoint := map[string]cancelCall{}
			for _, c := range calls {
				byEndpoint[c.endpoint] = c
			}
			// The worker dials each agent by its identity URL (DialURL(node.Name)).
			srcDial, tgtDial := agentclient.DialURL(nodeA.Name), agentclient.DialURL(nodeB.Name)
			src, ok := byEndpoint[srcDial]
			if !ok {
				t.Fatalf("no cancel call to source %q (calls=%v)", srcDial, calls)
			}
			if src.vmName != vm.Name || src.migID != m.ID.String() {
				t.Errorf("source cancel = %+v, want vmName %q migID %q", src, vm.Name, m.ID)
			}
			tgt, ok := byEndpoint[tgtDial]
			if !ok {
				t.Fatalf("no cancel call to target %q (calls=%v)", tgtDial, calls)
			}
			if tgt.vmName != vm.Name || tgt.migID != m.ID.String() {
				t.Errorf("target cancel = %+v, want vmName %q migID %q", tgt, vm.Name, m.ID)
			}
		})
	}
}

// TestMigrationCancel_AgentErrorDoesNotFailHandler is the teeth: a propagation
// error must NOT fail the cancel. The migration is already cancelled (the
// authoritative state); the agent abort is best-effort acceleration on top. Even
// when BOTH agent calls error, the handler still returns 200 with the cancelled
// migration.
func TestMigrationCancel_AgentErrorDoesNotFailHandler(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)
	m := seedBoundLiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	agent := &recordingCancelClient{err: errors.New("agent unreachable")}
	h := newHandlerWithAgent(s, agent)
	rec := httptest.NewRecorder()
	h.Cancel(rec, cancelRequest(operator(owner), m.ID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on agent failure (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Phase != string(store.MigrationPhaseCancelled) {
		t.Errorf("phase = %q, want cancelled (agent failure must not change outcome)", body.Phase)
	}
	got, err := s.MigrationByID(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("stored phase = %q, want cancelled (agent failure must not change outcome)", got.Phase)
	}
}

// TestMigrationCancel_TerminalDoesNotPropagate pins that the already-terminal
// idempotent-replay path does NOT contact the agent: re-cancelling an already
// cancelled migration returns the unchanged row, with nothing new to abort.
func TestMigrationCancel_TerminalDoesNotPropagate(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)
	m := seedBoundLiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	// Pre-cancel so the handler hits the already-terminal replay path.
	if _, err := s.CancelMigration(context.Background(), m.ID, "first cancel"); err != nil {
		t.Fatalf("pre-cancel: %v", err)
	}

	agent := &recordingCancelClient{}
	h := newHandlerWithAgent(s, agent)
	rec := httptest.NewRecorder()
	h.Cancel(rec, cancelRequest(operator(owner), m.ID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if calls := agent.recorded(); len(calls) != 0 {
		t.Errorf("cancel calls on already-terminal replay = %v, want 0 (nothing new to abort)", calls)
	}
}

// TestMigrationCancel_NilAgentStillCancels pins nil-safety: a handler
// constructed with a nil MigrationCancelClient still cancels (200) and does not
// panic - propagation is skipped.
func TestMigrationCancel_NilAgentStillCancels(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)
	m := seedBoundLiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	h := newHandlerWithAgent(s, nil)
	rec := httptest.NewRecorder()
	h.Cancel(rec, cancelRequest(operator(owner), m.ID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got, err := s.MigrationByID(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("phase = %q, want cancelled", got.Phase)
	}
}
