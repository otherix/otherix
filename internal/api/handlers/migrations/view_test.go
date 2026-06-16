// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestToView_PopulatesStats(t *testing.T) {
	m := store.Migration{
		ID: uuid.New(), VmID: uuid.New(),
		Reason: store.MigrationReasonManual, Phase: store.MigrationPhaseCompleted,
		Stats: &store.MigrationStats{
			RAMTransferred: 10737418240, RAMTotal: 10737418240, RAMDirtyPagesRate: 7,
			DiskTransferred: 5368709120, DiskTotal: 5368709120,
			TotalTimeMs: 45125, DowntimeMs: 150, SetupTimeMs: 1200,
		},
	}
	v := toView(m)
	if v.Stats == nil {
		t.Fatalf("view.Stats = nil, want populated")
	}
	if v.Stats.RAM.Total != 10737418240 || v.Stats.RAM.DirtyPagesRate != 7 {
		t.Errorf("view ram = %+v", v.Stats.RAM)
	}
	if v.Stats.Disk.Total != 5368709120 {
		t.Errorf("view disk = %+v", v.Stats.Disk)
	}
	if v.Stats.TotalTimeMs != 45125 || v.Stats.DowntimeMs != 150 || v.Stats.SetupTimeMs != 1200 {
		t.Errorf("view timings = %d/%d/%d", v.Stats.TotalTimeMs, v.Stats.DowntimeMs, v.Stats.SetupTimeMs)
	}
}

func TestToView_NilStatsStaysNil(t *testing.T) {
	m := store.Migration{ID: uuid.New(), VmID: uuid.New(), Reason: store.MigrationReasonManual, Phase: store.MigrationPhasePending}
	if v := toView(m); v.Stats != nil {
		t.Errorf("view.Stats = %+v, want nil", v.Stats)
	}
}
