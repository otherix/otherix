// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// healthProbeTick is the health reconciler's base cadence. Unlike the
// storage-pool reconciler's 10 s DefaultTickInterval, the prober ticks
// every second so a per-target interval_seconds as low as 1 is honored;
// each target still carries its own nextProbeAt and is probed only when
// due, so a longer interval costs nothing extra between due passes.
const healthProbeTick = time.Second

// healthProbeConcurrency bounds the number of in-flight probes in a single
// due pass. Probes run in parallel behind this fixed-size semaphore so one
// hung dial cannot block the others and a burst of due targets cannot
// stretch the effective interval past the CP-side staleness window.
const healthProbeConcurrency = 16

// HealthProbe performs one TCP-connect probe of vmName's port with a timeout.
// The result is tri-state: probed reports whether a real TCP dial was attempted;
// healthy carries the dial's outcome and is meaningful only when probed is true.
// probed=false means the target could not be probed (VM not found / not running /
// no managed-DHCP lease) and MUST leave hysteresis untouched so a valid backend
// with no lease stays warming rather than being darkened. The production adapter
// binds the dial to the guest bridge and resolves the IP from local NIC/lease
// state; tests inject a stub.
type HealthProbe interface {
	Probe(ctx context.Context, vmName string, port int32, timeout time.Duration) (healthy, probed bool)
}

// healthTargetState is the per-(lb, vm) hysteresis state. A target is
// WARMING (verdictKnown=false) until consecutiveOK first reaches
// healthy_threshold or consecutiveFail first reaches unhealthy_threshold;
// after that it holds its last healthy value between flips. Warming targets
// are omitted from HealthCheckReports so a fresh backend with no CP record
// never darkens its load balancer.
type healthTargetState struct {
	target          heartbeat.DeclaredHealthCheck
	consecutiveOK   int32
	consecutiveFail int32
	healthy         bool
	verdictKnown    bool
	nextProbeAt     time.Time
}

// Health is the per-resource reconciler for active L4 load-balancer backend
// health checks (ADR-0027 down-channel declared_health_checks, up-channel
// health_checks). Single instance per agent process; owned by the agent's
// server-level glue, which starts Run in a goroutine and injects the probe.
//
// Implements heartbeat.ResponseHandler (HandleHeartbeatResponse) and the
// health reporter seam (HealthCheckReports). The sender wires the same
// reconciler instance to both.
type Health struct {
	log   *slog.Logger
	probe HealthProbe
	tick  time.Duration
	now   func() time.Time

	desired atomic.Pointer[[]heartbeat.DeclaredHealthCheck]
	trigger chan struct{}

	mu    sync.Mutex
	state map[string]*healthTargetState // key = lbID+"/"+vmID
}

// NewHealth builds the health reconciler. tick<=0 falls back to
// healthProbeTick. Returns ErrNilProbe when probe is nil so boot-time
// misconfiguration surfaces at construction, not at the first probe pass.
func NewHealth(probe HealthProbe, log *slog.Logger, tick time.Duration) (*Health, error) {
	if probe == nil {
		return nil, ErrNilProbe
	}
	if tick <= 0 {
		tick = healthProbeTick
	}
	return &Health{
		log:     log,
		probe:   probe,
		tick:    tick,
		now:     time.Now,
		trigger: make(chan struct{}, 1),
		state:   map[string]*healthTargetState{},
	}, nil
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. The sender
// invokes this immediately after a successful POST returns, outside the
// reconciler's own goroutine. We copy the slice (the sender's struct may be
// reused) and nudge the trigger channel.
func (h *Health) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	checks := append([]heartbeat.DeclaredHealthCheck(nil), resp.DeclaredHealthChecks...)
	h.desired.Store(&checks)
	select {
	case h.trigger <- struct{}{}:
	default:
		// Earlier nudge already queued - collapse into one pass.
	}
}

// HealthCheckReports implements the health reporter seam. The collector
// calls this on every tick to fill HeartbeatRequest.health_checks. Only
// targets with a settled verdict (verdictKnown) are emitted; warming
// targets are omitted so the CP degrades them to phase==running rather than
// darkening a fresh or post-restart load balancer. Sorted by (lbID, vmID).
func (h *Health) HealthCheckReports() []heartbeat.HealthCheckReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.state) == 0 {
		return nil
	}
	out := make([]heartbeat.HealthCheckReport, 0, len(h.state))
	for _, s := range h.state {
		if !s.verdictKnown {
			continue
		}
		out = append(out, heartbeat.HealthCheckReport{
			LBID:    s.target.LBID,
			VMID:    s.target.VMID,
			Healthy: s.healthy,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LBID != out[j].LBID {
			return out[i].LBID.String() < out[j].LBID.String()
		}
		return out[i].VMID.String() < out[j].VMID.String()
	})
	return out
}

