// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestCasUserUpdateRetriesOnStaleModRevision proves TouchUserLastLogin's CAS has
// teeth: a competing admin write (a role change via UpdateUser) that lands
// between casUserUpdate's read and its commit must force the CAS to miss, drive a
// fresh re-read, and leave BOTH writes on the row - the role change AND the
// last_login stamp. A blind put would clobber the role change with its stale
// snapshot, silently re-persisting the prior role on the next login.
func TestCasUserUpdateRetriesOnStaleModRevision(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()

	created, err := s.CreateUser(ctx, store.CreateUserParams{
		ID:           uuid.New(),
		Username:     "cas-" + uuid.NewString()[:8],
		Email:        "cas-" + uuid.NewString()[:8] + "@example.test",
		PasswordHash: "argon2id$stub",
		DisplayName:  "CAS User",
		Role:         "operator",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	now := time.Now().UTC()
	var attempts int
	out, err := s.casUserUpdate(ctx, created.ID, func(u *store.User) {
		attempts++
		if attempts == 1 {
			// Competing admin write between read and commit: demote the role.
			// It bumps the row's ModRevision, so the pending CAS misses.
			if _, uerr := s.UpdateUser(ctx, store.UpdateUserParams{
				ID:           created.ID,
				Email:        created.Email,
				PasswordHash: created.PasswordHash,
				DisplayName:  created.DisplayName,
				Role:         "viewer",
			}); uerr != nil {
				t.Fatalf("competing UpdateUser: %v", uerr)
			}
		}
		u.LastLoginAt = &now
	})
	if err != nil {
		t.Fatalf("casUserUpdate: %v", err)
	}
	if attempts < 2 {
		t.Errorf("casUserUpdate did not retry: attempts = %d, want >= 2", attempts)
	}
	if out.Role != "viewer" {
		t.Errorf("returned Role = %q, want viewer (competing role change survived)", out.Role)
	}
	if out.LastLoginAt == nil {
		t.Errorf("returned LastLoginAt = nil, want stamped")
	}

	// Re-read: both writes must be durably persisted.
	got, err := s.UserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.Role != "viewer" {
		t.Errorf("persisted Role = %q, want viewer", got.Role)
	}
	if got.LastLoginAt == nil {
		t.Errorf("persisted LastLoginAt = nil, want stamped")
	}
}

// TestTouchUserLastLoginNoOpOnMissingOrDeleted pins the no-op contract that the
// CAS conversion must preserve: a missing user and a soft-deleted user both
// return nil without writing.
func TestTouchUserLastLoginNoOpOnMissingOrDeleted(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()

	// Missing user: no-op.
	if err := s.TouchUserLastLogin(ctx, uuid.New()); err != nil {
		t.Errorf("TouchUserLastLogin(missing) = %v, want nil", err)
	}

	created, err := s.CreateUser(ctx, store.CreateUserParams{
		ID:           uuid.New(),
		Username:     "del-" + uuid.NewString()[:8],
		Email:        "del-" + uuid.NewString()[:8] + "@example.test",
		PasswordHash: "argon2id$stub",
		DisplayName:  "Deleted User",
		Role:         "developer",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DeleteUser(ctx, created.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	// Soft-deleted user: no-op (must not stamp last_login on a dead row).
	if err := s.TouchUserLastLogin(ctx, created.ID); err != nil {
		t.Errorf("TouchUserLastLogin(deleted) = %v, want nil", err)
	}
}
