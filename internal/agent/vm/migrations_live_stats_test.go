// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"encoding/json"
	"testing"

	"github.com/otherix/otherix/internal/agent/qemu"
)

func TestBuildLiveMigrationStats_Shape(t *testing.T) {
	var info qemu.MigrateInfo
	info.Status = "completed"
	info.RAM.Transferred = 10737418240
	info.RAM.Total = 10737418240
	info.RAM.DirtyPagesRate = 7
	info.TotalTimeMs = 45125
	info.DowntimeMs = 150
	info.SetupTimeMs = 1200

	stats := buildLiveMigrationStats(info, 5368709120, 5368709121)

	b, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("Marshal(stats) error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal stats error: %v", err)
	}
	ram, ok := got["ram"].(map[string]any)
	if !ok {
		t.Fatalf("stats.ram missing/!object: %v", got["ram"])
	}
	if ram["transferred"].(float64) != 10737418240 {
		t.Errorf("ram.transferred = %v, want 10737418240", ram["transferred"])
	}
	if ram["dirty_pages_rate"].(float64) != 7 {
		t.Errorf("ram.dirty_pages_rate = %v, want 7", ram["dirty_pages_rate"])
	}
	disk, ok := got["disk"].(map[string]any)
	if !ok {
		t.Fatalf("stats.disk missing/!object: %v", got["disk"])
	}
	if disk["transferred"].(float64) != 5368709120 {
		t.Errorf("disk.transferred = %v, want 5368709120", disk["transferred"])
	}
	if disk["total"].(float64) != 5368709121 {
		t.Errorf("disk.total = %v, want 5368709121", disk["total"])
	}
	if got["total_time_ms"].(float64) != 45125 {
		t.Errorf("total_time_ms = %v, want 45125", got["total_time_ms"])
	}
	if got["downtime_ms"].(float64) != 150 {
		t.Errorf("downtime_ms = %v, want 150", got["downtime_ms"])
	}
	if got["setup_time_ms"].(float64) != 1200 {
		t.Errorf("setup_time_ms = %v, want 1200", got["setup_time_ms"])
	}
}

func TestPeakDiskBytes_IndependentMaxima(t *testing.T) {
	ticks := [][]qemu.BlockJobInfo{
		{{Type: "mirror", Len: 100, Offset: 30}},
		{{Type: "mirror", Len: 100, Offset: 90}},
		{},
	}
	var total, transferred int64
	for _, jobs := range ticks {
		total, transferred = peakDiskBytes(total, transferred, jobs)
	}
	if total != 100 {
		t.Errorf("total = %d, want 100", total)
	}
	if transferred != 90 {
		t.Errorf("transferred = %d, want 90", transferred)
	}
}
