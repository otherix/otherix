// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

func mustUUID() uuid.UUID { return uuid.New() }

// bootstrapEnv builds a BootstrapAdminEnv that returns canned values.
// An empty string for either argument simulates an unset env var.
func bootstrapEnv(email, password string) api.BootstrapAdminEnv {
	return func(key string) string {
		switch key {
		case api.EnvBootstrapAdminEmail:
			return email
		case api.EnvBootstrapAdminPassword:
			return password
		default:
			return ""
		}
	}
}

// freshStore returns a *store.Store backed by the shared Postgres
// container, with all admin rows soft-deleted so CountAdmins starts at
// zero. Caller closes via t.Cleanup.
func freshStore(t *testing.T) *store.Store {
	t.Helper()
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN: sharedHarness.DSN, MaxConns: 4, MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(s.Close)

	if _, err := s.Pool().Exec(context.Background(),
		"update users set deleted_at = now() where role = 'admin' and deleted_at is null"); err != nil {
		t.Fatalf("reset admins: %v", err)
	}
	return s
}

func TestBootstrapAdmin_NoEnv_NoOp(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := api.BootstrapAdminWithEnv(context.Background(), s, log, bootstrapEnv("", "")); err != nil {
		t.Fatalf("BootstrapAdminWithEnv: %v", err)
	}
	count, err := s.Queries().CountAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if count != 0 {
		t.Errorf("admin count after no-env bootstrap = %d, want 0", count)
	}
}

func TestBootstrapAdmin_PartialEnvFails(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := api.BootstrapAdminWithEnv(context.Background(), s, log, bootstrapEnv("admin@local.test", ""))
	if err == nil || !strings.Contains(err.Error(), "requires both") {
		t.Errorf("err = %v, want 'requires both' substring", err)
	}
}

func TestBootstrapAdmin_InvalidEmail(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := api.BootstrapAdminWithEnv(context.Background(), s, log,
		bootstrapEnv("not-an-email", "long-enough-password-1"))
	if err == nil || !strings.Contains(err.Error(), "email") {
		t.Errorf("err = %v, want email validation error", err)
	}
}

func TestBootstrapAdmin_ShortPassword(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := api.BootstrapAdminWithEnv(context.Background(), s, log,
		bootstrapEnv("admin@local.test", "short"))
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Errorf("err = %v, want password length error", err)
	}
}

func TestBootstrapAdmin_CreatesAdmin(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	const email = "admin-bootstrap@local.test"
	const password = "correct-horse-battery-staple"

	if err := api.BootstrapAdminWithEnv(context.Background(), s, log,
		bootstrapEnv(email, password)); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	count, err := s.Queries().CountAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if count != 1 {
		t.Errorf("admin count after bootstrap = %d, want 1", count)
	}

	got, err := s.Queries().GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if got.Role != string(auth.RoleAdmin) {
		t.Errorf("role = %q, want admin", got.Role)
	}
	ok, err := auth.VerifyPassword(got.PasswordHash, password)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword = false, want true")
	}
}

func TestBootstrapAdmin_Idempotent(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	const email = "admin-idempotent@local.test"
	const password = "correct-horse-battery-staple"
	env := bootstrapEnv(email, password)

	for i := 0; i < 3; i++ {
		if err := api.BootstrapAdminWithEnv(context.Background(), s, log, env); err != nil {
			t.Fatalf("bootstrap call %d: %v", i+1, err)
		}
	}

	count, err := s.Queries().CountAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if count != 1 {
		t.Errorf("admin count after 3 bootstraps = %d, want 1", count)
	}
}

func TestBootstrapAdmin_SkipsWhenAdminExists(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Pre-existing admin (different email than the bootstrap config)
	// must block creation of the seed admin.
	hash, err := auth.HashPassword("preexisting-admin-password-1")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	preExisting, err := s.Queries().CreateUser(context.Background(), store.CreateUserParams{
		ID: mustUUID(), Email: "preexisting@local.test", PasswordHash: hash, Role: string(auth.RoleAdmin),
	})
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	if err := api.BootstrapAdminWithEnv(context.Background(), s, log,
		bootstrapEnv("would-be-bootstrapped@local.test", "correct-horse-battery-staple")); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	count, err := s.Queries().CountAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if count != 1 {
		t.Errorf("admin count = %d, want 1 (skip path should not create)", count)
	}
	if _, err := s.Queries().GetUserByEmail(context.Background(), preExisting.Email); err != nil {
		t.Errorf("preexisting admin gone: %v", err)
	}

	// Allow each test in this file to start from a clean slate.
	if err := s.Queries().SoftDeleteUser(context.Background(), preExisting.ID); err != nil {
		t.Fatalf("cleanup soft delete: %v", err)
	}
}

func TestBootstrapAdmin_ContextRespected(t *testing.T) {
	s := freshStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := api.BootstrapAdminWithEnv(ctx, s, log,
		bootstrapEnv("ctx-admin@local.test", "correct-horse-battery-staple")); err != nil {
		t.Fatalf("BootstrapAdminWithEnv: %v", err)
	}
}
