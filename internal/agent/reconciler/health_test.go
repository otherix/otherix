// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

type healthProbeStub struct {
	results []bool
	i       int
}

func (p *healthProbeStub) Probe(_ context.Context, _ string, _ int32, _ time.Duration) bool {
	r := p.results[p.i%len(p.results)]
	p.i++
	return r
}

func TestHealthHysteresisMarksHealthyAfterThreshold(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	h, err := NewHealth(&healthProbeStub{results: []bool{true}}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewHealth() error = %v", err)
	}
	h.now = func() time.Time { return fixed }
	h.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredHealthChecks: []heartbeat.DeclaredHealthCheck{{
			LBID: lb, VMID: vm, VMName: "web", Port: 80,
			IntervalSeconds: 1, TimeoutSeconds: 1, HealthyThreshold: 2, UnhealthyThreshold: 3,
		}},
	})

	// One probe pass: still warming (need 2 successes) -> OMITTED from the report,
	// never reported healthy=false (the H1 warming-does-not-darken invariant).
	h.probeDue(context.Background())
	if rep := healthReportFor(h, lb, vm); rep != nil {
		t.Errorf("after 1 success (warming): report = %+v, want omitted (nil)", rep)
	}
	// Advance so the target is due again, second success crosses the threshold.
	fixed = fixed.Add(2 * time.Second)
	h.probeDue(context.Background())
	if rep := healthReportFor(h, lb, vm); rep == nil || !rep.Healthy {
		t.Errorf("after 2 successes: report = %+v, want healthy=true", rep)
	}
}

func TestHealthHysteresisMarksUnhealthyAfterThreshold(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	// One success reaches healthy (threshold 1), then two consecutive failures
	// (unhealthy_threshold 2) flip the reported verdict to unhealthy.
	h, err := NewHealth(&healthProbeStub{results: []bool{true, false, false}}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewHealth() error = %v", err)
	}
	h.now = func() time.Time { return fixed }
	h.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredHealthChecks: []heartbeat.DeclaredHealthCheck{{
			LBID: lb, VMID: vm, VMName: "web", Port: 80,
			IntervalSeconds: 1, TimeoutSeconds: 1, HealthyThreshold: 1, UnhealthyThreshold: 2,
		}},
	})

	h.probeDue(context.Background()) // success -> healthy=true
	if rep := healthReportFor(h, lb, vm); rep == nil || !rep.Healthy {
		t.Fatalf("after 1 success: report = %+v, want healthy=true", rep)
	}
	fixed = fixed.Add(2 * time.Second)
	h.probeDue(context.Background()) // 1st failure, below unhealthy_threshold -> holds healthy
	if rep := healthReportFor(h, lb, vm); rep == nil || !rep.Healthy {
		t.Errorf("after 1 failure (below threshold): report = %+v, want still healthy=true", rep)
	}
	fixed = fixed.Add(2 * time.Second)
	h.probeDue(context.Background()) // 2nd failure crosses unhealthy_threshold -> unhealthy
	if rep := healthReportFor(h, lb, vm); rep == nil || rep.Healthy {
		t.Errorf("after 2 failures: report = %+v, want healthy=false", rep)
	}
}

func TestHealthWarmingTargetOmittedUntilThreshold(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	// A target that has only failed once - below unhealthy_threshold - and has
	// never been healthy is still WARMING: it is omitted from the report, NEVER
	// reported healthy=false (the H1 invariant on the failing-from-cold path).
	h, err := NewHealth(&healthProbeStub{results: []bool{false}}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewHealth() error = %v", err)
	}
	h.now = func() time.Time { return fixed }
	h.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredHealthChecks: []heartbeat.DeclaredHealthCheck{{
			LBID: lb, VMID: vm, VMName: "web", Port: 80,
			IntervalSeconds: 1, TimeoutSeconds: 1, HealthyThreshold: 2, UnhealthyThreshold: 3,
		}},
	})

	h.probeDue(context.Background()) // 1 failure, below unhealthy_threshold 3
	if rep := healthReportFor(h, lb, vm); rep != nil {
		t.Errorf("warming target after 1 failure: report = %+v, want omitted (nil)", rep)
	}
}

func TestHealthDropsUndeclaredTargets(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	h, err := NewHealth(&healthProbeStub{results: []bool{true}}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewHealth() error = %v", err)
	}
	h.now = func() time.Time { return fixed }
	h.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredHealthChecks: []heartbeat.DeclaredHealthCheck{{
			LBID: lb, VMID: vm, VMName: "web", Port: 80,
			IntervalSeconds: 1, TimeoutSeconds: 1, HealthyThreshold: 1, UnhealthyThreshold: 2,
		}},
	})
	h.probeDue(context.Background())
	if rep := healthReportFor(h, lb, vm); rep == nil || !rep.Healthy {
		t.Fatalf("after 1 success: report = %+v, want healthy=true", rep)
	}

	// Re-declare an empty set: the undeclared target is dropped and the report
	// empties out (full-snapshot semantics).
	h.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredHealthChecks: []heartbeat.DeclaredHealthCheck{},
	})
	h.probeDue(context.Background())
	if reps := h.HealthCheckReports(); len(reps) != 0 {
		t.Errorf("after re-declaring empty: reports = %+v, want empty", reps)
	}
}

func healthReportFor(h *Health, lb, vm uuid.UUID) *heartbeat.HealthCheckReport {
	for _, r := range h.HealthCheckReports() {
		if r.LBID == lb && r.VMID == vm {
			return &r
		}
	}
	return nil
}
