// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrUserEmailExists is returned when a user with the same email
// already exists among non-deleted rows (uq_users_email).
var ErrUserEmailExists = errors.New("store: user email already in use")

// translateUserUnique maps a unique-constraint violation on the users
// table to ErrUserEmailExists; any other error passes through. The only
// surfaced unique constraint is the partial email index.
func translateUserUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrUserEmailExists
	}
	return err
}

// UserByID returns the non-deleted user with the given id, or
// ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := s.queries.GetUserByID(ctx, id)
	if err != nil {
		return User{}, translateNoRows(err)
	}
	return u, nil
}

// UserByEmail returns the non-deleted user with the given email
// (case-insensitive), or ErrNotFound.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	u, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		return User{}, translateNoRows(err)
	}
	return u, nil
}

// CreateUser inserts a user, translating the email-uniqueness
// violation to ErrUserEmailExists.
func (s *Store) CreateUser(ctx context.Context, arg CreateUserParams) (User, error) {
	u, err := s.queries.CreateUser(ctx, arg)
	if err != nil {
		return User{}, translateUserUnique(err)
	}
	return u, nil
}

// UpdateUser updates a user. Email is not mutated through this path, so
// no uniqueness translation is applied.
func (s *Store) UpdateUser(ctx context.Context, arg UpdateUserParams) (User, error) {
	return s.queries.UpdateUser(ctx, arg)
}

// ListUsers returns users matching the cursor-paginated params.
func (s *Store) ListUsers(ctx context.Context, arg ListUsersParams) ([]User, error) {
	return s.queries.ListUsers(ctx, arg)
}

// CountUserResources returns the count of owned vms, templates and
// snapshots for the user. Used by the delete path to refuse removing a
// user who still owns resources (those FKs are ON DELETE RESTRICT).
func (s *Store) CountUserResources(ctx context.Context, id uuid.UUID) (CountUserResourcesRow, error) {
	return s.queries.CountUserResources(ctx, id)
}

// DeleteUser revokes every API token the user owns and soft-deletes the
// user row in a single transaction. The caller is responsible for the
// pre-delete checks (self-delete guard, owned-resource refusal); this
// method performs only the atomic mutation.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	return s.InTx(ctx, func(q *Queries) error {
		if err := q.RevokeApiTokensForUser(ctx, id); err != nil {
			return err
		}
		return q.SoftDeleteUser(ctx, id)
	})
}

// CountAdmins returns the number of non-deleted admin users. Used by the
// boot-time admin bootstrap to decide whether to seed the first admin.
func (s *Store) CountAdmins(ctx context.Context) (int64, error) {
	return s.queries.CountAdmins(ctx)
}
