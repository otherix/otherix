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

	"github.com/otherix/otherix/internal/store"
)

func userStoreParams(id uuid.UUID, email string) store.CreateUserParams {
	return store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: "x",
		DisplayName:  "Test User",
		Role:         "developer",
	}
}

func TestUserByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.UserByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByID(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestUserByEmailNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.UserByEmail(context.Background(), uniqueEmail()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByEmail(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestCreateUserDuplicateEmail(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	email := uniqueEmail()
	if _, err := s.CreateUser(ctx, userStoreParams(uuid.New(), email)); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err := s.CreateUser(ctx, userStoreParams(uuid.New(), email))
	if !errors.Is(err, store.ErrUserEmailExists) {
		t.Errorf("duplicate CreateUser error = %v, want store.ErrUserEmailExists", err)
	}
}

func TestDeleteUserSucceeds(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.CreateUser(ctx, userStoreParams(id, uniqueEmail())); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DeleteUser(ctx, id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.UserByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after delete UserByID error = %v, want store.ErrNotFound", err)
	}
}
