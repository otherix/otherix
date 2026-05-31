// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

type userView struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func TestUsersCRUDAsAdmin(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Create.
	body := map[string]string{
		"email":        "crud-" + uuid.NewString()[:8] + "@example.test",
		"password":     "correct-horse-battery-staple",
		"display_name": "CRUD User",
		"role":         "developer",
	}
	resp := h.post(t, "/v1/users", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created userView
	decodeJSON(t, resp, &created)
	if created.ID == "" || created.Email != body["email"] || created.Role != "developer" {
		t.Fatalf("create view = %+v", created)
	}

	// Get by id.
	resp = h.get(t, "/v1/users/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got userView
	decodeJSON(t, resp, &got)
	if got.ID != created.ID {
		t.Errorf("get id = %q, want %q", got.ID, created.ID)
	}

	// List includes the new user.
	resp = h.get(t, "/v1/users", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Data []userView `json:"data"`
	}
	decodeJSON(t, resp, &list)
	found := false
	for _, u := range list.Data {
		if u.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("list did not contain created user %s", created.ID)
	}

	// Patch role.
	resp = h.patch(t, "/v1/users/"+created.ID, map[string]string{"role": "operator"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var patched userView
	decodeJSON(t, resp, &patched)
	if patched.Role != "operator" {
		t.Errorf("patched role = %q, want operator", patched.Role)
	}

	// Delete.
	resp = h.delete(t, "/v1/users/"+created.ID, admin)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 204/200", resp.StatusCode)
	}
	resp.Body.Close()

	// Gone.
	resp = h.get(t, "/v1/users/"+created.ID, admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUsersCreateForbiddenForDeveloper(t *testing.T) {
	h := newE2E(t)
	dev, _ := loginAs(t, h, auth.RoleDeveloper)
	resp := h.post(t, "/v1/users", map[string]string{
		"email": "x@example.test", "password": "correct-horse-battery-staple",
		"display_name": "X", "role": "developer",
	}, dev)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("developer create status = %d, want 403", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodePermissionDenied {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodePermissionDenied)
	}
}

func TestUsersGetMissingIs404(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/users/"+uuid.NewString(), admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get missing status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUsersMe(t *testing.T) {
	h := newE2E(t)
	token, id := loginAs(t, h, auth.RoleOperator)
	resp := h.get(t, "/v1/users/me", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get me status = %d, want 200", resp.StatusCode)
	}
	var me userView
	decodeJSON(t, resp, &me)
	if me.ID != id.String() {
		t.Errorf("me id = %q, want %q", me.ID, id.String())
	}
	if me.Role != "operator" {
		t.Errorf("me role = %q, want operator", me.Role)
	}
}
