// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Environment variables consulted by BootstrapAdmin.
const (
	EnvBootstrapAdminEmail    = "OTHERIX_BOOTSTRAP_ADMIN_EMAIL"
	EnvBootstrapAdminPassword = "OTHERIX_BOOTSTRAP_ADMIN_PASSWORD" //nolint:gosec // env var name, not a credential
)

// BootstrapAdminEnv is the env-vars contract BootstrapAdminWithEnv reads.
// Pass os.Getenv for production wiring; tests pass a closure with canned
// values so they need not mutate process state.
type BootstrapAdminEnv func(string) string

// AdminBootstrapStore is the storage surface the admin bootstrap depends on.
// Both *store.Store (pgx) and *etcdstore.Store satisfy it, so the boot hook is
// backend-agnostic.
type AdminBootstrapStore interface {
	CountAdmins(ctx context.Context) (int64, error)
	CreateUser(ctx context.Context, arg store.CreateUserParams) (store.User, error)
}

// BootstrapAdmin seeds the first admin user when both env vars are set
// and the database has no admin row. The function is idempotent: a
// repeat call against a database that already contains an admin is a
// no-op.
//
// Call BootstrapAdmin after migrations have been applied and before
// the HTTP server starts.
func BootstrapAdmin(ctx context.Context, s AdminBootstrapStore, log *slog.Logger) error {
	return BootstrapAdminWithEnv(ctx, s, log, getenv)
}

// BootstrapAdminWithEnv is the env-injectable form of BootstrapAdmin.
// Production wiring goes through BootstrapAdmin; tests use this entry
// point with a stub env reader.
func BootstrapAdminWithEnv(ctx context.Context, s AdminBootstrapStore, log *slog.Logger, env BootstrapAdminEnv) error {
	email := strings.TrimSpace(env(EnvBootstrapAdminEmail))
	password := env(EnvBootstrapAdminPassword)

	if email == "" && password == "" {
		log.InfoContext(ctx, "bootstrap admin skipped: env vars not set",
			slog.String("email_var", EnvBootstrapAdminEmail),
			slog.String("password_var", EnvBootstrapAdminPassword))
		return nil
	}
	if email == "" || password == "" {
		return fmt.Errorf("bootstrap admin requires both %s and %s",
			EnvBootstrapAdminEmail, EnvBootstrapAdminPassword)
	}

	if err := validation.ValidateEmail(email); err != nil {
		return fmt.Errorf("bootstrap admin email: %v", err)
	}
	if err := validation.ValidatePassword(password); err != nil {
		return fmt.Errorf("bootstrap admin password: %v", err)
	}

	count, err := s.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("count admins: %v", err)
	}
	if count > 0 {
		log.InfoContext(ctx, "bootstrap admin skipped: admin user already exists",
			slog.Int64("admin_count", count))
		return nil
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %v", err)
	}

	row, err := s.CreateUser(ctx, store.CreateUserParams{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: hash,
		Role:         string(auth.RoleAdmin),
	})
	if err != nil {
		// A unique violation here means a concurrent process beat us
		// to the insert. Treat that as success: the requested email is
		// taken, an admin row exists, the goal is met.
		if errors.Is(err, store.ErrUserEmailExists) {
			log.WarnContext(ctx, "bootstrap admin: email already taken, continuing",
				slog.String("email", email))
			return nil
		}
		return fmt.Errorf("create admin: %v", err)
	}

	log.InfoContext(ctx, "bootstrap admin created",
		slog.String("email", email),
		slog.String("user_id", row.ID.String()))
	return nil
}
