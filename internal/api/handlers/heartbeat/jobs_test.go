// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

type stubReconciler struct {
	promoted    []store.PromoteHealthyNodesRow
	demoted     []store.MarkNodesUnreachableRow
	promoteErr  error
	demoteErr   error
	promoteCall int
	demoteCall  int
	freshArg    time.Time
	staleArg    time.Time
}

func (s *stubReconciler) PromoteHealthyNodes(_ context.Context, freshAfter time.Time) ([]store.PromoteHealthyNodesRow, error) {
	s.promoteCall++
	s.freshArg = freshAfter
	return s.promoted, s.promoteErr
}

func (s *stubReconciler) MarkNodesUnreachable(_ context.Context, staleBefore time.Time) ([]store.MarkNodesUnreachableRow, error) {
	s.demoteCall++
	s.staleArg = staleBefore
	return s.demoted, s.demoteErr
}

// TestReconcileWorker_RunsBothPasses confirms one tick exercises both
// promotion and demotion. Identical cutoff (`freshAfter == staleBefore
// == now - StaleThreshold`) is a deliberate property of the SQL —
// "fresh" and "stale" are complements on the same time axis.
func TestReconcileWorker_RunsBothPasses(t *testing.T) {
	rec := &stubReconciler{
		promoted: []store.PromoteHealthyNodesRow{{ID: uuid.New(), Name: "n1", Status: store.NodeStatusReady}},
		demoted:  []store.MarkNodesUnreachableRow{{ID: uuid.New(), Name: "n2"}},
	}
	w := &ReconcileWorker{
		reconciler: rec,
		cfg:        ReconcileConfig{StaleThreshold: 60 * time.Second, Interval: 30 * time.Second},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := w.Work(context.Background(), &river.Job[ReconcileArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if rec.promoteCall != 1 {
		t.Errorf("PromoteHealthyNodes called %d times, want 1", rec.promoteCall)
	}
	if rec.demoteCall != 1 {
		t.Errorf("MarkNodesUnreachable called %d times, want 1", rec.demoteCall)
	}
	if !rec.freshArg.Equal(rec.staleArg) {
		t.Errorf("freshAfter (%v) and staleBefore (%v) should be equal", rec.freshArg, rec.staleArg)
	}
	if delta := time.Since(rec.freshArg); delta < 55*time.Second || delta > 65*time.Second {
		t.Errorf("freshAfter offset = %v, want ~60s", delta)
	}
}

// TestReconcileWorker_PropagatesPromoteError confirms a failed
// promote pass aborts the tick rather than masking the error and
// silently continuing to demote.
func TestReconcileWorker_PropagatesPromoteError(t *testing.T) {
	rec := &stubReconciler{promoteErr: errors.New("db down")}
	w := &ReconcileWorker{
		reconciler: rec,
		cfg:        ReconcileConfig{StaleThreshold: 60 * time.Second},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := w.Work(context.Background(), &river.Job[ReconcileArgs]{})
	if err == nil {
		t.Fatal("expected error from failed promote")
	}
	if rec.demoteCall != 0 {
		t.Errorf("MarkNodesUnreachable should not run when promote failed, called %d times", rec.demoteCall)
	}
}

// TestReconcileConfig_Defaults pins the fallback values applied by
// withDefaults. Changing these affects every test that ships with a
// zero config; reviewers must consciously update.
func TestReconcileConfig_Defaults(t *testing.T) {
	cfg := ReconcileConfig{}.withDefaults()
	if cfg.StaleThreshold != defaultStaleThreshold {
		t.Errorf("StaleThreshold default = %v, want %v", cfg.StaleThreshold, defaultStaleThreshold)
	}
	if cfg.Interval != defaultInterval {
		t.Errorf("Interval default = %v, want %v", cfg.Interval, defaultInterval)
	}

	// Non-zero overrides survive.
	override := ReconcileConfig{StaleThreshold: 5 * time.Second, Interval: 7 * time.Second}.withDefaults()
	if override.StaleThreshold != 5*time.Second || override.Interval != 7*time.Second {
		t.Errorf("withDefaults clobbered explicit values: %+v", override)
	}
}
