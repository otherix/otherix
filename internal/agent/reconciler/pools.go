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

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/vm"
)

// DefaultTickInterval pins the reconciler's periodic cadence. The
// 10 s default sits between the 30 s default heartbeat interval
// (which already drives most reconciles via the trigger channel) and
// the seconds-scale operator-perceived "couple of seconds after CLI" -
// short enough to feel responsive when a declared_pools payload
// races a ticker, long enough to keep reconciliation near-zero-cost
// on idle clusters.
const DefaultTickInterval = 10 * time.Second

// PoolManager is the narrow vm.Manager surface the reconciler needs.
// vm.Manager satisfies it structurally; tests pass a fake without
// importing the production package.
type PoolManager interface {
	AddPool(name, root string) error
	RemovePool(name string)
	ListPools() []vm.PoolView
}

// Pools is the per-resource reconciler for storage pools. Single
// instance per agent process; owned by the agent's server-level glue,
// which starts Run in a goroutine and supplies the manager.
//
// Implements heartbeat.ResponseHandler (HandleHeartbeatResponse) and
// heartbeat.PoolReporter (PoolReports). The sender wires the same
// reconciler instance to both seams.
type Pools struct {
	log     *slog.Logger
	manager PoolManager
	tick    time.Duration

	desired atomic.Pointer[[]heartbeat.DeclaredPool]
	trigger chan struct{}

	mu      sync.Mutex
	reports map[string]heartbeat.PoolReport
}

// NewPools builds the storage-pool reconciler. tick==0 falls back to
// DefaultTickInterval. Returns ErrNilManager when manager is nil so
// boot-time misconfiguration surfaces at construction, not at the
// first reconcile.
func NewPools(manager PoolManager, log *slog.Logger, tick time.Duration) (*Pools, error) {
	if manager == nil {
		return nil, ErrNilManager
	}
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	return &Pools{
		log:     log,
		manager: manager,
		tick:    tick,
		trigger: make(chan struct{}, 1),
		reports: map[string]heartbeat.PoolReport{},
	}, nil
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. The
// sender invokes this immediately after a successful POST returns,
// outside the reconciler's own goroutine. We copy the slice (the
// sender's struct may be reused) and nudge the trigger channel.
func (r *Pools) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	pools := append([]heartbeat.DeclaredPool(nil), resp.DeclaredPools...)
	r.desired.Store(&pools)
	select {
	case r.trigger <- struct{}{}:
	default:
		// Earlier nudge already queued — collapse into one reconcile.
	}
}

// PoolReports implements heartbeat.PoolReporter. The collector calls
// this on every tick to fill HeartbeatRequest.pools. The snapshot
// covers every pool the reconciler has observed; entries that the
// CP no longer declares stop appearing (pool gone), and new pools
// the reconciler has just processed appear with their fresh status.
func (r *Pools) PoolReports() []heartbeat.PoolReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		return nil
	}
	out := make([]heartbeat.PoolReport, 0, len(r.reports))
	for _, rep := range r.reports {
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run blocks until ctx is cancelled. Ticks every r.tick OR on
// trigger; each tick runs one reconcile pass.
func (r *Pools) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	// Initial pass — in case the first heartbeat lands before this
	// loop boots. Cheap when there's nothing to do.
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.trigger:
			r.reconcile(ctx)
		}
	}
}

// reconcile is one pass over the (desired, observed) diff. Pure local
// effects (mkdir, manager.AddPool / RemovePool, reports map mutation).
// Errors are recorded in the reports map; they do not propagate.
func (r *Pools) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	desiredPtr := r.desired.Load()
	var desired []heartbeat.DeclaredPool
	if desiredPtr != nil {
		desired = *desiredPtr
	}

	desiredByName := make(map[string]heartbeat.DeclaredPool, len(desired))
	for _, d := range desired {
		desiredByName[d.Name] = d
	}

	observed := r.manager.ListPools()
	observedByName := make(map[string]vm.PoolView, len(observed))
	for _, o := range observed {
		observedByName[o.Name] = o
	}

	// Apply additions / re-validations. We attempt AddPool for every
	// declared pool — the function is idempotent on (name, root) and
	// inexpensive (a stat-after-mkdir per call). On root change the
	// manager replaces the entry; on failure we record `failed`.
	nextReports := make(map[string]heartbeat.PoolReport, len(desired))
	for _, d := range desired {
		err := r.manager.AddPool(d.Name, d.Path)
		if err != nil {
			msg := err.Error()
			nextReports[d.Name] = heartbeat.PoolReport{
				Name:                 d.Name,
				ReconciliationStatus: "failed",
				ReconciliationError:  &msg,
			}
			r.log.WarnContext(ctx, "pool reconcile failed",
				slog.String("pool", d.Name),
				slog.String("path", d.Path),
				slog.String("error", msg),
			)
			continue
		}
		nextReports[d.Name] = heartbeat.PoolReport{
			Name:                 d.Name,
			ReconciliationStatus: "ready",
		}
	}

	// Apply removals: pools the agent observes that the CP no longer
	// declares. The filesystem is left intact — only the registry
	// entry is dropped.
	for name := range observedByName {
		if _, declared := desiredByName[name]; declared {
			continue
		}
		r.manager.RemovePool(name)
		r.log.InfoContext(ctx, "pool unregistered (CP-side delete reconciled)",
			slog.String("pool", name),
		)
	}

	r.mu.Lock()
	r.reports = nextReports
	r.mu.Unlock()
}

// ErrNilManager guards against nil-injection at construction time.
// Returned by NewPools on a nil manager so the agent fails-fast at
// boot rather than panic on the first reconcile.
var ErrNilManager = errors.New("reconciler: PoolManager is required")
