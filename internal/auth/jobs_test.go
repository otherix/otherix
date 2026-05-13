// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// fakeCleaner records each call and replays canned (deleted, err) pairs.
type fakeCleaner struct {
	calls   []time.Time
	deleted int64
	err     error
}

func (f *fakeCleaner) DeleteExpiredRefreshTokens(_ context.Context, cutoff time.Time) (int64, error) {
	f.calls = append(f.calls, cutoff)
	return f.deleted, f.err
}

func TestRefreshTokenCleanupWorker_Work_HappyPath(t *testing.T) {
	t.Parallel()

	fake := &fakeCleaner{deleted: 7}
	w := &RefreshTokenCleanupWorker{
		cleaner: fake,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	before := time.Now()
	if err := w.Work(context.Background(), &river.Job[RefreshTokenCleanupArgs]{
		Args: RefreshTokenCleanupArgs{},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	after := time.Now()

	if len(fake.calls) != 1 {
		t.Fatalf("DeleteExpiredRefreshTokens calls = %d, want 1", len(fake.calls))
	}
	cutoff := fake.calls[0]
	if cutoff.Before(before) || cutoff.After(after) {
		t.Errorf("cutoff %v not within [%v, %v]", cutoff, before, after)
	}
}

func TestRefreshTokenCleanupWorker_Work_PropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("DB exploded")
	fake := &fakeCleaner{err: wantErr}
	w := &RefreshTokenCleanupWorker{
		cleaner: fake,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err := w.Work(context.Background(), &river.Job[RefreshTokenCleanupArgs]{
		Args: RefreshTokenCleanupArgs{},
	})
	if err == nil {
		t.Fatalf("Work returned nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		// Work wraps with fmt.Errorf %v, so errors.Is can't see through —
		// match on substring instead.
		if !contains(err.Error(), wantErr.Error()) {
			t.Errorf("Work error = %q, want it to mention %q", err, wantErr)
		}
	}
}

func TestRefreshTokenCleanupArgs_Kind(t *testing.T) {
	t.Parallel()

	if got, want := (RefreshTokenCleanupArgs{}).Kind(), "auth.refresh_token_cleanup"; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
}

func TestPeriodicJobs_Shape(t *testing.T) {
	t.Parallel()

	jobs := PeriodicJobs(Deps{})
	if len(jobs) != 1 {
		t.Fatalf("PeriodicJobs returned %d jobs, want 1", len(jobs))
	}
	// Deeper assertions on schedule and args constructor are guarded by
	// river's unexported fields; the shape is exercised end-to-end in
	// tests/integration/auth_jobs_test.go via Subscribe + Insert.
	if jobs[0] == nil {
		t.Fatalf("PeriodicJobs[0] is nil")
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
