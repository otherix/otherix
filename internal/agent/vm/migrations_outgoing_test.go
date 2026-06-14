// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
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
