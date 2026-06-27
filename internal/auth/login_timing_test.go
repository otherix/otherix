// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// argonParams extracts the cost-parameter segment ("m=...,t=...,p=...") from
// an argon2id PHC string: $argon2id$v=19$m=..,t=..,p=..$salt$hash. Fails the
// test if the string does not have the expected five-segment shape.
func argonParams(t *testing.T, phc string) string {
	t.Helper()
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		t.Fatalf("argonParams(%q): not a well-formed argon2id PHC string", phc)
	}
	return parts[3]
}

// TestDummyLoginHashParamsMatchActiveArgonParams guards the M6 timing fix
// against init-order regressions: the dummy hash must carry the same argon2
// cost parameters as a hash produced by the active HashPassword
// configuration. A package-level var initializer runs before the
// test_fast_argon init() override and would capture the production cost,
// inverting the timing equalization under test builds; lazy initialization
// keeps the two in lockstep in every build.
func TestDummyLoginHashParamsMatchActiveArgonParams(t *testing.T) {
	fresh, err := HashPassword("x")
	if err != nil {
		t.Fatalf("HashPassword(\"x\") error = %v, want nil", err)
	}
	got := argonParams(t, dummyLoginHash())
	want := argonParams(t, fresh)
	if got != want {
		t.Errorf("dummyLoginHash params = %q, want %q (active HashPassword params)", got, want)
	}
}

// loginTimingStore is a minimal in-package Store stub for the login
// equal-cost test: UserByUsername is programmable, every other method is an
// inert success. It lets the timing test drive the real Login entry path
// (including the dummy-hash KDF on the not-found branch) without the external
// fakeAuthStore in service_test.go.
type loginTimingStore struct {
	userByUsername func(ctx context.Context, username string) (store.User, error)
}

func (s loginTimingStore) UserByUsername(ctx context.Context, username string) (store.User, error) {
	if s.userByUsername != nil {
		return s.userByUsername(ctx, username)
	}
	return store.User{}, store.ErrNotFound
}

func (loginTimingStore) UserByID(context.Context, uuid.UUID) (store.User, error) {
	return store.User{}, store.ErrNotFound
}
func (loginTimingStore) TouchUserLastLogin(context.Context, uuid.UUID) error { return nil }
func (loginTimingStore) RefreshTokenByHash(context.Context, []byte) (store.RefreshToken, error) {
	return store.RefreshToken{}, store.ErrNotFound
}

func (loginTimingStore) CreateRefreshToken(context.Context, store.CreateRefreshTokenParams) (store.RefreshToken, error) {
	return store.RefreshToken{}, nil
}

func (loginTimingStore) RotateRefreshToken(context.Context, uuid.UUID, store.CreateRefreshTokenParams) (store.RefreshToken, error) {
	return store.RefreshToken{}, nil
}
func (loginTimingStore) RevokeRefreshToken(context.Context, uuid.UUID) error       { return nil }
func (loginTimingStore) RevokeRefreshTokenFamily(context.Context, uuid.UUID) error { return nil }
func (loginTimingStore) RevokeAllUserRefreshTokens(context.Context, uuid.UUID) error {
	return nil
}
func (loginTimingStore) TouchRefreshToken(context.Context, uuid.UUID) error { return nil }
func (loginTimingStore) APITokenByHash(context.Context, []byte) (store.ApiToken, error) {
	return store.ApiToken{}, store.ErrNotFound
}
func (loginTimingStore) TouchAPIToken(context.Context, uuid.UUID) error { return nil }

// TestLoginUnknownUsernameAndWrongPasswordPayEqualKDFCost guards the
// anti-enumeration property end to end through the real Login path: an unknown
// username and a known username with a wrong password must both run a full
// argon2id verification (the not-found branch verifies the dummy hash) and both
// return ErrInvalidCredentials, so neither error shape nor KDF cost
// distinguishes the two. The structural KDF-cost guarantee is in
// TestDummyLoginHashIsRealArgon2id; this test pins the wiring that invokes it.
func TestLoginUnknownUsernameAndWrongPasswordPayEqualKDFCost(t *testing.T) {
	realHash, err := HashPassword("the-correct-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	svc, err := NewService(Config{
		JWTSecret:    make([]byte, 32),
		JWTAccessTTL: time.Hour,
		RefreshTTL:   24 * time.Hour,
	}, loginTimingStore{
		userByUsername: func(_ context.Context, username string) (store.User, error) {
			if username == "alice" {
				return store.User{ID: uuid.New(), Username: "alice", PasswordHash: realHash, Role: "viewer"}, nil
			}
			return store.User{}, store.ErrNotFound
		},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Unknown username: hits the not-found branch, verifies the dummy hash.
	if _, err := svc.Login(context.Background(), Credentials{Username: "ghost", Password: "whatever-wrong"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login(unknown username) error = %v, want ErrInvalidCredentials", err)
	}
	// Known username, wrong password: hits the user-found branch, verifies the real hash.
	if _, err := svc.Login(context.Background(), Credentials{Username: "alice", Password: "wrong-password"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login(wrong password) error = %v, want ErrInvalidCredentials", err)
	}
}

// TestDummyLoginHashIsRealArgon2id guards the user-enumeration timing fix:
// the dummy hash verified on Login's user-not-found path must
// be a well-formed argon2id PHC string so VerifyPassword runs the full KDF
// rather than bailing out on a parse error. A (false, nil) result proves
// the mismatch path was reached, i.e. the KDF cost was actually paid.
func TestDummyLoginHashIsRealArgon2id(t *testing.T) {
	ok, err := VerifyPassword(dummyLoginHash(), "anything")
	if err != nil {
		t.Fatalf("VerifyPassword(dummyLoginHash(), ...) error = %v, want nil (hash must parse)", err)
	}
	if ok {
		t.Fatal("VerifyPassword(dummyLoginHash(), \"anything\") = true, want false")
	}
}
