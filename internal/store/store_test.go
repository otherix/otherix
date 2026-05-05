// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// newStore opens a Store against the harness's DSN. Each call produces a
// fresh Store (and pool) — cheap because the underlying container is shared
// across the test package.
func newStore(t *testing.T, h *migrationtest.Harness) *store.Store {
	t.Helper()
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN:      h.DSN,
		MaxConns: 4,
		MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// uniqueEmail returns a per-call unique email so concurrent / sequential
// tests in this package don't collide on uq_users_email.
func uniqueEmail() string {
	return "u-" + uuid.NewString() + "@example.test"
}

func requireSharedHarness(t *testing.T) {
	t.Helper()
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised; TestMain skipped?")
	}
}

func TestInTxCommit(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	email := uniqueEmail()

	if err := s.InTx(ctx, func(q *store.Queries) error {
		_, err := q.CreateUser(ctx, store.CreateUserParams{
			ID:           id,
			Email:        email,
			PasswordHash: "x",
			DisplayName:  "",
			Role:         "viewer",
		})
		return err
	}); err != nil {
		t.Fatalf("InTx commit = %v, want nil", err)
	}

	got, err := s.Queries().GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("GetUserByID after commit: %v", err)
	}
	if got.Email != email {
		t.Errorf("user.Email = %q, want %q", got.Email, email)
	}
}

func TestInTxRollbackOnError(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	wantErr := errors.New("synthetic")

	err := s.InTx(ctx, func(q *store.Queries) error {
		if _, err := q.CreateUser(ctx, store.CreateUserParams{
			ID:           id,
			Email:        uniqueEmail(),
			PasswordHash: "x",
			DisplayName:  "",
			Role:         "viewer",
		}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("InTx err = %v, want wraps %v", err, wantErr)
	}

	if _, err := s.Queries().GetUserByID(ctx, id); err == nil {
		t.Errorf("GetUserByID after rollback succeeded; want sql.ErrNoRows-like error")
	}
}

func TestInTxRollbackOnPanic(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("InTx swallowed the panic; want it to propagate")
		}
		if _, err := s.Queries().GetUserByID(ctx, id); err == nil {
			t.Errorf("GetUserByID after panic-rollback succeeded; want error")
		}
	}()

	_ = s.InTx(ctx, func(q *store.Queries) error {
		if _, err := q.CreateUser(ctx, store.CreateUserParams{
			ID:           id,
			Email:        uniqueEmail(),
			PasswordHash: "x",
			DisplayName:  "",
			Role:         "viewer",
		}); err != nil {
			return err
		}
		panic("boom")
	})
}

func TestInTxSeesItsOwnWrites(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	email := uniqueEmail()

	err := s.InTx(ctx, func(q *store.Queries) error {
		if _, err := q.CreateUser(ctx, store.CreateUserParams{
			ID:           id,
			Email:        email,
			PasswordHash: "x",
			DisplayName:  "",
			Role:         "viewer",
		}); err != nil {
			return err
		}
		got, err := q.GetUserByID(ctx, id)
		if err != nil {
			return err
		}
		if got.Email != email {
			t.Errorf("inside-tx GetUserByID email = %q, want %q", got.Email, email)
		}
		return nil
	})
	if err != nil {
		t.Errorf("InTx = %v, want nil", err)
	}
}
