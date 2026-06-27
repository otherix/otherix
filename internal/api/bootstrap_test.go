// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// existingAdminStoreStub reports that an admin already exists and records whether
// CreateUser was called. The already-exists path must never create a user.
type existingAdminStoreStub struct {
	createCalled bool
}

func (s *existingAdminStoreStub) CountAdmins(context.Context) (int64, error) {
	return 1, nil
}

func (s *existingAdminStoreStub) CreateUser(context.Context, store.CreateUserParams) (store.User, error) {
	s.createCalled = true
	return store.User{}, nil
}

// captureRecord is a single slog record captured by captureHandler.
type captureRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// captureHandler records every slog record (level, message, attrs) into a slice
// so a test can assert on the emitted log without an assertion library.
type captureHandler struct {
	records *[]captureRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := captureRecord{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	*h.records = append(*h.records, rec)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// TestBootstrapAdmin_ExistingAdminWarnsToRemoveEnv asserts that when an admin
// already exists and the bootstrap env vars are still set, BootstrapAdminWithEnv
// returns nil without creating a user and emits a WARN naming both env vars so
// the operator removes them, rather than logging silently at INFO.
func TestBootstrapAdmin_ExistingAdminWarnsToRemoveEnv(t *testing.T) {
	stub := &existingAdminStoreStub{}
	var records []captureRecord
	log := slog.New(&captureHandler{records: &records})

	env := func(key string) string {
		switch key {
		case api.EnvBootstrapAdminUsername:
			return "admin"
		case api.EnvBootstrapAdminPassword:
			return "correct horse battery staple"
		default:
			return ""
		}
	}

	if err := api.BootstrapAdminWithEnv(context.Background(), stub, log, env); err != nil {
		t.Fatalf("BootstrapAdminWithEnv() = %v, want nil", err)
	}

	if stub.createCalled {
		t.Errorf("CreateUser was called, want not called when an admin already exists")
	}

	var warns []captureRecord
	for _, rec := range records {
		if rec.level == slog.LevelWarn {
			warns = append(warns, rec)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("WARN records = %d, want 1 (records: %+v)", len(warns), records)
	}

	w := warns[0]
	if got := w.attrs["username_var"]; got != api.EnvBootstrapAdminUsername {
		t.Errorf("username_var = %v, want %v", got, api.EnvBootstrapAdminUsername)
	}
	if got := w.attrs["password_var"]; got != api.EnvBootstrapAdminPassword {
		t.Errorf("password_var = %v, want %v", got, api.EnvBootstrapAdminPassword)
	}
}

// recordingStoreStub reports no existing admin and captures the params passed to
// CreateUser so a test can assert the bootstrapped admin shape.
type recordingStoreStub struct {
	gotParams store.CreateUserParams
}

func (s *recordingStoreStub) CountAdmins(context.Context) (int64, error) {
	return 0, nil
}

func (s *recordingStoreStub) CreateUser(_ context.Context, arg store.CreateUserParams) (store.User, error) {
	s.gotParams = arg
	return store.User{ID: arg.ID, Username: arg.Username, DisplayName: arg.DisplayName, Role: arg.Role}, nil
}

// TestBootstrapAdmin_CreatesAdminByUsername asserts that with both env vars set
// and no existing admin, BootstrapAdminWithEnv creates an admin keyed on the
// username (not an email), with the admin role and a non-empty display name.
func TestBootstrapAdmin_CreatesAdminByUsername(t *testing.T) {
	stub := &recordingStoreStub{}
	log := slog.New(&captureHandler{records: &[]captureRecord{}})

	env := func(key string) string {
		switch key {
		case api.EnvBootstrapAdminUsername:
			return "admin"
		case api.EnvBootstrapAdminPassword:
			return "correct horse battery staple"
		default:
			return ""
		}
	}

	if err := api.BootstrapAdminWithEnv(context.Background(), stub, log, env); err != nil {
		t.Fatalf("BootstrapAdminWithEnv() = %v, want nil", err)
	}

	if got := stub.gotParams.Username; got != "admin" {
		t.Errorf("created admin username = %q, want %q", got, "admin")
	}
	if got := stub.gotParams.Email; got != "" {
		t.Errorf("created admin email = %q, want empty", got)
	}
	if got := stub.gotParams.Role; got != string(auth.RoleAdmin) {
		t.Errorf("created admin role = %q, want %q", got, string(auth.RoleAdmin))
	}
	if got := stub.gotParams.DisplayName; got != "admin" {
		t.Errorf("created admin display_name = %q, want %q", got, "admin")
	}
}

// TestBootstrapAdmin_InvalidUsername asserts that a syntactically invalid
// bootstrap username is rejected before any user is created.
func TestBootstrapAdmin_InvalidUsername(t *testing.T) {
	stub := &recordingStoreStub{}
	log := slog.New(&captureHandler{records: &[]captureRecord{}})

	env := func(key string) string {
		switch key {
		case api.EnvBootstrapAdminUsername:
			return "Bad Name"
		case api.EnvBootstrapAdminPassword:
			return "correct horse battery staple"
		default:
			return ""
		}
	}

	if err := api.BootstrapAdminWithEnv(context.Background(), stub, log, env); err == nil {
		t.Fatalf("BootstrapAdminWithEnv() = nil, want error for invalid username")
	}
	if stub.gotParams.Username != "" {
		t.Errorf("CreateUser was called with %q, want not called for invalid username", stub.gotParams.Username)
	}
}

// TestBootstrapAdmin_BothUnsetSkips asserts that with neither env var set the
// bootstrap is a no-op returning nil.
func TestBootstrapAdmin_BothUnsetSkips(t *testing.T) {
	stub := &recordingStoreStub{}
	log := slog.New(&captureHandler{records: &[]captureRecord{}})

	env := func(string) string { return "" }

	if err := api.BootstrapAdminWithEnv(context.Background(), stub, log, env); err != nil {
		t.Fatalf("BootstrapAdminWithEnv() = %v, want nil", err)
	}
	if stub.gotParams.Username != "" {
		t.Errorf("CreateUser was called, want not called when env unset")
	}
}

// TestBootstrapAdmin_OnlyUsernameSetIsFatal asserts that setting the username
// without the password is a fatal misconfiguration.
func TestBootstrapAdmin_OnlyUsernameSetIsFatal(t *testing.T) {
	stub := &recordingStoreStub{}
	log := slog.New(&captureHandler{records: &[]captureRecord{}})

	env := func(key string) string {
		if key == api.EnvBootstrapAdminUsername {
			return "admin"
		}
		return ""
	}

	if err := api.BootstrapAdminWithEnv(context.Background(), stub, log, env); err == nil {
		t.Fatalf("BootstrapAdminWithEnv() = nil, want error when only username set")
	}
}