// Run blocks until ctx is cancelled. Ticks every h.tick OR on trigger; each
// tick runs one probeDue pass.
func (h *Health) Run(ctx context.Context) error {
	ticker := time.NewTicker(h.tick)
	defer ticker.Stop()
	// Initial pass - in case the first heartbeat lands before this loop
	// boots. Cheap when there's nothing due.
	h.probeDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			h.probeDue(ctx)
		case <-h.trigger:
			h.probeDue(ctx)
		}
	}
}

// probeDue syncs the in-memory target set to the latest declared set, probes
// every target whose nextProbeAt has arrived (in parallel behind a bounded
// semaphore), then applies hysteresis to each result under mu. Factored out
// of Run so tests drive one pass deterministically. Undeclared targets are
// dropped here (full-snapshot semantics).
func (h *Health) probeDue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := h.now()
	due := h.syncTargets(now)
	if len(due) == 0 {
		return
	}

	results := make([]probeResult, len(due))
	sem := make(chan struct{}, healthProbeConcurrency)
	var wg sync.WaitGroup
	for i := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, t heartbeat.DeclaredHealthCheck) {
			defer wg.Done()
			defer func() { <-sem }()
			timeout := time.Duration(t.TimeoutSeconds) * time.Second
			healthy, probed := h.probe.Probe(ctx, t.VMName, t.Port, timeout)
			results[idx] = probeResult{healthy: healthy, probed: probed}
		}(i, due[i].target)
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	for i, d := range due {
		key := targetKey(d.target.LBID, d.target.VMID)
		s := h.state[key]
		if s == nil {
			// Target was dropped by a concurrent re-declaration between the
			// syncTargets snapshot and here; discard its result.
			continue
		}
		// Only a real dial feeds hysteresis. An unprobed target (no lease / not
		// running) holds its counters and verdict untouched - a warming target
		// stays warming, a settled target keeps its last verdict - and is merely
		// retried next interval so a never-leased backend is never darkened.
		if results[i].probed {
			applyHysteresis(s, results[i].healthy)
		}
		interval := time.Duration(s.target.IntervalSeconds) * time.Second
		if interval <= 0 {
			interval = h.tick
		}
		s.nextProbeAt = now.Add(interval)
	}
}

// probeResult is one target's tri-state probe outcome for a due pass: healthy is
// meaningful only when probed is true (a real TCP dial completed).
type probeResult struct {
	healthy bool
	probed  bool
}

// syncTargets reconciles the in-memory state map to the latest declared set: new
// targets are inserted (due immediately, nextProbeAt=now), targets no longer
// declared are dropped, and existing targets have their declared parameters
// refreshed. It returns a snapshot of the targets due at now (nextProbeAt <=
// now) for the caller to probe outside the lock.
func (h *Health) syncTargets(now time.Time) []healthTargetState {
	desiredPtr := h.desired.Load()
	var desired []heartbeat.DeclaredHealthCheck
	if desiredPtr != nil {
		desired = *desiredPtr
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	declared := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		key := targetKey(d.LBID, d.VMID)
		declared[key] = struct{}{}
		s := h.state[key]
		if s == nil {
			h.state[key] = &healthTargetState{target: d, nextProbeAt: now}
			continue
		}
		// Refresh declared parameters; hysteresis counters and verdict carry.
		s.target = d
	}
	for key := range h.state {
		if _, ok := declared[key]; !ok {
			delete(h.state, key)
		}
	}

	var due []healthTargetState
	for _, s := range h.state {
		if !s.nextProbeAt.After(now) {
			due = append(due, *s)
		}
	}
	return due
}

// applyHysteresis folds one probe result into a target's counters and, once a
// threshold is crossed, its settled verdict. On success consecutiveOK grows
// and consecutiveFail resets; crossing healthy_threshold sets healthy=true.
// On failure the mirror applies against unhealthy_threshold. Before the first
// threshold crossing the target stays WARMING (verdictKnown=false).
func applyHysteresis(s *healthTargetState, ok bool) {
	if ok {
		s.consecutiveOK++
		s.consecutiveFail = 0
		if s.consecutiveOK >= s.target.HealthyThreshold {
			s.healthy = true
			s.verdictKnown = true
		}
		return
	}
	s.consecutiveFail++
	s.consecutiveOK = 0
	if s.consecutiveFail >= s.target.UnhealthyThreshold {
		s.healthy = false
		s.verdictKnown = true
	}
}

// targetKey is the state-map key for a probe target: lbID+"/"+vmID.
func targetKey(lb, vm uuid.UUID) string {
	return lb.String() + "/" + vm.String()
}

// ErrNilProbe guards against nil-injection at construction time. Returned by
// NewHealth on a nil probe so the agent fails-fast at boot rather than panic
// on the first probe pass.
var ErrNilProbe = errors.New("reconciler: HealthProbe is required")
