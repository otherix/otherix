// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users_test

import (
	"context"
	"errors"
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

// fakeUsersStore is a programmable test double for users.Store. It serves a
// single user row and records every RevokeAllUserRefreshTokens call so tests
// can assert whether a password change revoked the user's sessions (audit M7).
type fakeUsersStore struct {
	user store.User

	revokeErr   error
	revokeCalls []uuid.UUID
}

func (f *fakeUsersStore) UserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	if id == f.user.ID {
		return f.user, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeUsersStore) UserByEmail(_ context.Context, email string) (store.User, error) {
	if email == f.user.Email {
		return f.user, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeUsersStore) CreateUser(_ context.Context, _ store.CreateUserParams) (store.User, error) {
	return store.User{}, nil
}

func (f *fakeUsersStore) UpdateUser(_ context.Context, arg store.UpdateUserParams) (store.User, error) {
	f.user.Email = arg.Email
	f.user.PasswordHash = arg.PasswordHash
	f.user.DisplayName = arg.DisplayName
	f.user.Role = arg.Role
	return f.user, nil
}

func (f *fakeUsersStore) ListUsers(_ context.Context, _ store.ListUsersParams) ([]store.User, error) {
	return nil, nil
}

func (f *fakeUsersStore) CountUserResources(_ context.Context, _ uuid.UUID) (store.CountUserResourcesRow, error) {
	return store.CountUserResourcesRow{}, nil
}

func (f *fakeUsersStore) DeleteUser(_ context.Context, _ uuid.UUID) error { return nil }

func (f *fakeUsersStore) RevokeAllUserRefreshTokens(_ context.Context, userID uuid.UUID) error {
	f.revokeCalls = append(f.revokeCalls, userID)
	return f.revokeErr
}

// newUpdateHarness wires the users handler into a chi router behind a stub
// authn layer that injects caller as the request principal, mirroring the
// production route shapes for PATCH /v1/users/me and /v1/users/{id}.
func newUpdateHarness(fake *fakeUsersStore, caller *auth.User) http.Handler {
	h := users.New(fake)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUser(req.Context(), caller)))
		})
	})
	r.Patch("/v1/users/me", h.UpdateMe)
	r.Patch("/v1/users/{id}", h.Update)
	return r
}

func seedFakeUser(role string) store.User {
	return store.User{
		ID:           uuid.New(),
		Email:        "victim@example.test",
		PasswordHash: "$argon2id$v=19$m=8,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		DisplayName:  "Victim",
		Role:         role,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
}

func patchJSON(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestUpdateMePasswordChangeRevokesSessions(t *testing.T) {
	fake := &fakeUsersStore{user: seedFakeUser("developer")}
	caller := &auth.User{ID: fake.user.ID, Role: auth.RoleDeveloper, Type: auth.TypeJWT}
	router := newUpdateHarness(fake, caller)

	rec := patchJSON(t, router, "/v1/users/me", `{"password":"brand-new-password-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/users/me status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fake.revokeCalls) != 1 || fake.revokeCalls[0] != fake.user.ID {
		t.Errorf("revoke calls = %v, want exactly [%v]", fake.revokeCalls, fake.user.ID)
	}
}

func TestUpdateMeWithoutPasswordDoesNotRevokeSessions(t *testing.T) {
	fake := &fakeUsersStore{user: seedFakeUser("developer")}
	caller := &auth.User{ID: fake.user.ID, Role: auth.RoleDeveloper, Type: auth.TypeJWT}
	router := newUpdateHarness(fake, caller)

	rec := patchJSON(t, router, "/v1/users/me", `{"display_name":"New Name"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/users/me status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fake.revokeCalls) != 0 {
		t.Errorf("revoke calls = %v, want none for a non-password patch", fake.revokeCalls)
	}
}

func TestUpdatePasswordChangeRevokesTargetSessions(t *testing.T) {
	fake := &fakeUsersStore{user: seedFakeUser("developer")}
	admin := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT}
	router := newUpdateHarness(fake, admin)

	rec := patchJSON(t, router, "/v1/users/"+fake.user.ID.String(), `{"password":"brand-new-password-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/users/{id} status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fake.revokeCalls) != 1 || fake.revokeCalls[0] != fake.user.ID {
		t.Errorf("revoke calls = %v, want exactly [%v]", fake.revokeCalls, fake.user.ID)
	}
}

func TestUpdateWithoutPasswordDoesNotRevokeSessions(t *testing.T) {
	fake := &fakeUsersStore{user: seedFakeUser("developer")}
	admin := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT}
	router := newUpdateHarness(fake, admin)

	rec := patchJSON(t, router, "/v1/users/"+fake.user.ID.String(), `{"display_name":"New Name"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/users/{id} status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fake.revokeCalls) != 0 {
		t.Errorf("revoke calls = %v, want none for a non-password patch", fake.revokeCalls)
	}
}

func TestUpdateMeRevokeFailureSurfacesAs500(t *testing.T) {
	fake := &fakeUsersStore{user: seedFakeUser("developer"), revokeErr: errors.New("revoke failed")}
	caller := &auth.User{ID: fake.user.ID, Role: auth.RoleDeveloper, Type: auth.TypeJWT}
	router := newUpdateHarness(fake, caller)

	rec := patchJSON(t, router, "/v1/users/me", `{"password":"brand-new-password-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PATCH /v1/users/me with failing revoke status = %d, want 500", rec.Code)
	}
}
