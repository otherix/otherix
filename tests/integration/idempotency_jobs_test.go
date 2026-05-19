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
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// TestIdempotencyCleanupWorker_EndToEnd seeds expired idempotency_keys
// rows in mixed states (`in_flight` + `completed`) plus fresh rows of
// each state, drives one cleanup job through the real river client,
// and asserts the worker deletes every row past expires_at regardless
// of state. Defends against a regression that adds a state filter
// to the cleanup query.
func TestIdempotencyCleanupWorker_EndToEnd(t *testing.T) {
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

	// Seed: 2 expired in_flight, 1 expired completed, 1 fresh in_flight,
	// 1 fresh completed. After cleanup: only the 2 fresh rows remain.
	type seed struct {
		state     string
		expiresAt time.Time
	}
	now := time.Now()
	seeds := []seed{
		{state: "in_flight", expiresAt: now.Add(-1 * time.Hour)},
		{state: "in_flight", expiresAt: now.Add(-2 * time.Hour)},
		{state: "completed", expiresAt: now.Add(-30 * time.Minute)},
		{state: "in_flight", expiresAt: now.Add(24 * time.Hour)},
		{state: "completed", expiresAt: now.Add(48 * time.Hour)},
	}
	const wantSurvivors = 2
	for i, sd := range seeds {
		if _, err := h.Pool.Exec(ctx, `
			insert into idempotency_keys
				(key, request_method, request_path, request_hash, state, expires_at)
			values
				($1, 'POST', '/v1/test', $2, $3, $4)
		`, fmt.Sprintf("seed-%d-%s", i, uuid.NewString()[:8]), randomBytes(t, 32), sd.state, sd.expiresAt); err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
	}

	beforeTotal := countIdempotencyKeys(t, ctx, s)
	if beforeTotal != len(seeds) {
		t.Fatalf("seeded idempotency_keys count = %d, want %d", beforeTotal, len(seeds))
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := api.BuildRiverClient(api.RiverDeps{
		Pool:   h.Pool,
		Cfg:    config.WorkersConfig{Enabled: true, MaxWorkers: 4},
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

	if _, err := client.Insert(ctx, middleware.IdempotencyCleanupArgs{}, nil); err != nil {
		t.Fatalf("Insert IdempotencyCleanupArgs: %v", err)
	}

	// Multiple kinds may complete during the test (the SystemLivenessProbe
	// is registered too); loop until we see ours or hit the deadline.
	deadline := time.After(5 * time.Second)
	gotKind := ""
loop:
	for {
		select {
		case ev := <-completions:
			if ev.Job.Kind == "idempotency.cleanup" {
				gotKind = ev.Job.Kind
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if gotKind != "idempotency.cleanup" {
		t.Fatalf("did not observe idempotency.cleanup completion within 5s")
	}

	afterTotal := countIdempotencyKeys(t, ctx, s)
	if afterTotal != wantSurvivors {
		t.Fatalf("after cleanup, idempotency_keys count = %d, want %d (seeded %d total, expected %d expired deleted regardless of state)",
			afterTotal, wantSurvivors, len(seeds), len(seeds)-wantSurvivors)
	}
}

func countIdempotencyKeys(t *testing.T, ctx context.Context, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx,
		`select count(*) from idempotency_keys`,
	).Scan(&n); err != nil {
		t.Fatalf("count idempotency_keys: %v", err)
	}
	return n
}
