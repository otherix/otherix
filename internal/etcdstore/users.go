// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Users are addressed by UUID. The username uniqueness guard
// (uniq/users/username/<username>) is the primary handle index: it is always
// written, its value is the user id, and it backs UserByUsername. A second,
// unique-if-present email guard (uniq/users/email/<lower(email)>, partial on
// deleted_at) backs UserByEmail and is written only when the email is
// non-empty (email is optional). Both guard values are the user id; the user
// row value carries password_hash (the source of truth for auth) which
// handlers strip at the view boundary.

func userKey(id uuid.UUID) string { return etcd.Key("users", id.String()) }

func userPrefix() string { return etcd.Key("users") + "/" }

func userUsernameGuard(username string) string {
	return etcd.Key("uniq", "users", "username", username) // already lowercase by validation
}

func userEmailGuard(email string) string {
	return etcd.Key("uniq", "users", "email", strings.ToLower(email))
}

// Owner-index prefixes consulted by CountUserResources. They are written by the
// vms / snapshots slices; until those land the counts are zero.
func vmsOwnerPrefix(id uuid.UUID) string {
	return etcd.Key("index", "vms", "owner", id.String()) + "/"
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

// UserByUsername returns the non-deleted user with the given username by
// resolving the username guard to an id, or store.ErrNotFound. The guard is
// dropped on soft-delete, so a deleted username resolves to not-found.
func (s *Store) UserByUsername(ctx context.Context, username string) (store.User, error) {
	idBytes, found, err := s.c.Get(ctx, userUsernameGuard(username))
	if err != nil {
		return store.User{}, err
	}
	if !found {
		return store.User{}, store.ErrNotFound
	}
	id, perr := uuid.Parse(string(idBytes))
	if perr != nil {
		return store.User{}, fmt.Errorf("corrupt username guard for %q: %v", username, perr)
	}
	return s.UserByID(ctx, id)
}

// CreateUser inserts a user, stamping created_at/updated_at, writing the user
// row, the username guard (always), and the email guard (only when the email
// is non-empty) atomically. A username collision among non-deleted rows
// returns store.ErrUserUsernameExists; a case-insensitive email collision
// returns store.ErrUserEmailExists.
func (s *Store) CreateUser(ctx context.Context, arg store.CreateUserParams) (store.User, error) {
	now := time.Now().UTC()
	u := store.User{
		ID:           arg.ID,
		Username:     arg.Username,
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
	usernameGuard := userUsernameGuard(u.Username)
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(usernameGuard), "=", 0)}
	ops := []clientv3.Op{
		clientv3.OpPut(userKey(u.ID), string(val)),
		clientv3.OpPut(usernameGuard, u.ID.String()),
	}
	if u.Email != "" {
		emailGuard := userEmailGuard(u.Email)
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(emailGuard), "=", 0))
		ops = append(ops, clientv3.OpPut(emailGuard, u.ID.String()))
	}
	resp, err := s.c.Raw().Txn(ctx).If(cmps...).Then(ops...).Commit()
	if err != nil {
		return store.User{}, fmt.Errorf("create user txn: %v", err)
	}
	if !resp.Succeeded {
		// The write was atomic; re-read only to choose which sentinel to
		// report. The username guard is always written, so it is the dominant
		// collision cause; if neither guard is present (a concurrent delete
		// removed the winner between the failed commit and this read) default
		// to the username sentinel rather than a misleading email one.
		if _, found, gerr := s.c.Get(ctx, usernameGuard); gerr == nil && found {
			return store.User{}, store.ErrUserUsernameExists
		}
		if u.Email != "" {
			if _, found, gerr := s.c.Get(ctx, userEmailGuard(u.Email)); gerr == nil && found {
				return store.User{}, store.ErrUserEmailExists
			}
		}
		return store.User{}, store.ErrUserUsernameExists
	}
	return u, nil
}

