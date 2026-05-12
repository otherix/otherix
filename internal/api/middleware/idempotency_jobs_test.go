// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/riverqueue/river"
)

// fakeIdempotencyCleaner records call counts and replays canned
// (deleted, err) pairs.
type fakeIdempotencyCleaner struct {
	calls   int
	deleted int64
	err     error
}

func (f *fakeIdempotencyCleaner) DeleteExpiredIdempotencyKeys(_ context.Context) (int64, error) {
	f.calls++
	return f.deleted, f.err
}

func TestIdempotencyCleanupWorker_Work_HappyPath(t *testing.T) {
	t.Parallel()

	fake := &fakeIdempotencyCleaner{deleted: 11}
	w := &IdempotencyCleanupWorker{
		cleaner: fake,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := w.Work(context.Background(), &river.Job[IdempotencyCleanupArgs]{
		Args: IdempotencyCleanupArgs{},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if fake.calls != 1 {
		t.Errorf("DeleteExpiredIdempotencyKeys calls = %d, want 1", fake.calls)
	}
}

func TestIdempotencyCleanupWorker_Work_PropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("DB exploded")
	fake := &fakeIdempotencyCleaner{err: wantErr}
	w := &IdempotencyCleanupWorker{
		cleaner: fake,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err := w.Work(context.Background(), &river.Job[IdempotencyCleanupArgs]{
		Args: IdempotencyCleanupArgs{},
	})
	if err == nil {
		t.Fatalf("Work returned nil, want non-nil")
	}
	// Work wraps with fmt.Errorf %v, which doesn't preserve the chain
	// for errors.Is — match on substring.
	if !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("Work error = %q, want it to mention %q", err, wantErr)
	}
}

func TestIdempotencyCleanupArgs_Kind(t *testing.T) {
	t.Parallel()

	if got, want := (IdempotencyCleanupArgs{}).Kind(), "idempotency.cleanup"; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
}

func TestIdempotencyPeriodicJobs_Shape(t *testing.T) {
	t.Parallel()

	jobs := IdempotencyPeriodicJobs(IdempotencyDeps{})
	if len(jobs) != 1 {
		t.Fatalf("IdempotencyPeriodicJobs returned %d jobs, want 1", len(jobs))
	}
	if jobs[0] == nil {
		t.Fatalf("IdempotencyPeriodicJobs[0] is nil")
	}
}
