// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// TestRefreshTokenCleanupWorker_EndToEnd seeds three expired and two
// fresh refresh-token rows, drives one cleanup job through the real
// river client, and asserts the worker honours the cutoff: expired
// rows are deleted, fresh rows survive.
//
// The test uses migrationtest.Start (full migrate stack) — using
// NewIsolated would land Step 3's known regression where
// rivermigrate fails because river_queue does not exist yet.
func TestRefreshTokenCleanupWorker_EndToEnd(t *testing.T) {
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
		Email:        fmt.Sprintf("cleanup-%s@example.test", uuid.NewString()[:8]),
		PasswordHash: "hash",
		DisplayName:  "Cleanup Fixture",
		Role:         "viewer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	const (
		expiredCount = 3
		freshCount   = 2
	)
	now := time.Now()
	for i := 0; i < expiredCount+freshCount; i++ {
		expiresAt := now.Add(-1 * time.Hour) // expired
		if i >= expiredCount {
			expiresAt = now.Add(24 * time.Hour) // fresh
		}
		tokenID := uuid.New()
		if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
			ID:        tokenID,
			UserID:    userID,
			TokenHash: randomBytes(t, 32),
			FamilyID:  tokenID, // first token in its own family
			ExpiresAt: expiresAt,
		}); err != nil {
			t.Fatalf("CreateRefreshToken[%d]: %v", i, err)
		}
	}

	beforeTotal := countRefreshTokens(t, ctx, s, userID)
	if beforeTotal != expiredCount+freshCount {
		t.Fatalf("seeded refresh_tokens count = %d, want %d", beforeTotal, expiredCount+freshCount)
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

	if _, err := client.Insert(ctx, auth.RefreshTokenCleanupArgs{}, nil); err != nil {
		t.Fatalf("Insert RefreshTokenCleanupArgs: %v", err)
	}

	select {
	case ev := <-completions:
		if ev.Job.Kind != "auth.refresh_token_cleanup" {
			t.Fatalf("completed kind = %q, want auth.refresh_token_cleanup", ev.Job.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("refresh-token cleanup did not complete within 5s")
	}

	afterTotal := countRefreshTokens(t, ctx, s, userID)
	if afterTotal != freshCount {
		t.Fatalf("after cleanup, refresh_tokens count = %d, want %d (expected %d expired deleted, %d fresh kept)",
			afterTotal, freshCount, expiredCount, freshCount)
	}
}

func countRefreshTokens(t *testing.T, ctx context.Context, s *store.Store, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx,
		`select count(*) from refresh_tokens where user_id = $1`, userID,
	).Scan(&n); err != nil {
		t.Fatalf("count refresh_tokens: %v", err)
	}
	return n
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}
