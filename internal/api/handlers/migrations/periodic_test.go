// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/store"
)

func TestRetentionConfig_WithDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   RetentionConfig
		want RetentionConfig
	}{
		{
			name: "all zero - all defaulted",
			in:   RetentionConfig{},
			want: RetentionConfig{Completed: defaultCompletedRetention, Failed: defaultFailedRetention, Cancelled: defaultCancelledRetention},
		},
		{
			name: "completed set, rest zero - rest defaulted",
			in:   RetentionConfig{Completed: 1 * time.Hour},
			want: RetentionConfig{Completed: 1 * time.Hour, Failed: defaultFailedRetention, Cancelled: defaultCancelledRetention},
		},
		{
			name: "all set - pass through",
			in:   RetentionConfig{Completed: 3 * time.Minute, Failed: 4 * time.Hour, Cancelled: 5 * time.Hour},
			want: RetentionConfig{Completed: 3 * time.Minute, Failed: 4 * time.Hour, Cancelled: 5 * time.Hour},
		},
		{
			name: "negative treated as zero",
			in:   RetentionConfig{Cancelled: -1 * time.Second},
			want: RetentionConfig{Completed: defaultCompletedRetention, Failed: defaultFailedRetention, Cancelled: defaultCancelledRetention},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.withDefaults(); got != tc.want {
				t.Errorf("withDefaults() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// cleanupStoreSpy records the params CleanupFunc computes and the call count, and
// returns a canned deleted-count.
type cleanupStoreSpy struct {
	gotArg  store.DeleteExpiredMigrationsParams
	calls   int
	deleted int64
}

func (s *cleanupStoreSpy) DeleteExpiredMigrations(_ context.Context, arg store.DeleteExpiredMigrationsParams) (int64, error) {
	s.calls++
	s.gotArg = arg
	return s.deleted, nil
}

// TestCleanupFunc_ComputesCutoffsAndDeletes drives the periodic function through
// its real entry point and asserts it derives each per-state cutoff from now minus
// the configured window and calls the store exactly once.
func TestCleanupFunc_ComputesCutoffsAndDeletes(t *testing.T) {
	t.Parallel()

	spy := &cleanupStoreSpy{deleted: 3}
	rc := RetentionConfig{
		Completed: 2 * time.Hour,
		Failed:    6 * time.Hour,
		Cancelled: 9 * time.Hour,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	before := time.Now()
	if err := CleanupFunc(spy, rc, log)(context.Background()); err != nil {
		t.Fatalf("CleanupFunc()() = %v, want nil", err)
	}
	after := time.Now()

	if spy.calls != 1 {
		t.Fatalf("DeleteExpiredMigrations calls = %d, want 1", spy.calls)
	}
	// Each cutoff = now - window, with now between before and after.
	checkCutoff := func(name string, got time.Time, window time.Duration) {
		lo := before.Add(-window)
		hi := after.Add(-window)
		if got.Before(lo) || got.After(hi) {
			t.Errorf("%s cutoff = %v, want in [%v, %v]", name, got, lo, hi)
		}
	}
	checkCutoff("completed", spy.gotArg.CompletedCutoff, rc.Completed)
	checkCutoff("failed", spy.gotArg.FailedCutoff, rc.Failed)
	checkCutoff("cancelled", spy.gotArg.CancelledCutoff, rc.Cancelled)
}
