// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func waitPhase(t *testing.T, m *Manager, id uuid.UUID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := m.Migrations().Get(id); ok && string(rec.Phase) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, _ := m.Migrations().Get(id)
	t.Fatalf("phase never reached %q; last = %q", want, rec.Phase)
}

func TestStartOutgoingPushesAndCompletes(t *testing.T) {
	m := newTestManager(t)
	v := m.seedStoppedVM(t, "demo")

	var convertArgs []string
	m.migRunConvert = func(ctx context.Context, args []string) error {
		convertArgs = args
		return nil
	}

	migID := uuid.New()
	task, err := m.StartOutgoing(context.Background(), OutgoingSpec{
		MigrationID:    migID,
		VMUUID:         v.ID,
		VMName:         v.Name,
		Mode:           "offline",
		TargetEndpoint: "10.0.0.2:49152",
		TargetIdentity: "node-tgt.agents.otherix.local",
		AuthToken:      migID.String(),
	})
	if err != nil {
		t.Fatalf("StartOutgoing() error = %v", err)
	}
	if task.Kind != TaskKindVMMigrate {
		t.Errorf("task.Kind = %v, want vm.migrate", task.Kind)
	}

	waitPhase(t, m, migID, "completed")
	if !argsContain(convertArgs, "--target-image-opts") {
		t.Errorf("convert not invoked as push: %v", convertArgs)
	}
}

// TestStartOutgoingIsIdempotentPerMigration pins the split-brain guard: the CP
// persists the agent_task_id only AFTER StartOutgoing returns, so a crash /
// redelivery in that window re-POSTs the outgoing start for the same migration.
// A second StartOutgoing must replay the ORIGINAL task, not mint a new one,
// Put-overwrite the source record, and spawn a second concurrent push against
// the same guest QMP. Without the guard task2 gets a fresh id, the record's
// AgentTaskID is overwritten, and convert runs twice - all three asserted.
func TestStartOutgoingIsIdempotentPerMigration(t *testing.T) {
	m := newTestManager(t)
	v := m.seedStoppedVM(t, "demo")

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var convertCalls int32
	m.migRunConvert = func(ctx context.Context, args []string) error {
		atomic.AddInt32(&convertCalls, 1)
		once.Do(func() { close(started) })
		<-release
		return nil
	}

	migID := uuid.New()
	spec := OutgoingSpec{
		MigrationID: migID, VMUUID: v.ID, VMName: v.Name, Mode: "offline",
		TargetEndpoint: "10.0.0.2:49152", TargetIdentity: "node-tgt.agents.otherix.local", AuthToken: migID.String(),
	}
	task1, err := m.StartOutgoing(context.Background(), spec)
	if err != nil {
		t.Fatalf("StartOutgoing #1 error = %v", err)
	}
	<-started // saga #1 is now pushing (convert blocked on release)

	task2, err := m.StartOutgoing(context.Background(), spec)
	if err != nil {
		t.Fatalf("StartOutgoing #2 (redelivery) error = %v", err)
	}
	if task2.ID != task1.ID {
		t.Errorf("redelivered StartOutgoing minted new task %s, want original %s (split-brain double-saga)",
			task2.ID, task1.ID)
	}
	rec, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatal("migration record missing after StartOutgoing")
	}
	if rec.AgentTaskID != task1.ID {
		t.Errorf("migration record AgentTaskID = %s, want %s (record overwritten by duplicate start)",
			rec.AgentTaskID, task1.ID)
	}

	close(release)
	waitPhase(t, m, migID, "completed")
	if n := atomic.LoadInt32(&convertCalls); n != 1 {
		t.Errorf("convert invoked %d times, want 1 (a duplicate saga was spawned)", n)
	}
}

func TestStartOutgoingFailClosedOnConvertError(t *testing.T) {
	m := newTestManager(t)
	v := m.seedStoppedVM(t, "demo")
	m.migRunConvert = func(ctx context.Context, args []string) error {
		return errors.New("tls handshake failed")
	}
	migID := uuid.New()
	if _, err := m.StartOutgoing(context.Background(), OutgoingSpec{
		MigrationID: migID, VMUUID: v.ID, VMName: v.Name, Mode: "offline",
		TargetEndpoint: "10.0.0.2:49152", TargetIdentity: "node-tgt.agents.otherix.local", AuthToken: migID.String(),
	}); err != nil {
		t.Fatalf("StartOutgoing() returned sync error = %v", err)
	}
	waitPhase(t, m, migID, "failed")
	rec, _ := m.Migrations().Get(migID)
	if rec.ErrorMessage == "" {
		t.Errorf("failed record has empty error message")
	}
}
