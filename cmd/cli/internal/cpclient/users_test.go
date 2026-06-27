// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestCreateUser(t *testing.T) {
	t.Parallel()
	userID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/users" {
			t.Errorf("path = %s, want /v1/users", r.URL.Path)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["username"] != "web-admin" {
			t.Errorf("username = %v, want web-admin", body["username"])
		}
		if body["role"] != "developer" {
			t.Errorf("role = %v, want developer", body["role"])
		}
		if body["email"] != "wa@example.com" {
			t.Errorf("email = %v, want wa@example.com", body["email"])
		}
		if body["display_name"] != "Web Admin" {
			t.Errorf("display_name = %v, want Web Admin", body["display_name"])
		}
		if body["password"] != "supersecretpass" {
			t.Errorf("password = %v, want supersecretpass", body["password"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           userID,
			"username":     "web-admin",
			"email":        "wa@example.com",
			"display_name": "Web Admin",
			"role":         "developer",
			"created_at":   "2026-06-27T10:00:00Z",
			"updated_at":   "2026-06-27T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.CreateUser(context.Background(), cpclient.CreateUserRequest{
		Username:    "web-admin",
		Email:       "wa@example.com",
		DisplayName: "Web Admin",
		Role:        "developer",
		Password:    "supersecretpass",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if got.ID != userID {
		t.Errorf("ID = %s, want %s", got.ID, userID)
	}
	if got.Username != "web-admin" {
		t.Errorf("Username = %s, want web-admin", got.Username)
	}
	if got.Role != "developer" {
		t.Errorf("Role = %s, want developer", got.Role)
	}
}

func TestListUsers_UsernameFilter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/users" {
			t.Errorf("path = %s, want /v1/users", r.URL.Path)
		}
		if got := r.URL.Query().Get("username"); got != "web-admin" {
			t.Errorf("username query = %q, want web-admin", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":         uuid.NewString(),
					"username":   "web-admin",
					"role":       "developer",
					"created_at": "2026-06-27T10:00:00Z",
					"updated_at": "2026-06-27T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListUsers(context.Background(), cpclient.ListUsersParams{Username: "web-admin"})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(got.Data))
	}
	if got.Data[0].Username != "web-admin" {
		t.Errorf("Username = %s, want web-admin", got.Data[0].Username)
	}
}

func TestListUsers_NoUsernameFilter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["username"]; ok {
			t.Errorf("username query present, want absent")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.ListUsers(context.Background(), cpclient.ListUsersParams{}); err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
}

func TestGetUserByUsername_Hit(t *testing.T) {
	t.Parallel()
	userID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("username"); got != "web-admin" {
			t.Errorf("username query = %q, want web-admin", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":         userID,
					"username":   "web-admin",
					"role":       "operator",
					"created_at": "2026-06-27T10:00:00Z",
					"updated_at": "2026-06-27T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetUserByUsername(context.Background(), "web-admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.ID != userID {
		t.Errorf("ID = %s, want %s", got.ID, userID)
	}
	if got.Role != "operator" {
		t.Errorf("Role = %s, want operator", got.Role)
	}
}

// TestGetUserByUsername_EmptyPageIsNotFound locks the M3 invariant: the
// server returns 200 + an empty data array on a username miss (mirroring
// listByEmail, NOT a 404), so the client must translate the empty page to
// the package sentinel ErrNotFound rather than returning a phantom zero row.
func TestGetUserByUsername_EmptyPageIsNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.GetUserByUsername(context.Background(), "ghost")
	if !errors.Is(err, cpclient.ErrNotFound) {
		t.Fatalf("GetUserByUsername err = %v, want ErrNotFound", err)
	}
}

func TestUpdateUser_OnlyNonNilFields(t *testing.T) {
	t.Parallel()
	userID := uuid.NewString()
	role := "operator"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/v1/users/"+userID {
			t.Errorf("path = %s, want /v1/users/%s", r.URL.Path, userID)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["role"] != "operator" {
			t.Errorf("role = %v, want operator", body["role"])
		}
		if _, ok := body["password"]; ok {
			t.Errorf("password present, want absent (nil field must be omitted)")
		}
		if _, ok := body["display_name"]; ok {
			t.Errorf("display_name present, want absent (nil field must be omitted)")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         userID,
			"username":   "web-admin",
			"role":       "operator",
			"created_at": "2026-06-27T10:00:00Z",
			"updated_at": "2026-06-27T11:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.UpdateUser(context.Background(), userID, cpclient.UpdateUserRequest{Role: &role})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if got.Role != "operator" {
		t.Errorf("Role = %s, want operator", got.Role)
	}
}

func TestDeleteUser(t *testing.T) {
	t.Parallel()
	userID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/users/"+userID {
			t.Errorf("path = %s, want /v1/users/%s", r.URL.Path, userID)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.DeleteUser(context.Background(), userID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
}

func TestGetMe(t *testing.T) {
	t.Parallel()
	userID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/users/me" {
			t.Errorf("path = %s, want /v1/users/me", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         userID,
			"username":   "admin",
			"role":       "admin",
			"created_at": "2026-06-27T10:00:00Z",
			"updated_at": "2026-06-27T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if got.ID != userID {
		t.Errorf("ID = %s, want %s", got.ID, userID)
	}
	if got.Username != "admin" {
		t.Errorf("Username = %s, want admin", got.Username)
	}
}
