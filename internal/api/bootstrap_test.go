// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/otherix/otherix/internal/api"
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
		case api.EnvBootstrapAdminEmail:
			return "admin@otherix.local"
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
	if got := w.attrs["email_var"]; got != api.EnvBootstrapAdminEmail {
		t.Errorf("email_var = %v, want %v", got, api.EnvBootstrapAdminEmail)
	}
	if got := w.attrs["password_var"]; got != api.EnvBootstrapAdminPassword {
		t.Errorf("password_var = %v, want %v", got, api.EnvBootstrapAdminPassword)
	}
}
