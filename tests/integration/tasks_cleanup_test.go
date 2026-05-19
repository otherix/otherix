// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// TestTasksCleanupWorker_EndToEnd seeds a mixed set of finalized
// task rows (success / failed / cancelled, expired and fresh) plus
// one pending row, drives one tasks.cleanup pass through the real
// river client, and asserts the worker honours per-state retention
// windows: only rows past their respective cutoff are removed,
// and rows that have not finalized (finished_at IS NULL) are
// immune.
//
// Like the auth-domain cleanup test, the cutoffs come from the
// production defaults baked into the tasks package (7 day completed,
// 30 day failed/cancelled), so the test seeds finished_at values
// far enough on either side of those windows that scheduler jitter
// cannot flip the assertion.
func TestTasksCleanupWorker_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := migrationtest.Start(ctx)
	if err != nil {
		t.Fatalf("migrationtest.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})

	s := store.FromPool(h.Pool)

	userID := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           userID,
		Email:        fmt.Sprintf("tasks-cleanup-%s@example.test", uuid.NewString()[:8]),
		PasswordHash: "x",
		DisplayName:  "tasks cleanup",
		Role:         "developer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	now := time.Now()
	type seed struct {
		status     string
		finishedAt *time.Time
		expired    bool // expected to be deleted by cleanup
	}
	expiredSuccess := now.Add(-8 * 24 * time.Hour) // 8d > 7d retention
	freshSuccess := now.Add(-6 * 24 * time.Hour)   // 6d < 7d retention
	expiredFailed := now.Add(-31 * 24 * time.Hour) // 31d > 30d
	freshFailed := now.Add(-29 * 24 * time.Hour)   // 29d < 30d
	expiredCancelled := now.Add(-31 * 24 * time.Hour)

	seeds := []seed{
		{status: "success", finishedAt: &expiredSuccess, expired: true},
		{status: "success", finishedAt: &expiredSuccess, expired: true},
		{status: "success", finishedAt: &expiredSuccess, expired: true},
		{status: "success", finishedAt: &freshSuccess, expired: false},
		{status: "failed", finishedAt: &expiredFailed, expired: true},
		{status: "failed", finishedAt: &expiredFailed, expired: true},
		{status: "failed", finishedAt: &freshFailed, expired: false},
		{status: "cancelled", finishedAt: &expiredCancelled, expired: true},
		{status: "pending", finishedAt: nil, expired: false},
	}

	keptIDs := []uuid.UUID{}
	for i, sd := range seeds {
		taskID := uuid.New()
		if _, err := h.Pool.Exec(ctx,
			`insert into tasks
			 (id, type, status, resource_type, resource_id,
			  args, max_attempts, created_by, created_at, finished_at)
			 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			taskID,
			"storage_pool.scan",
			sd.status,
			"storage_pool",
			uuid.New(),
			[]byte(`{}`),
			25,
			userID,
			now.Add(-32*24*time.Hour), // older than any cutoff for created_at sorting
			sd.finishedAt,
		); err != nil {
			t.Fatalf("insert tasks[%d]: %v", i, err)
		}
		if !sd.expired {
			keptIDs = append(keptIDs, taskID)
		}
	}

	beforeTotal := countTasks(t, ctx, s, userID)
	if beforeTotal != len(seeds) {
		t.Fatalf("seeded tasks count = %d, want %d", beforeTotal, len(seeds))
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := api.BuildRiverClient(api.RiverDeps{
		Pool: h.Pool,
		Cfg: config.WorkersConfig{
			Enabled:    true,
			MaxWorkers: 4,
			Tasks: config.TasksWorkersConfig{
				Retention: config.TaskRetentionConfig{
					Completed: 7 * 24 * time.Hour,
					Failed:    30 * 24 * time.Hour,
				},
			},
		},
		Logger: log,
		Store:  s,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}

	completions, cancelSub := client.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = client.Stop(stopCtx)
	})

	if _, err := client.Insert(ctx, taskshandlers.CleanupArgs{}, nil); err != nil {
		t.Fatalf("Insert tasks.CleanupArgs: %v", err)
	}

	// Drain completions until the cleanup job lands. Other periodic
	// workers (auth refresh-token, idempotency) may also fire under
	// RunOnStart=false (they shouldn't, but match strictly to be
	// safe).
	deadline := time.After(15 * time.Second)
WaitLoop:
	for {
		select {
		case ev := <-completions:
			if ev.Job.Kind == "tasks.cleanup" {
				break WaitLoop
			}
		case <-deadline:
			t.Fatalf("tasks.cleanup did not complete within 15s")
		}
	}

	afterTotal := countTasks(t, ctx, s, userID)
	wantAfter := len(keptIDs)
	if afterTotal != wantAfter {
		t.Fatalf("after cleanup, tasks count = %d, want %d (%d expired deleted, %d fresh+pending kept)",
			afterTotal, wantAfter, len(seeds)-wantAfter, wantAfter)
	}

	// Verify each kept id survived (catches pathological state-mixup).
	for _, id := range keptIDs {
		if _, err := s.Queries().GetTask(ctx, id); err != nil {
			t.Errorf("kept task %s missing after cleanup: %v", id, err)
		}
	}
}

func countTasks(t *testing.T, ctx context.Context, s *store.Store, createdBy uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx,
		`select count(*) from tasks where created_by = $1`, createdBy,
	).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}
