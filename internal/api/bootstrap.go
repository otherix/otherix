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
// *etcdstore.Store satisfies it.
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
		log.WarnContext(ctx, "bootstrap admin env is set but an admin already exists; "+
			"remove OTHERIX_BOOTSTRAP_ADMIN_EMAIL and OTHERIX_BOOTSTRAP_ADMIN_PASSWORD "+
			"(e.g. from /etc/otherix/api.env) so the bootstrap credentials no longer sit in the environment",
			slog.Int64("admin_count", count),
			slog.String("email_var", EnvBootstrapAdminEmail),
			slog.String("password_var", EnvBootstrapAdminPassword))
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
		// Seed a human-readable display name so audit surfaces (e.g. the
		// migration cancel record's "cancelled by <name>") show "admin" rather
		// than the bootstrap email (PII) or the opaque user id.
		DisplayName: "admin",
		Role:        string(auth.RoleAdmin),
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
