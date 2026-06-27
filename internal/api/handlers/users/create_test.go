// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/users"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// createFakeStore is a programmable double for users.Store focused on the
// create path. It records the params it was asked to persist and serves a
// username/email guard backed by the rows it has already created.
type createFakeStore struct {
	byUsername map[string]store.User
	byEmail    map[string]store.User

	createErr error
}

func newCreateFakeStore() *createFakeStore {
	return &createFakeStore{
		byUsername: map[string]store.User{},
		byEmail:    map[string]store.User{},
	}
}

func (f *createFakeStore) UserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	for _, u := range f.byUsername {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (f *createFakeStore) UserByEmail(_ context.Context, email string) (store.User, error) {
	if u, ok := f.byEmail[email]; ok {
		return u, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *createFakeStore) UserByUsername(_ context.Context, username string) (store.User, error) {
	if u, ok := f.byUsername[username]; ok {
		return u, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *createFakeStore) CreateUser(_ context.Context, arg store.CreateUserParams) (store.User, error) {
	if f.createErr != nil {
		return store.User{}, f.createErr
	}
	if _, ok := f.byUsername[arg.Username]; ok {
		return store.User{}, store.ErrUserUsernameExists
	}
	if arg.Email != "" {
		if _, ok := f.byEmail[arg.Email]; ok {
			return store.User{}, store.ErrUserEmailExists
		}
	}
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
	f.byUsername[u.Username] = u
	if u.Email != "" {
		f.byEmail[u.Email] = u
	}
	return u, nil
}

func (f *createFakeStore) UpdateUser(_ context.Context, _ store.UpdateUserParams) (store.User, error) {
	return store.User{}, nil
}

func (f *createFakeStore) ListUsers(_ context.Context, _ store.ListUsersParams) ([]store.User, error) {
	return nil, nil
}

func (f *createFakeStore) CountUserResources(_ context.Context, _ uuid.UUID) (store.CountUserResourcesRow, error) {
	return store.CountUserResourcesRow{}, nil
}

func (f *createFakeStore) DeleteUser(_ context.Context, _ uuid.UUID) error { return nil }

func (f *createFakeStore) RevokeAllUserRefreshTokens(_ context.Context, _ uuid.UUID) error {
	return nil
}

// newCreateHarness wires the users handler into a chi router behind a stub
// authn layer that injects an admin caller, mirroring POST /v1/users.
func newCreateHarness(fake users.Store) http.Handler {
	h := users.New(fake)
	admin := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUser(req.Context(), admin)))
		})
	})
	r.Post("/v1/users", h.Create)
	return r
}

func postJSON(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCreateUserRequiresValidUsername(t *testing.T) {
	// Missing username -> 400 validation_failed.
	router := newCreateHarness(newCreateFakeStore())
	rec := postJSON(t, router, "/v1/users",
		`{"password":"a-valid-password-123","role":"developer"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing username status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "validation_failed" {
		t.Errorf("missing username error code = %q, want validation_failed", code)
	}

	// Invalid username (uppercase) -> 400 validation_failed.
	rec = postJSON(t, router, "/v1/users",
		`{"username":"Bad_Name","password":"a-valid-password-123","role":"developer"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid username status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "validation_failed" {
		t.Errorf("invalid username error code = %q, want validation_failed", code)
	}
}

func TestCreateUserValidUsernameNoEmail(t *testing.T) {
	router := newCreateHarness(newCreateFakeStore())
	rec := postJSON(t, router, "/v1/users",
		`{"username":"alice","password":"a-valid-password-123","role":"developer"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid username status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := body["username"]; got != "alice" {
		t.Errorf("response username = %v, want alice", got)
	}
	if _, present := body["email"]; present {
		t.Errorf("response email should be omitted when empty, got %v", body["email"])
	}
}

func TestCreateUserUsernameConflict409(t *testing.T) {
	router := newCreateHarness(newCreateFakeStore())
	body := `{"username":"alice","password":"a-valid-password-123","role":"developer"}`

	if rec := postJSON(t, router, "/v1/users", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	rec := postJSON(t, router, "/v1/users", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate username status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "conflict" {
		t.Errorf("duplicate username error code = %q, want conflict", code)
	}
}

// errorCode pulls error.code out of the standard error envelope.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	return env.Error.Code
}
