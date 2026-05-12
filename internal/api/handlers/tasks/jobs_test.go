// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

// fakeCleaner records each DeleteExpiredTasks invocation and replays
// canned (deleted, err) pairs so direct Worker.Work tests can pin
// the cutoff arithmetic without the database.
type fakeCleaner struct {
	calls   []store.DeleteExpiredTasksParams
	deleted int64
	err     error
}

func (f *fakeCleaner) DeleteExpiredTasks(_ context.Context, arg store.DeleteExpiredTasksParams) (int64, error) {
	f.calls = append(f.calls, arg)
	return f.deleted, f.err
}

func TestCleanupArgs_Kind(t *testing.T) {
	t.Parallel()
	if got, want := (CleanupArgs{}).Kind(), "tasks.cleanup"; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
}

func TestCleanupWorker_Work_HappyPath_DefaultRetention(t *testing.T) {
	t.Parallel()

	fake := &fakeCleaner{deleted: 4}
	w := &CleanupWorker{
		cleaner:   fake,
		retention: RetentionConfig{}.withDefaults(),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	before := time.Now()
	if err := w.Work(context.Background(), &river.Job[CleanupArgs]{Args: CleanupArgs{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	after := time.Now()

	if len(fake.calls) != 1 {
		t.Fatalf("DeleteExpiredTasks calls = %d, want 1", len(fake.calls))
	}
	args := fake.calls[0]

	wantCompletedLow := before.Add(-defaultCompletedRetention)
	wantCompletedHigh := after.Add(-defaultCompletedRetention)
	if args.CompletedCutoff.Before(wantCompletedLow) || args.CompletedCutoff.After(wantCompletedHigh) {
		t.Errorf("CompletedCutoff = %v, want within [%v, %v]",
			args.CompletedCutoff, wantCompletedLow, wantCompletedHigh)
	}

	wantFailedLow := before.Add(-defaultFailedRetention)
	wantFailedHigh := after.Add(-defaultFailedRetention)
	if args.FailedCutoff.Before(wantFailedLow) || args.FailedCutoff.After(wantFailedHigh) {
		t.Errorf("FailedCutoff = %v, want within [%v, %v]",
			args.FailedCutoff, wantFailedLow, wantFailedHigh)
	}
}

func TestCleanupWorker_Work_HappyPath_OverriddenRetention(t *testing.T) {
	t.Parallel()

	fake := &fakeCleaner{}
	w := &CleanupWorker{
		cleaner: fake,
		retention: RetentionConfig{
			Completed: 5 * time.Minute,
			Failed:    1 * time.Hour,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	before := time.Now()
	if err := w.Work(context.Background(), &river.Job[CleanupArgs]{Args: CleanupArgs{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	after := time.Now()

	args := fake.calls[0]
	if args.CompletedCutoff.Before(before.Add(-5*time.Minute)) || args.CompletedCutoff.After(after.Add(-5*time.Minute)) {
		t.Errorf("CompletedCutoff = %v, want now-5m", args.CompletedCutoff)
	}
	if args.FailedCutoff.Before(before.Add(-1*time.Hour)) || args.FailedCutoff.After(after.Add(-1*time.Hour)) {
		t.Errorf("FailedCutoff = %v, want now-1h", args.FailedCutoff)
	}
}

func TestCleanupWorker_Work_PropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("DB exploded")
	fake := &fakeCleaner{err: wantErr}
	w := &CleanupWorker{
		cleaner:   fake,
		retention: RetentionConfig{}.withDefaults(),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err := w.Work(context.Background(), &river.Job[CleanupArgs]{Args: CleanupArgs{}})
	if err == nil {
		t.Fatalf("Work returned nil, want non-nil")
	}
	// Work wraps with fmt.Errorf %v, so errors.Is can't see through —
	// match on substring instead, mirroring the auth-domain pattern.
	if !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("Work error = %q, want it to mention %q", err, wantErr)
	}
}

func TestPeriodicJobs_Shape(t *testing.T) {
	t.Parallel()

	jobs := PeriodicJobs(Deps{})
	if len(jobs) != 1 {
		t.Fatalf("PeriodicJobs returned %d jobs, want 1", len(jobs))
	}
	if jobs[0] == nil {
		t.Fatalf("PeriodicJobs[0] is nil")
	}
	// Deeper assertions on schedule cadence and args constructor are
	// guarded by river's unexported fields; the wiring is exercised
	// end-to-end in tests/integration/tasks_cleanup_test.go via
	// Subscribe + Insert.
}

func TestRetentionConfig_WithDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   RetentionConfig
		want RetentionConfig
	}{
		{
			name: "all zero — both defaulted",
			in:   RetentionConfig{},
			want: RetentionConfig{Completed: defaultCompletedRetention, Failed: defaultFailedRetention},
		},
		{
			name: "completed set, failed zero — failed defaulted",
			in:   RetentionConfig{Completed: 1 * time.Hour},
			want: RetentionConfig{Completed: 1 * time.Hour, Failed: defaultFailedRetention},
		},
		{
			name: "both set — pass through",
			in:   RetentionConfig{Completed: 3 * time.Minute, Failed: 4 * time.Hour},
			want: RetentionConfig{Completed: 3 * time.Minute, Failed: 4 * time.Hour},
		},
		{
			name: "negative treated as zero",
			in:   RetentionConfig{Completed: -1 * time.Second},
			want: RetentionConfig{Completed: defaultCompletedRetention, Failed: defaultFailedRetention},
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
