// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// fakeAuthStore is a programmable test double for auth.Store. Each lookup
// is overridable per test via a func field; unset lookups default to the
// store's not-found sentinel and mutators to nil. Every call is recorded in
// calls so a test can assert which methods the service touched (the
// stateless-JWT guard relies on this).
type fakeAuthStore struct {
	calls []string

	userByEmail        func(ctx context.Context, email string) (store.User, error)
	userByID           func(ctx context.Context, id uuid.UUID) (store.User, error)
	refreshTokenByHash func(ctx context.Context, hash []byte) (store.RefreshToken, error)
	apiTokenByHash     func(ctx context.Context, hash []byte) (store.ApiToken, error)
	rotateRefreshToken func(ctx context.Context, parentID uuid.UUID, child store.CreateRefreshTokenParams) (store.RefreshToken, error)

	revokedRefreshTokens []uuid.UUID
	revokedFamilies      []uuid.UUID
}

func (f *fakeAuthStore) UserByEmail(ctx context.Context, email string) (store.User, error) {
	f.calls = append(f.calls, "UserByEmail")
	if f.userByEmail != nil {
		return f.userByEmail(ctx, email)
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeAuthStore) UserByID(ctx context.Context, id uuid.UUID) (store.User, error) {
	f.calls = append(f.calls, "UserByID")
	if f.userByID != nil {
		return f.userByID(ctx, id)
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeAuthStore) TouchUserLastLogin(_ context.Context, _ uuid.UUID) error {
	f.calls = append(f.calls, "TouchUserLastLogin")
	return nil
}

func (f *fakeAuthStore) RefreshTokenByHash(ctx context.Context, hash []byte) (store.RefreshToken, error) {
	f.calls = append(f.calls, "RefreshTokenByHash")
	if f.refreshTokenByHash != nil {
		return f.refreshTokenByHash(ctx, hash)
	}
	return store.RefreshToken{}, store.ErrNotFound
}

func (f *fakeAuthStore) CreateRefreshToken(_ context.Context, _ store.CreateRefreshTokenParams) (store.RefreshToken, error) {
	f.calls = append(f.calls, "CreateRefreshToken")
	return store.RefreshToken{}, nil
}

func (f *fakeAuthStore) RotateRefreshToken(ctx context.Context, parentID uuid.UUID, child store.CreateRefreshTokenParams) (store.RefreshToken, error) {
	f.calls = append(f.calls, "RotateRefreshToken")
	if f.rotateRefreshToken != nil {
		return f.rotateRefreshToken(ctx, parentID, child)
	}
	return store.RefreshToken{}, nil
}

func (f *fakeAuthStore) RevokeRefreshToken(_ context.Context, id uuid.UUID) error {
	f.calls = append(f.calls, "RevokeRefreshToken")
	f.revokedRefreshTokens = append(f.revokedRefreshTokens, id)
	return nil
}

func (f *fakeAuthStore) RevokeRefreshTokenFamily(_ context.Context, familyID uuid.UUID) error {
	f.calls = append(f.calls, "RevokeRefreshTokenFamily")
	f.revokedFamilies = append(f.revokedFamilies, familyID)
	return nil
}

func (f *fakeAuthStore) RevokeAllUserRefreshTokens(_ context.Context, _ uuid.UUID) error {
	f.calls = append(f.calls, "RevokeAllUserRefreshTokens")
	return nil
}

func (f *fakeAuthStore) TouchRefreshToken(_ context.Context, _ uuid.UUID) error {
	f.calls = append(f.calls, "TouchRefreshToken")
	return nil
}

func (f *fakeAuthStore) APITokenByHash(ctx context.Context, hash []byte) (store.ApiToken, error) {
	f.calls = append(f.calls, "APITokenByHash")
	if f.apiTokenByHash != nil {
		return f.apiTokenByHash(ctx, hash)
	}
	return store.ApiToken{}, store.ErrNotFound
}

func (f *fakeAuthStore) TouchAPIToken(_ context.Context, _ uuid.UUID) error {
	f.calls = append(f.calls, "TouchAPIToken")
	return nil
}

func newTestService(t *testing.T, s auth.Store) *auth.Service {
	t.Helper()
	svc, err := auth.NewService(auth.Config{
		JWTSecret:    testSecret(),
		JWTAccessTTL: time.Hour,
		RefreshTTL:   24 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestVerifyAccessTokenRejectsUnknownRole guards against a JWT minted by an
// older revision (or forged) whose role claim is outside the enum landing as
// a principal. ParseRole must reject it with ErrInvalidToken rather than
// admitting a roleless principal.
func TestVerifyAccessTokenRejectsUnknownRole(t *testing.T) {
	svc := newTestService(t, &fakeAuthStore{})

	tok, err := auth.IssueAccessToken(testSecret(), uuid.New(), auth.Role("superadmin"), time.Hour)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	if _, err := svc.VerifyAccessToken(tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("VerifyAccessToken(drifted role) error = %v, want ErrInvalidToken", err)
	}
}

// TestVerifyAccessTokenIsStateless guards the documented contract that access
// tokens are verified without a database round trip. A regression that adds a
// store lookup (e.g. re-checking the user) would be caught here.
func TestVerifyAccessTokenIsStateless(t *testing.T) {
	fake := &fakeAuthStore{}
	svc := newTestService(t, fake)
	uid := uuid.New()

	tok, err := auth.IssueAccessToken(testSecret(), uid, auth.RoleOperator, time.Hour)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	principal, err := svc.VerifyAccessToken(tok)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if principal.ID != uid {
		t.Errorf("principal.ID = %v, want %v", principal.ID, uid)
	}
	if principal.Role != auth.RoleOperator {
		t.Errorf("principal.Role = %v, want %v", principal.Role, auth.RoleOperator)
	}
	if len(fake.calls) != 0 {
		t.Errorf("VerifyAccessToken consulted the store: %v, want no calls", fake.calls)
	}
}

// TestVerifyAPITokenRejectsInvalidRows covers the guard paths: a revoked
// or expired token (the store's valid-only filter returns not-found), a
// live token whose owner was soft-deleted, and a live token whose owner
// carries a role outside the enum. All must surface ErrInvalidToken.
func TestVerifyAPITokenRejectsInvalidRows(t *testing.T) {
	t.Run("revoked or expired token", func(t *testing.T) {
		fake := &fakeAuthStore{
			apiTokenByHash: func(_ context.Context, _ []byte) (store.ApiToken, error) {
				return store.ApiToken{}, store.ErrNotFound
			},
		}
		svc := newTestService(t, fake)

		if _, err := svc.VerifyAPIToken(context.Background(), "otx_revoked"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("VerifyAPIToken(revoked) error = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("soft-deleted owner", func(t *testing.T) {
		fake := &fakeAuthStore{
			apiTokenByHash: func(_ context.Context, _ []byte) (store.ApiToken, error) {
				return store.ApiToken{ID: uuid.New(), UserID: uuid.New()}, nil
			},
			userByID: func(_ context.Context, _ uuid.UUID) (store.User, error) {
				return store.User{}, store.ErrNotFound
			},
		}
		svc := newTestService(t, fake)

		if _, err := svc.VerifyAPIToken(context.Background(), "otx_orphaned"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("VerifyAPIToken(soft-deleted owner) error = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("owner with out-of-enum role", func(t *testing.T) {
		// Mirrors the VerifyAccessToken guard: a stored role outside
		// the enum (schema drift, manual edit) must not land as a
		// principal with no permissions.
		for _, role := range []string{"superuser", ""} {
			fake := &fakeAuthStore{
				apiTokenByHash: func(_ context.Context, _ []byte) (store.ApiToken, error) {
					return store.ApiToken{ID: uuid.New(), UserID: uuid.New()}, nil
				},
				userByID: func(_ context.Context, _ uuid.UUID) (store.User, error) {
					return store.User{ID: uuid.New(), Role: role}, nil
				},
			}
			svc := newTestService(t, fake)

			if _, err := svc.VerifyAPIToken(context.Background(), "otx_badrole"); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("VerifyAPIToken(role %q) error = %v, want ErrInvalidToken", role, err)
			}
		}
	})

	t.Run("malformed prefix is rejected without a store lookup", func(t *testing.T) {
		fake := &fakeAuthStore{}
		svc := newTestService(t, fake)

		if _, err := svc.VerifyAPIToken(context.Background(), "not-an-otx-token"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("VerifyAPIToken(malformed) error = %v, want ErrInvalidToken", err)
		}
		if len(fake.calls) != 0 {
			t.Errorf("VerifyAPIToken(malformed) consulted the store: %v, want no calls", fake.calls)
		}
	})
}

// TestRefreshRejectsSoftDeletedUser guards the case where a refresh token is
// still valid but its owner was soft-deleted mid-window. The service must
// report ErrInvalidToken and revoke the orphaned token so a retry does not
// repeat the lookup.
func TestRefreshRejectsSoftDeletedUser(t *testing.T) {
	row := store.RefreshToken{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		FamilyID:  uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	fake := &fakeAuthStore{
		refreshTokenByHash: func(_ context.Context, _ []byte) (store.RefreshToken, error) {
			return row, nil
		},
		userByID: func(_ context.Context, _ uuid.UUID) (store.User, error) {
			return store.User{}, store.ErrNotFound
		},
	}
	svc := newTestService(t, fake)

	if _, err := svc.Refresh(context.Background(), "otx_refresh_plaintext", "", netip.Addr{}); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Refresh(soft-deleted user) error = %v, want ErrInvalidToken", err)
	}
	if len(fake.revokedRefreshTokens) != 1 || fake.revokedRefreshTokens[0] != row.ID {
		t.Errorf("revoked tokens = %v, want [%v]", fake.revokedRefreshTokens, row.ID)
	}
}

// TestRefreshConcurrentRotationIsTheft guards the concurrent double-spend
// path (audit M8): when the store reports store.ErrRefreshTokenConflict from
// RotateRefreshToken (a concurrent rotation already spent the parent), the
// service must treat the presentation as theft - burn the whole family and
// return ErrTokenReplay, exactly like a revoked-token presentation.
func TestRefreshConcurrentRotationIsTheft(t *testing.T) {
	row := store.RefreshToken{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		FamilyID:  uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	fake := &fakeAuthStore{
		refreshTokenByHash: func(_ context.Context, _ []byte) (store.RefreshToken, error) {
			return row, nil
		},
		userByID: func(_ context.Context, id uuid.UUID) (store.User, error) {
			return store.User{ID: id, Role: "viewer"}, nil
		},
		rotateRefreshToken: func(_ context.Context, _ uuid.UUID, _ store.CreateRefreshTokenParams) (store.RefreshToken, error) {
			return store.RefreshToken{}, store.ErrRefreshTokenConflict
		},
	}
	svc := newTestService(t, fake)

	if _, err := svc.Refresh(context.Background(), "refresh-plaintext", "", netip.Addr{}); !errors.Is(err, auth.ErrTokenReplay) {
		t.Errorf("Refresh(concurrent rotation) error = %v, want ErrTokenReplay", err)
	}
	if len(fake.revokedFamilies) != 1 || fake.revokedFamilies[0] != row.FamilyID {
		t.Errorf("revoked families = %v, want [%v]", fake.revokedFamilies, row.FamilyID)
	}
}

// TestLoginUnknownEmailReturnsInvalidCredentials pins the user-not-found
// branch of Login to the same sentinel as a wrong password, so the endpoint
// cannot distinguish the two cases. The timing half of that guarantee (the
// dummy-hash KDF cost, audit M6) is covered structurally by
// TestDummyLoginHashIsRealArgon2id.
func TestLoginUnknownEmailReturnsInvalidCredentials(t *testing.T) {
	fake := &fakeAuthStore{} // UserByEmail defaults to store.ErrNotFound.
	svc := newTestService(t, fake)

	_, err := svc.Login(context.Background(), auth.Credentials{
		Email:    "nobody@example.test",
		Password: "irrelevant-password",
	})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login(unknown email) error = %v, want ErrInvalidCredentials", err)
	}
	// The not-found path must never mint or persist tokens.
	for _, call := range fake.calls {
		if call == "CreateRefreshToken" {
			t.Errorf("Login(unknown email) persisted a refresh token; calls = %v", fake.calls)
		}
	}
}
