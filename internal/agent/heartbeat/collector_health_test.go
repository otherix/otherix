// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
)

// healthReporterStub returns a fixed HealthCheckReport slice so the collector
// fold can be exercised without the reconciler package.
type healthReporterStub struct{ reports []HealthCheckReport }

func (s healthReporterStub) HealthCheckReports() []HealthCheckReport { return s.reports }

// TestCollect_IncludesHealthChecks confirms the collector folds the health
// reporter's verdicts into Report.HealthChecks. Mirrors TestCollect_IncludesBlobs:
// the LinuxCollector is built directly (bypassing the GOOS gate in NewLinux) so
// the fold is exercised on every host.
func TestCollect_IncludesHealthChecks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	want := []HealthCheckReport{
		{LBID: uuid.New(), VMID: uuid.New(), Healthy: true},
		{LBID: uuid.New(), VMID: uuid.New(), Healthy: false},
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		healthChecks: healthReporterStub{reports: want},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if diff := cmp.Diff(want, rep.HealthChecks); diff != "" {
		t.Errorf("HealthChecks mismatch (-want +got):\n%s", diff)
	}
}
