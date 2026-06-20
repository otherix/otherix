// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// spyStagingSweep records the maxAge values it is called with and returns a
// canned result.
type spyStagingSweep struct {
	mu      sync.Mutex
	calls   []time.Duration
	removed int
	err     error
}

func (s *spyStagingSweep) sweep(maxAge time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, maxAge)
	return s.removed, s.err
}

func (s *spyStagingSweep) seen() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.calls))
	copy(out, s.calls)
	return out
}

// TestSnapshotStagingSweeperBootSweepUsesZeroMaxAge asserts BootSweep clears all
// staging (maxAge 0).
func TestSnapshotStagingSweeperBootSweepUsesZeroMaxAge(t *testing.T) {
	spy := &spyStagingSweep{removed: 2}
	sw := newSnapshotStagingSweeper(spy.sweep, sweeperDiscardLogger())
	sw.BootSweep(context.Background())

	calls := spy.seen()
	if len(calls) != 1 || calls[0] != 0 {
		t.Errorf("BootSweep calls = %v, want one call with maxAge 0", calls)
	}
}

// TestSnapshotStagingSweeperRunSparesFreshAndStopsOnCancel asserts Run uses the
// generous periodic max-age (not 0) so it spares fresh temps, and returns
// ctx.Err() on cancel.
func TestSnapshotStagingSweeperRunSparesFreshAndStopsOnCancel(t *testing.T) {
	spy := &spyStagingSweep{}
	sw := newSnapshotStagingSweeper(spy.sweep, sweeperDiscardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sw.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run on cancelled ctx = %v, want context.Canceled", err)
	}
	// Run performs the periodic pass on ticks only; a cancelled ctx returns
	// before any tick, so no immediate boot-style sweep fires.
	if calls := spy.seen(); len(calls) != 0 {
		t.Errorf("Run fired a sweep before its first tick: %v", calls)
	}
	// The periodic max-age must be positive so a live capture is spared.
	if snapshotStagingMaxAge <= 0 {
		t.Errorf("snapshotStagingMaxAge = %v, want positive", snapshotStagingMaxAge)
	}
}