// UpdateUser rewrites email, password_hash, display_name, and role, bumps
// updated_at, and moves the email guard when the email changes. Username is
// immutable, so its guard is never moved. Returns store.ErrNotFound when the
// row is missing and store.ErrUserEmailExists when a changed email collides
// with another live user. (In current flows the handler keeps email constant,
// so the guard move is a no-op; it is handled for correctness.)
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

	// Email is optional and immutable in current flows; only move the email
	// guard when the email actually changed. An empty email has no guard to
	// write or delete (two no-email users must not share the empty-email key).
	if existing.Email == arg.Email {
		if err := s.c.Put(ctx, userKey(arg.ID), val); err != nil {
			return store.User{}, err
		}
		return updated, nil
	}
	cmps := []clientv3.Cmp{}
	ops := []clientv3.Op{clientv3.OpPut(userKey(arg.ID), string(val))}
	if arg.Email != "" {
		newGuard := userEmailGuard(arg.Email)
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0))
		ops = append(ops, clientv3.OpPut(newGuard, arg.ID.String()))
	}
	if existing.Email != "" {
		ops = append(ops, clientv3.OpDelete(userEmailGuard(existing.Email)))
	}
	resp, err := s.c.Raw().Txn(ctx).If(cmps...).Then(ops...).Commit()
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

// CountUserResources counts the live vms / snapshots owned by the user via
// their owner-index prefixes, for the handler's delete precheck.
func (s *Store) CountUserResources(ctx context.Context, id uuid.UUID) (store.CountUserResourcesRow, error) {
	vms, err := s.countPrefix(ctx, vmsOwnerPrefix(id))
	if err != nil {
		return store.CountUserResourcesRow{}, err
	}
	snapshots, err := s.countPrefix(ctx, snapshotsOwnerPrefix(id))
	if err != nil {
		return store.CountUserResourcesRow{}, err
	}
	return store.CountUserResourcesRow{Vms: vms, Snapshots: snapshots}, nil
}

// DeleteUser soft-deletes the user (sets deleted_at, drops the username guard
// and, when present, the email guard, so both handles are reusable) and revokes
// every api token the user owns, all in one
// transaction - matching the SQL DeleteUser's RevokeApiTokensForUser +
// SoftDeleteUser InTx. The caller (handler) performs the owned-resource refusal
// via CountUserResources first.
//
// The per-token revokes are stamped from a snapshot read of the by-user index;
// a token created concurrently after that read is not revoked here, but it
// cannot authenticate either - the user is soft-deleted and
// auth.Service.VerifyAPIToken rejects on the UserByID -> ErrNotFound lookup.
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

	ops := []clientv3.Op{
		clientv3.OpPut(userKey(id), string(val)),
		clientv3.OpDelete(userUsernameGuard(existing.Username)),
	}
	if existing.Email != "" {
		ops = append(ops, clientv3.OpDelete(userEmailGuard(existing.Email)))
	}
	revokeOps, err := s.revokeUserAPITokenOps(ctx, id, now)
	if err != nil {
		return err
	}
	ops = append(ops, revokeOps...)

	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("delete user txn: %v", err)
	}
	return nil
}

// revokeUserAPITokenOps returns the put ops that stamp revoked_at on every
// not-yet-revoked api token owned by the user, read from the by-user index.
// Returned for inclusion in the DeleteUser txn so the revocation is atomic with
// the soft-delete. Already-revoked tokens are skipped (idempotent).
func (s *Store) revokeUserAPITokenOps(ctx context.Context, userID uuid.UUID, revokedAt time.Time) ([]clientv3.Op, error) {
	items, err := s.c.Range(ctx, apiTokenUserIndexPrefix(userID))
	if err != nil {
		return nil, err
	}
	ops := make([]clientv3.Op, 0, len(items))
	for _, kv := range items {
		tokenID, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt api token user index %q: %v", kv.Key, perr)
		}
		tok, err := s.APITokenByID(ctx, tokenID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if tok.RevokedAt != nil {
			continue
		}
		tok.RevokedAt = &revokedAt
		val, err := etcd.Marshal(tok)
		if err != nil {
			return nil, err
		}
		ops = append(ops, clientv3.OpPut(apiTokenKey(tokenID), string(val)))
	}
	return ops, nil
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
