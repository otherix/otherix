// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Users are addressed by UUID with a case-insensitive email uniqueness guard
// (uq_users_email, partial on deleted_at). The guard value is the user id, so
// it doubles as the email->id index that backs UserByEmail. The value carries
// password_hash (the source of truth for auth); handlers strip it at the view
// boundary.

func userKey(id uuid.UUID) string { return etcd.Key("users", id.String()) }

func userPrefix() string { return etcd.Key("users") + "/" }

func userEmailGuard(email string) string {
	return etcd.Key("uniq", "users", "email", strings.ToLower(email))
}

// Owner-index prefixes consulted by CountUserResources. They are written by the
// vms / templates / snapshots slices; until those land the counts are zero.
func vmsOwnerPrefix(id uuid.UUID) string {
	return etcd.Key("index", "vms", "owner", id.String()) + "/"
}

func templatesOwnerPrefix(id uuid.UUID) string {
	return etcd.Key("index", "templates", "owner", id.String()) + "/"
}

func snapshotsOwnerPrefix(id uuid.UUID) string {
	return etcd.Key("index", "snapshots", "owner", id.String()) + "/"
}

// UserByID returns the non-deleted user with the given id, or store.ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (store.User, error) {
	var u store.User
	found, err := s.c.GetJSON(ctx, userKey(id), &u)
	if err != nil {
		return store.User{}, err
	}
	if !found || u.DeletedAt != nil {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

// UserByEmail returns the non-deleted user with the given email
// (case-insensitive) by resolving the email guard to an id, or
// store.ErrNotFound. The guard exists only for live users (dropped on delete),
// so a soft-deleted email resolves to not-found.
func (s *Store) UserByEmail(ctx context.Context, email string) (store.User, error) {
	idBytes, found, err := s.c.Get(ctx, userEmailGuard(email))
	if err != nil {
		return store.User{}, err
	}
	if !found {
		return store.User{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(idBytes))
	if err != nil {
		return store.User{}, fmt.Errorf("corrupt email guard for %q: %v", email, err)
	}
	return s.UserByID(ctx, id)
}

// CreateUser inserts a user, stamping created_at/updated_at, writing the email
// guard + primary atomically. A case-insensitive email collision among
// non-deleted rows returns store.ErrUserEmailExists.
func (s *Store) CreateUser(ctx context.Context, arg store.CreateUserParams) (store.User, error) {
	now := time.Now().UTC()
	u := store.User{
		ID:           arg.ID,
		Email:        arg.Email,
		PasswordHash: arg.PasswordHash,
		DisplayName:  arg.DisplayName,
		Role:         arg.Role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	val, err := etcd.Marshal(u)
	if err != nil {
		return store.User{}, err
	}
	guard := userEmailGuard(u.Email)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, u.ID.String()),
			clientv3.OpPut(userKey(u.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.User{}, fmt.Errorf("create user txn: %v", err)
	}
	if !resp.Succeeded {
		return store.User{}, store.ErrUserEmailExists
	}
	return u, nil
}

// UpdateUser rewrites email, password_hash, display_name, and role, bumps
// updated_at, and moves the email guard when the email changes. Returns
// store.ErrNotFound when the row is missing and store.ErrUserEmailExists when a
// changed email collides with another live user. (In current flows the handler
// keeps email constant, so the guard move is a no-op; it is handled for
// correctness.)
func (s *Store) UpdateUser(ctx context.Context, arg store.UpdateUserParams) (store.User, error) {
	existing, err := s.UserByID(ctx, arg.ID)
	if err != nil {
		return store.User{}, err
	}
	updated := existing
	updated.Email = arg.Email
	updated.PasswordHash = arg.PasswordHash
	updated.DisplayName = arg.DisplayName
	updated.Role = arg.Role
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.User{}, err
	}

	oldGuard := userEmailGuard(existing.Email)
	newGuard := userEmailGuard(arg.Email)
	if oldGuard == newGuard {
		if err := s.c.Put(ctx, userKey(arg.ID), val); err != nil {
			return store.User{}, err
		}
		return updated, nil
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0)).
		Then(
			clientv3.OpPut(newGuard, arg.ID.String()),
			clientv3.OpDelete(oldGuard),
			clientv3.OpPut(userKey(arg.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.User{}, fmt.Errorf("update user txn: %v", err)
	}
	if !resp.Succeeded {
		return store.User{}, store.ErrUserEmailExists
	}
	return updated, nil
}

// ListUsers returns the non-deleted users ordered by (created_at, id)
// ascending, after the cursor, capped at LimitCount - matching the SQL
// ListUsers query. Bounded collection -> primary-prefix scan.
func (s *Store) ListUsers(ctx context.Context, arg store.ListUsersParams) ([]store.User, error) {
	items, err := s.c.Range(ctx, userPrefix())
	if err != nil {
		return nil, err
	}
	users := make([]store.User, 0, len(items))
	for _, kv := range items {
		var u store.User
		if err := json.Unmarshal(kv.Value, &u); err != nil {
			return nil, fmt.Errorf("unmarshal user %q: %v", kv.Key, err)
		}
		if u.DeletedAt != nil {
			continue
		}
		if !afterCursor(u.CreatedAt, u.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool {
		if !users[i].CreatedAt.Equal(users[j].CreatedAt) {
			return users[i].CreatedAt.Before(users[j].CreatedAt)
		}
		return users[i].ID.String() < users[j].ID.String()
	})
	if n := int(arg.LimitCount); n > 0 && len(users) > n {
		users = users[:n]
	}
	return users, nil
}

// CountUserResources counts the live vms / templates / snapshots owned by the
// user via their owner-index prefixes, for the handler's delete precheck.
func (s *Store) CountUserResources(ctx context.Context, id uuid.UUID) (store.CountUserResourcesRow, error) {
	vms, err := s.countPrefix(ctx, vmsOwnerPrefix(id))
	if err != nil {
		return store.CountUserResourcesRow{}, err
	}
	templates, err := s.countPrefix(ctx, templatesOwnerPrefix(id))
	if err != nil {
		return store.CountUserResourcesRow{}, err
	}
	snapshots, err := s.countPrefix(ctx, snapshotsOwnerPrefix(id))
	if err != nil {
		return store.CountUserResourcesRow{}, err
	}
	return store.CountUserResourcesRow{Vms: vms, Templates: templates, Snapshots: snapshots}, nil
}

// DeleteUser soft-deletes the user (sets deleted_at, drops the email guard so
// the address is reusable). The caller (handler) performs the owned-resource
// refusal via CountUserResources first.
//
// TODO(phase-3 auth slice): revoke every api token the user owns, matching the
// SQL DeleteUser's RevokeApiTokensForUser step. There are no api_tokens in the
// etcd store until the auth-tokens slice lands, so this is currently a no-op;
// that slice extends DeleteUser to range the api_token-by-user index and revoke.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	existing, err := s.UserByID(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	existing.DeletedAt = &now
	val, err := etcd.Marshal(existing)
	if err != nil {
		return err
	}
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(userKey(id), string(val)),
			clientv3.OpDelete(userEmailGuard(existing.Email)),
		).
		Commit(); err != nil {
		return fmt.Errorf("delete user txn: %v", err)
	}
	return nil
}

// countPrefix returns the number of keys under prefix.
func (s *Store) countPrefix(ctx context.Context, prefix string) (int64, error) {
	items, err := s.c.Range(ctx, prefix)
	if err != nil {
		return 0, err
	}
	return int64(len(items)), nil
}

// CountAdmins returns the number of non-deleted users with the admin role.
// Backs the boot-time admin bootstrap. The role is stored as the lowercase
// wire string ("admin").
func (s *Store) CountAdmins(ctx context.Context) (int64, error) {
	items, err := s.c.Range(ctx, userPrefix())
	if err != nil {
		return 0, err
	}
	var n int64
	for _, kv := range items {
		var u store.User
		if err := json.Unmarshal(kv.Value, &u); err != nil {
			return 0, fmt.Errorf("unmarshal user %q: %v", kv.Key, err)
		}
		if u.DeletedAt == nil && u.Role == "admin" {
			n++
		}
	}
	return n, nil
}

// TouchUserLastLogin stamps last_login_at on a non-deleted user. A missing or
// soft-deleted user is a no-op (matching the SQL `where deleted_at is null`).
func (s *Store) TouchUserLastLogin(ctx context.Context, id uuid.UUID) error {
	var u store.User
	found, err := s.c.GetJSON(ctx, userKey(id), &u)
	if err != nil {
		return err
	}
	if !found || u.DeletedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	u.LastLoginAt = &now
	return s.c.PutJSON(ctx, userKey(id), u)
}
