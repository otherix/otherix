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
		Username:     "user-" + uuid.NewString()[:8],
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

func TestUserDeleteRevokesAPITokens(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := userParams(uniqueEmail("tokowner"))
	if _, err := s.CreateUser(ctx, p); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t1, err := s.CreateAPIToken(ctx, store.CreateApiTokenParams{
		ID: uuid.New(), UserID: p.ID, Name: "tok-1", TokenHash: []byte("del-hash-1"), Prefix: "otx_aaaa",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken 1: %v", err)
	}
	t2, err := s.CreateAPIToken(ctx, store.CreateApiTokenParams{
		ID: uuid.New(), UserID: p.ID, Name: "tok-2", TokenHash: []byte("del-hash-2"), Prefix: "otx_bbbb",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken 2: %v", err)
	}

	if err := s.DeleteUser(ctx, p.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// Both tokens must be revoked, matching the SQL DeleteUser's
	// RevokeApiTokensForUser cascade.
	for _, id := range []uuid.UUID{t1.ID, t2.ID} {
		got, err := s.APITokenByID(ctx, id)
		if err != nil {
			t.Fatalf("APITokenByID(%v) after user delete: %v", id, err)
		}
		if got.RevokedAt == nil {
			t.Errorf("APIToken %v revoked_at = nil after user delete, want stamped", id)
		}
	}
	// The revoked token no longer authenticates by hash.
	if _, err := s.APITokenByHash(ctx, []byte("del-hash-1")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("APITokenByHash after user delete = %v, want store.ErrNotFound", err)
	}
}

func TestUserByUsername(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	created, err := s.CreateUser(ctx, store.CreateUserParams{
		ID: uuid.New(), Username: "alice", PasswordHash: "x", Role: "developer",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := s.UserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("UserByUsername id = %v, want %v", got.ID, created.ID)
	}
	if _, err := s.UserByUsername(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByUsername(nobody) err = %v, want ErrNotFound", err)
	}
}

func TestCreateUserUsernameConflict(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "alice", PasswordHash: "x", Role: "developer"}); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "alice", PasswordHash: "y", Role: "viewer"})
	if !errors.Is(err, store.ErrUserUsernameExists) {
		t.Errorf("duplicate username err = %v, want ErrUserUsernameExists", err)
	}
}

func TestCreateUserEmailOptional(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	// no email -> succeeds, and two users with empty email do NOT collide
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "user-one", PasswordHash: "x", Role: "viewer"}); err != nil {
		t.Fatalf("create user-one (no email): %v", err)
	}
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "user-two", PasswordHash: "x", Role: "viewer"}); err != nil {
		t.Fatalf("create user-two (no email): %v", err)
	}
	// email present -> still unique-if-present
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "user-three", Email: "e@x.io", PasswordHash: "x", Role: "viewer"}); err != nil {
		t.Fatalf("create user-three (with email): %v", err)
	}
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "user-four", Email: "e@x.io", PasswordHash: "x", Role: "viewer"}); !errors.Is(err, store.ErrUserEmailExists) {
		t.Errorf("duplicate email err = %v, want ErrUserEmailExists", err)
	}
}

func TestDeleteUserNoEmailDoesNotClobberSharedKey(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	// two no-email users must coexist; deleting one must not drop a shared empty email key
	a, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "no-mail-a", PasswordHash: "x", Role: "viewer"})
	if err != nil {
		t.Fatalf("first no-email create: %v", err)
	}
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "no-mail-b", PasswordHash: "x", Role: "viewer"}); err != nil {
		t.Fatalf("second no-email create: %v", err)
	}
	if err := s.DeleteUser(ctx, a.ID); err != nil {
		t.Fatalf("DeleteUser(a): %v", err)
	}
	// b is still resolvable; a third no-email create still succeeds (no orphaned/clobbered key)
	if _, err := s.UserByUsername(ctx, "no-mail-b"); err != nil {
		t.Errorf("no-mail-b lookup after deleting a: %v", err)
	}
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "no-mail-c", PasswordHash: "x", Role: "viewer"}); err != nil {
		t.Errorf("third no-email create after delete: %v", err)
	}
}

func TestUpdateUserNoEmailIsNoop(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "no-mail-u", PasswordHash: "x", DisplayName: "U", Role: "viewer"})
	if err != nil {
		t.Fatalf("CreateUser(no-email): %v", err)
	}
	// update a no-email user's role/display_name; must not touch any email guard
	if _, err := s.UpdateUser(ctx, store.UpdateUserParams{ID: u.ID, DisplayName: "U2", Role: "operator", PasswordHash: "x"}); err != nil {
		t.Fatalf("UpdateUser(no-email): %v", err)
	}
	if _, err := s.CreateUser(ctx, store.CreateUserParams{ID: uuid.New(), Username: "no-mail-v", PasswordHash: "x", Role: "viewer"}); err != nil {
		t.Errorf("no-email create after a no-email update: %v", err)
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
	if zero.Vms != 0 || zero.Snapshots != 0 {
		t.Errorf("CountUserResources(no owned) = %+v, want all zero", zero)
	}

	// Seed two owned-vm index entries.
	for i := 0; i < 2; i++ {
		k := etcd.Key("index", "vms", "owner", p.ID.String(), uuid.NewString())
		if err := cli.Put(ctx, k, []byte("vm")); err != nil {
			t.Fatalf("seed vm index: %v", err)
		}
	}

	got, err := s.CountUserResources(ctx, p.ID)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if got.Vms != 2 || got.Snapshots != 0 {
		t.Errorf("CountUserResources = %+v, want {Vms:2 Snapshots:0}", got)
	}
}
