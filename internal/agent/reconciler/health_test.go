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

// stubProbeResult is one scripted tri-state probe outcome: healthy is meaningful
// only when probed is true (a real dial). probed=false models "could not probe"
// (no lease / VM not found / not running).
type stubProbeResult struct {
	healthy bool
	probed  bool
}

// dialed scripts a real TCP dial with the given health outcome (probed=true).
func dialed(healthy bool) stubProbeResult { return stubProbeResult{healthy: healthy, probed: true} }

// unprobed scripts a "could not probe" result (probed=false, e.g. no lease).
func unprobed() stubProbeResult { return stubProbeResult{healthy: false, probed: false} }

type healthProbeStub struct {
	results []stubProbeResult
	i       int
}

func (p *healthProbeStub) Probe(_ context.Context, _ string, _ int32, _ time.Duration) (healthy, probed bool) {
	r := p.results[p.i%len(p.results)]
	p.i++
	return r.healthy, r.probed
}

func TestHealthHysteresisMarksHealthyAfterThreshold(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	h, err := NewHealth(&healthProbeStub{results: []stubProbeResult{dialed(true)}}, discardLogger(), time.Second)
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
	// never reported healthy=false (the warming-does-not-darken invariant).
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
	h, err := NewHealth(&healthProbeStub{results: []stubProbeResult{dialed(true), dialed(false), dialed(false)}}, discardLogger(), time.Second)
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
	// reported healthy=false (the warming-does-not-darken invariant on the
	// failing-from-cold path).
	h, err := NewHealth(&healthProbeStub{results: []stubProbeResult{dialed(false)}}, discardLogger(), time.Second)
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
	h, err := NewHealth(&healthProbeStub{results: []stubProbeResult{dialed(true)}}, discardLogger(), time.Second)
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

func TestHealthUnprobedTargetStaysWarming(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	// The probe can NEVER perform a real dial (no managed-DHCP lease / not
	// running): every pass returns probed=false. Such a target reports NO verdict
	// - it stays warming and is omitted forever, even well past unhealthy_threshold,
	// because a leaseless-but-running backend is valid and must not be darkened.
	h, err := NewHealth(&healthProbeStub{results: []stubProbeResult{unprobed()}}, discardLogger(), time.Second)
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

	// Drive many more passes than unhealthy_threshold. If probed=false were
	// treated as a dial-failure (the bug), consecutiveFail would cross the
	// threshold and flip the target to a definite healthy=false verdict.
	for i := 0; i < 6; i++ {
		h.probeDue(context.Background())
		if rep := healthReportFor(h, lb, vm); rep != nil {
			t.Fatalf("unprobed target after %d passes: report = %+v, want omitted (nil)", i+1, rep)
		}
		fixed = fixed.Add(2 * time.Second)
	}

	// Revert-to-confirm: an otherwise-identical target whose probe DID dial and
	// fail every pass DOES cross unhealthy_threshold and flips to healthy=false -
	// proving it is the probed=false result, not the target config, that holds
	// the unprobed target warming above.
	lb2, vm2 := uuid.New(), uuid.New()
	fixed2 := time.Now()
	h2, err := NewHealth(&healthProbeStub{results: []stubProbeResult{dialed(false)}}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewHealth() error = %v", err)
	}
	h2.now = func() time.Time { return fixed2 }
	h2.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredHealthChecks: []heartbeat.DeclaredHealthCheck{{
			LBID: lb2, VMID: vm2, VMName: "web", Port: 80,
			IntervalSeconds: 1, TimeoutSeconds: 1, HealthyThreshold: 2, UnhealthyThreshold: 3,
		}},
	})
	for i := 0; i < 3; i++ {
		h2.probeDue(context.Background())
		fixed2 = fixed2.Add(2 * time.Second)
	}
	if rep := healthReportFor(h2, lb2, vm2); rep == nil || rep.Healthy {
		t.Errorf("dialed-and-failed target after 3 passes: report = %+v, want healthy=false", rep)
	}
}

func TestHealthDefiniteVerdictHeldAcrossUnprobed(t *testing.T) {
	lb, vm := uuid.New(), uuid.New()
	fixed := time.Now()
	// One real successful dial settles a definite healthy=true verdict; every
	// subsequent pass is unprobed (lease lost / warmup). A settled target HOLDS
	// its last verdict across unprobed passes - it does not drop back to warming
	// or unhealthy.
	h, err := NewHealth(&healthProbeStub{results: []stubProbeResult{dialed(true), unprobed()}}, discardLogger(), time.Second)
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

	h.probeDue(context.Background()) // real success -> healthy=true settled
	if rep := healthReportFor(h, lb, vm); rep == nil || !rep.Healthy {
		t.Fatalf("after 1 success: report = %+v, want healthy=true", rep)
	}
	// Now unprobed forever: the verdict must hold healthy=true.
	for i := 0; i < 5; i++ {
		fixed = fixed.Add(2 * time.Second)
		h.probeDue(context.Background())
		if rep := healthReportFor(h, lb, vm); rep == nil || !rep.Healthy {
			t.Fatalf("unprobed pass %d after healthy: report = %+v, want held healthy=true", i+1, rep)
		}
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
