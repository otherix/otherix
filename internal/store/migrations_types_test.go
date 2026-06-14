// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestMigrationPhasePendingWire(t *testing.T) {
	if got := string(store.MigrationPhasePending); got != "pending" {
		t.Errorf("MigrationPhasePending = %q, want %q", got, "pending")
	}
}

func TestMigrationParamTypesConstruct(t *testing.T) {
	create := store.CreateMigrationParams{
		ID:             uuid.New(),
		VmID:           uuid.New(),
		TargetPoolName: "pool-a",
		Reason:         store.MigrationReasonManual,
		Live:           true,
		Task:           store.CreateTaskParams{ID: uuid.New()},
	}
	if create.TargetPoolName != "pool-a" {
		t.Errorf("CreateMigrationParams.TargetPoolName = %q, want %q", create.TargetPoolName, "pool-a")
	}
	if !create.Live {
		t.Errorf("CreateMigrationParams.Live = %v, want %v", create.Live, true)
	}

	var list store.ListMigrationsParams
	if list.LimitCount != 0 {
		t.Errorf("ListMigrationsParams zero value LimitCount = %d, want 0", list.LimitCount)
	}
	if list.CursorCreatedAt != nil || list.CursorID != nil || list.VmID != nil || list.NodeID != nil {
		t.Errorf("ListMigrationsParams zero value has non-nil pointer fields: %+v", list)
	}
}
