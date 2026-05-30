// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	usershandlers "github.com/otherix/otherix/internal/api/handlers/users"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd-backed store satisfies the users handler's storage contract.
var _ usershandlers.Store = (*etcdstore.Store)(nil)

func userParams(email string) store.CreateUserParams {
	return store.CreateUserParams{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: "argon2id$stub",
		DisplayName:  "Test User",
		Role:         "developer",
	}
}

func uniqueEmail(prefix string) string { return prefix + "-" + uuid.NewString()[:8] + "@example.test" }

func TestUserCreateGetByIDAndEmail(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := userParams(uniqueEmail("u"))

	created, err := s.CreateUser(ctx, p)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID != p.ID || created.CreatedAt.IsZero() {
		t.Errorf("CreateUser = %+v, want id set + created_at stamped", created)
	}
	byID, err := s.UserByID(ctx, p.ID)
	if err != nil || byID.Email != p.Email {
		t.Fatalf("UserByID = (%+v, %v), want email %q", byID, err, p.Email)
	}
	// Case-insensitive email resolution via the guard index.
	byEmail, err := s.UserByEmail(ctx, strings.ToUpper(p.Email))
	if err != nil || byEmail.ID != p.ID {
		t.Fatalf("UserByEmail(upper) = (%+v, %v), want id %v", byEmail, err, p.ID)
	}
}

func TestUserByIDAndEmailNotFound(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.UserByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByID(absent) = %v, want store.ErrNotFound", err)
	}
	if _, err := s.UserByEmail(ctx, uniqueEmail("missing")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByEmail(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestUserCreateDuplicateEmail(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	email := uniqueEmail("dup")
	if _, err := s.CreateUser(ctx, userParams(email)); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	clash := userParams(strings.ToUpper(email)) // case-insensitive collision
	if _, err := s.CreateUser(ctx, clash); !errors.Is(err, store.ErrUserEmailExists) {
		t.Errorf("dup email err = %v, want store.ErrUserEmailExists", err)
	}
}

func TestUserUpdateKeepsEmail(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := userParams(uniqueEmail("upd"))
	created, err := s.CreateUser(ctx, p)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	updated, err := s.UpdateUser(ctx, store.UpdateUserParams{
		ID: p.ID, Email: p.Email, PasswordHash: "argon2id$new", DisplayName: "Renamed", Role: "operator",
	})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.DisplayName != "Renamed" || updated.Role != "operator" {
		t.Errorf("UpdateUser = %+v, want Renamed/operator", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at not bumped")
	}
	// Email guard intact: UserByEmail still resolves.
	if _, err := s.UserByEmail(ctx, p.Email); err != nil {
		t.Errorf("UserByEmail after same-email update: %v", err)
	}
}

func TestUserUpdateEmailMovesGuard(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := userParams(uniqueEmail("old"))
	if _, err := s.CreateUser(ctx, p); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	newEmail := uniqueEmail("new")
	if _, err := s.UpdateUser(ctx, store.UpdateUserParams{
		ID: p.ID, Email: newEmail, PasswordHash: p.PasswordHash, DisplayName: p.DisplayName, Role: p.Role,
	}); err != nil {
		t.Fatalf("UpdateUser email change: %v", err)
	}
	// New email resolves; old email is free again.
	if _, err := s.UserByEmail(ctx, newEmail); err != nil {
		t.Errorf("UserByEmail(new) = %v, want found", err)
	}
	if _, err := s.UserByEmail(ctx, p.Email); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByEmail(old) = %v, want store.ErrNotFound", err)
	}
	// Old email is reusable by a fresh user; new email collides.
	if _, err := s.CreateUser(ctx, userParams(p.Email)); err != nil {
		t.Errorf("old email not reusable: %v", err)
	}
	if _, err := s.CreateUser(ctx, userParams(newEmail)); !errors.Is(err, store.ErrUserEmailExists) {
		t.Errorf("new email should be taken, got %v", err)
	}
}

func TestUserListExcludesDeleted(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	keep := userParams(uniqueEmail("keep"))
	drop := userParams(uniqueEmail("drop"))
	if _, err := s.CreateUser(ctx, keep); err != nil {
		t.Fatalf("seed keep: %v", err)
	}
	if _, err := s.CreateUser(ctx, drop); err != nil {
		t.Fatalf("seed drop: %v", err)
	}
	if err := s.DeleteUser(ctx, drop.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	list, err := s.ListUsers(ctx, store.ListUsersParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var sawKeep bool
	for _, u := range list {
		if u.ID == drop.ID {
			t.Errorf("deleted user listed")
		}
		if u.ID == keep.ID {
			sawKeep = true
		}
	}
	if !sawKeep {
		t.Errorf("live user missing from list")
	}
}

func TestUserDeleteAndEmailReuse(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	email := uniqueEmail("del")
	p := userParams(email)
	if _, err := s.CreateUser(ctx, p); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DeleteUser(ctx, p.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.UserByID(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByID after delete = %v, want store.ErrNotFound", err)
	}
	if _, err := s.UserByEmail(ctx, email); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByEmail after delete = %v, want store.ErrNotFound", err)
	}
	if _, err := s.CreateUser(ctx, userParams(email)); err != nil {
		t.Errorf("email not reusable after delete: %v", err)
	}
}

func TestCountUserResources(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := userParams(uniqueEmail("owner"))
	if _, err := s.CreateUser(ctx, p); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// No owned resources yet.
	zero, err := s.CountUserResources(ctx, p.ID)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if zero.Vms != 0 || zero.Templates != 0 || zero.Snapshots != 0 {
		t.Errorf("CountUserResources(no owned) = %+v, want all zero", zero)
	}

	// Seed two owned-vm + one owned-template index entries.
	for i := 0; i < 2; i++ {
		k := etcd.Key("index", "vms", "owner", p.ID.String(), uuid.NewString())
		if err := cli.Put(ctx, k, []byte("vm")); err != nil {
			t.Fatalf("seed vm index: %v", err)
		}
	}
	if err := cli.Put(ctx, etcd.Key("index", "templates", "owner", p.ID.String(), uuid.NewString()), []byte("tpl")); err != nil {
		t.Fatalf("seed template index: %v", err)
	}

	got, err := s.CountUserResources(ctx, p.ID)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if got.Vms != 2 || got.Templates != 1 || got.Snapshots != 0 {
		t.Errorf("CountUserResources = %+v, want {Vms:2 Templates:1 Snapshots:0}", got)
	}
}
