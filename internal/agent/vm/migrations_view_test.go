// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
)

// TestGetMigrationProjection pins the record->view projection branches:
// completed pins 100%, an in-flight record derives percent from bytes
// (div-by-zero guarded), optional fields become nil pointers when unset,
// and a target record with no PeerEndpoint falls back to its ListenEndpt.
func TestGetMigrationProjection(t *testing.T) {
	m := newTestManager(t)

	completedID := uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: completedID, VMID: uuid.New(), Role: migration.RoleSource,
		Mode: migration.ModeOffline, Phase: migration.PhaseCompleted,
		PeerEndpoint: "10.0.0.2:49152",
	})
	activeID := uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: activeID, VMID: uuid.New(), Role: migration.RoleSource,
		Mode: migration.ModeOffline, Phase: migration.PhaseActive,
		BytesTotal: 1000, BytesTransferred: 250,
	})
	targetID := uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: targetID, VMID: uuid.New(), Role: migration.RoleTarget,
		Mode: migration.ModeOffline, Phase: migration.PhaseSetup,
		ListenEndpt: "10.0.0.3:49160",
	})
	failedID := uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: failedID, VMID: uuid.New(), Role: migration.RoleSource,
		Mode: migration.ModeOffline, Phase: migration.PhaseFailed,
		ErrorMessage: "tls handshake failed",
	})

	t.Run("completed pins 100 percent, nil BytesTotal", func(t *testing.T) {
		v, ok := m.GetMigration(completedID)
		if !ok {
			t.Fatal("GetMigration(completed) not found")
		}
		if v.ProgressPercent != 100 {
			t.Errorf("ProgressPercent = %d, want 100", v.ProgressPercent)
		}
		if v.BytesTotal != nil {
			t.Errorf("BytesTotal = %v, want nil (no total recorded)", *v.BytesTotal)
		}
		if v.PeerEndpoint == nil || *v.PeerEndpoint != "10.0.0.2:49152" {
			t.Errorf("PeerEndpoint = %v, want 10.0.0.2:49152", v.PeerEndpoint)
		}
	})

	t.Run("active derives percent from bytes", func(t *testing.T) {
		v, _ := m.GetMigration(activeID)
		if v.ProgressPercent != 25 {
			t.Errorf("ProgressPercent = %d, want 25", v.ProgressPercent)
		}
		if v.BytesTotal == nil || *v.BytesTotal != 1000 {
			t.Errorf("BytesTotal = %v, want 1000", v.BytesTotal)
		}
	})

	t.Run("target falls back to ListenEndpt", func(t *testing.T) {
		v, _ := m.GetMigration(targetID)
		if v.PeerEndpoint == nil || *v.PeerEndpoint != "10.0.0.3:49160" {
			t.Errorf("PeerEndpoint = %v, want fallback 10.0.0.3:49160", v.PeerEndpoint)
		}
	})

	t.Run("failed surfaces error message", func(t *testing.T) {
		v, _ := m.GetMigration(failedID)
		if v.ErrorMessage == nil || *v.ErrorMessage != "tls handshake failed" {
			t.Errorf("ErrorMessage = %v, want set", v.ErrorMessage)
		}
	})

	t.Run("missing returns not-found", func(t *testing.T) {
		if _, ok := m.GetMigration(uuid.New()); ok {
			t.Errorf("GetMigration(missing) ok = true, want false")
		}
	})
}
