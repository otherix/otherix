// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/store"
)

func TestUsersCreateGetByIDGetByEmail(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	email := uniqueEmail()

	created, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: "hash",
		DisplayName:  "Alice",
		Role:         "operator",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID = %v, want %v", created.ID, id)
	}
	if created.Email != email {
		t.Errorf("created.Email = %q, want %q", created.Email, email)
	}
	if created.Role != "operator" {
		t.Errorf("created.Role = %q, want %q", created.Role, "operator")
	}
	if created.CreatedAt.IsZero() {
		t.Errorf("created.CreatedAt is zero, want set")
	}

	gotByID, err := s.Queries().GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if gotByID.ID != id {
		t.Errorf("GetUserByID.ID = %v, want %v", gotByID.ID, id)
	}

	gotByEmail, err := s.Queries().GetUserByEmail(ctx, strings.ToUpper(email))
	if err != nil {
		t.Fatalf("GetUserByEmail (uppercased): %v", err)
	}
	if gotByEmail.ID != id {
		t.Errorf("GetUserByEmail (case-insensitive) returned id = %v, want %v", gotByEmail.ID, id)
	}
}

func TestUsersDuplicateEmailRejected(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	email := uniqueEmail()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: uuid.New(), Email: email, PasswordHash: "x", Role: "viewer",
	}); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: uuid.New(), Email: strings.ToUpper(email), PasswordHash: "x", Role: "viewer",
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("duplicate-email err = %v, want pg 23505 (unique_violation)", err)
	}
}

func TestUsersListPagination(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	// Insert 5 users belonging to this test only — tag them via display name
	// so we can filter them back out client-side, since the shared DB has
	// other rows from previous tests.
	tag := "list-page-" + uuid.NewString()
	createdIDs := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		u, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
			ID:           uuid.New(),
			Email:        uniqueEmail(),
			PasswordHash: "x",
			DisplayName:  tag,
			Role:         "viewer",
		})
		if err != nil {
			t.Fatalf("CreateUser %d: %v", i, err)
		}
		createdIDs = append(createdIDs, u.ID)
		// Tiny sleep so created_at strictly increases per row, exercising the
		// (created_at, id) cursor ordering.
		time.Sleep(2 * time.Millisecond)
	}

	got := make([]uuid.UUID, 0, 5)
	var (
		cursorTS *time.Time
		cursorID *uuid.UUID
	)
	const maxPages = 1000 // safety cap; real terminator is the empty-page break below.
	for page := 0; ; page++ {
		if page >= maxPages {
			t.Fatalf("ListUsers pagination did not terminate within %d pages", maxPages)
		}
		rows, err := s.Queries().ListUsers(ctx, store.ListUsersParams{
			CursorCreatedAt: cursorTS,
			CursorID:        cursorID,
			LimitCount:      2,
		})
		if err != nil {
			t.Fatalf("ListUsers page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if r.DisplayName == tag {
				got = append(got, r.ID)
			}
		}
		last := rows[len(rows)-1]
		ts, id := last.CreatedAt, last.ID
		cursorTS, cursorID = &ts, &id
		if len(rows) < 2 {
			break
		}
	}
	if len(got) != 5 {
		t.Errorf("ListUsers paginated through %d tagged users, want 5", len(got))
	}
	for i := 0; i < len(got); i++ {
		if got[i] != createdIDs[i] {
			t.Errorf("paginated order: got[%d] = %v, want %v", i, got[i], createdIDs[i])
		}
	}
}

func TestUsersUpdate(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: id, Email: uniqueEmail(), PasswordHash: "h1", DisplayName: "Old", Role: "viewer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	updated, err := s.Queries().UpdateUser(ctx, store.UpdateUserParams{
		ID:           id,
		Email:        uniqueEmail(),
		PasswordHash: "h2",
		DisplayName:  "New",
		Role:         "developer",
	})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.DisplayName != "New" {
		t.Errorf("updated.DisplayName = %q, want %q", updated.DisplayName, "New")
	}
	if updated.Role != "developer" {
		t.Errorf("updated.Role = %q, want %q", updated.Role, "developer")
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Errorf("UpdatedAt (%v) not after CreatedAt (%v)", updated.UpdatedAt, updated.CreatedAt)
	}
}

func TestUsersTouchLastLogin(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: id, Email: uniqueEmail(), PasswordHash: "x", Role: "viewer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.Queries().TouchUserLastLogin(ctx, id); err != nil {
		t.Fatalf("TouchUserLastLogin: %v", err)
	}

	got, err := s.Queries().GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.LastLoginAt == nil {
		t.Errorf("LastLoginAt = nil after Touch, want set")
	}
}

func TestUsersSoftDeleteHidesFromGet(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: id, Email: uniqueEmail(), PasswordHash: "x", Role: "viewer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.Queries().SoftDeleteUser(ctx, id); err != nil {
		t.Fatalf("SoftDeleteUser: %v", err)
	}

	if _, err := s.Queries().GetUserByID(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetUserByID after soft delete err = %v, want pgx.ErrNoRows", err)
	}
}

func TestUsersCountUserResourcesZeroForFreshUser(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: id, Email: uniqueEmail(), PasswordHash: "x", Role: "viewer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	got, err := s.Queries().CountUserResources(ctx, id)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if got.Vms != 0 || got.Templates != 0 || got.Snapshots != 0 {
		t.Errorf("CountUserResources = %+v, want zeros", got)
	}
}
