// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
)

func TestCheckOwnership_AnyScope_ForeignResource(t *testing.T) {
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin}
	other := uuid.New()
	if err := auth.CheckOwnership(caller, &other, auth.PermVMUpdate); err != nil {
		t.Errorf("admin update on foreign VM returned %v, want nil", err)
	}
}

func TestCheckOwnership_OwnScope_OwnResource(t *testing.T) {
	uid := uuid.New()
	caller := &auth.User{ID: uid, Role: auth.RoleDeveloper}
	if err := auth.CheckOwnership(caller, &uid, auth.PermVMUpdate); err != nil {
		t.Errorf("developer update on own VM returned %v, want nil", err)
	}
}

func TestCheckOwnership_OwnScope_ForeignResource(t *testing.T) {
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper}
	other := uuid.New()
	err := auth.CheckOwnership(caller, &other, auth.PermVMUpdate)
	if !errors.Is(err, auth.ErrPermissionDenied) {
		t.Errorf("developer update on foreign VM returned %v, want ErrPermissionDenied", err)
	}
}

func TestCheckOwnership_PermissionMissing(t *testing.T) {
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleViewer}
	owner := caller.ID
	err := auth.CheckOwnership(caller, &owner, auth.PermVMUpdate)
	if !errors.Is(err, auth.ErrPermissionDenied) {
		t.Errorf("viewer update on own resource returned %v, want ErrPermissionDenied", err)
	}
}

func TestCheckOwnership_NilUser(t *testing.T) {
	owner := uuid.New()
	err := auth.CheckOwnership(nil, &owner, auth.PermVMUpdate)
	if !errors.Is(err, auth.ErrPermissionDenied) {
		t.Errorf("nil user returned %v, want ErrPermissionDenied", err)
	}
}

func TestCheckOwnership_OwnScope_NilOwnerIsProgrammerError(t *testing.T) {
	// Pairing scope=own with a resource that has no owner_id is a
	// matrix bug, not a runtime authorisation outcome. Surface it as a
	// non-sentinel error so the caller can panic / log loudly rather
	// than silently denying.
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper}
	err := auth.CheckOwnership(caller, nil, auth.PermVMUpdate)
	if err == nil {
		t.Fatal("expected error for own-scope on ownerless resource, got nil")
	}
	if errors.Is(err, auth.ErrPermissionDenied) {
		t.Errorf("expected programmer-error sentinel, got ErrPermissionDenied (%v)", err)
	}
}

func TestCheckOwnership_AnyScope_NilOwner(t *testing.T) {
	// any scope with a nil owner is fine — admin should always succeed.
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin}
	if err := auth.CheckOwnership(caller, nil, auth.PermVMUpdate); err != nil {
		t.Errorf("admin any-scope with nil owner returned %v, want nil", err)
	}
}
