// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// sharedHarness is a process-global testcontainers Postgres set up by
// TestMain. Kept package-level (mirroring internal/store conventions)
// so each Test* function uses a single container.
var sharedHarness *migrationtest.Harness

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := migrationtest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrationtest.Start: %v\n", err)
		os.Exit(1)
	}
	sharedHarness = h

	code := m.Run()

	stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	h.Stop(stopCtx)

	os.Exit(code)
}

func newService(t *testing.T) (*auth.Service, *store.Store) {
	t.Helper()
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN:      sharedHarness.DSN,
		MaxConns: 4,
		MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(s.Close)

	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	return svc, s
}

// createUser inserts a user with a real argon2id-hashed password and
// returns id, email, plaintext password.
func createUser(t *testing.T, ctx context.Context, s *store.Store) (uuid.UUID, string, string) {
	t.Helper()
	const pw = "correct-horse-battery-staple"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	id := uuid.New()
	email := fmt.Sprintf("auth-%s@example.test", uuid.NewString())
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: hash,
		Role:         "developer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id, email, pw
}

func TestLoginRefreshHappyPath(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, email, pw := createUser(t, ctx, s)

	pair, err := svc.Login(ctx, auth.Credentials{
		Email: email, Password: pw,
		UserAgent: "test/1.0", IP: netip.MustParseAddr("127.0.0.1"),
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("Login returned empty tokens")
	}
	if pair.AccessExpiresIn != 900 {
		t.Errorf("AccessExpiresIn = %d, want 900 (15m)", pair.AccessExpiresIn)
	}

	// Access token verifies and points back at the user.
	user, err := svc.VerifyAccessToken(pair.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if user.ID != uid {
		t.Errorf("verified user.ID = %v, want %v", user.ID, uid)
	}
	if user.Type != auth.TypeJWT {
		t.Errorf("user.Type = %q, want %q", user.Type, auth.TypeJWT)
	}

	// Refresh rotates: same user, new tokens, parent revoked.
	newPair, err := svc.Refresh(ctx, pair.RefreshToken, "test/1.0", netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newPair.RefreshToken == pair.RefreshToken {
		t.Errorf("Refresh returned the same refresh token; rotation broken")
	}
	if newPair.AccessToken == pair.AccessToken {
		t.Errorf("Refresh returned the same access token")
	}

	// Old access still valid until JWT expiry — that is expected;
	// access tokens are stateless. (The refresh side is what we
	// guard against replay.)
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	_, email, _ := createUser(t, ctx, s)

	_, err := svc.Login(ctx, auth.Credentials{Email: email, Password: "wrong"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login(wrong pw) err = %v, want ErrInvalidCredentials", err)
	}

	_, err = svc.Login(ctx, auth.Credentials{Email: "missing@example.test", Password: "anything"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login(unknown email) err = %v, want ErrInvalidCredentials", err)
	}
}

func TestRefreshDetectsReplayAndRevokesFamily(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	_, email, pw := createUser(t, ctx, s)

	pair, err := svc.Login(ctx, auth.Credentials{Email: email, Password: pw})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Rotate once: pair → next.
	next, err := svc.Refresh(ctx, pair.RefreshToken, "", netip.Addr{})
	if err != nil {
		t.Fatalf("Refresh #1: %v", err)
	}

	// Replay the original (revoked) refresh — should detect theft and
	// revoke the family, not just the parent.
	_, err = svc.Refresh(ctx, pair.RefreshToken, "", netip.Addr{})
	if !errors.Is(err, auth.ErrTokenReplay) {
		t.Errorf("replay Refresh err = %v, want ErrTokenReplay", err)
	}

	// The current valid token should now also be revoked.
	_, err = svc.Refresh(ctx, next.RefreshToken, "", netip.Addr{})
	if !errors.Is(err, auth.ErrTokenReplay) && !errors.Is(err, auth.ErrInvalidToken) {
		// ErrTokenReplay if its row was already in the family at revoke
		// time (the expected outcome); ErrInvalidToken would be the
		// fallback if it weren't. Either is acceptable as proof that
		// the family was burned.
		t.Errorf("post-replay Refresh err = %v, want family-revoked", err)
	}
}

func TestRefreshRejectsExpired(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, _, _ := createUser(t, ctx, s)

	// Persist a refresh token whose expires_at is already past, then
	// try to use it. Goes through the same code path as natural expiry.
	plaintext, hash, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: uid, TokenHash: hash, FamilyID: uuid.New(),
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	if _, err := svc.Refresh(ctx, plaintext, "", netip.Addr{}); !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("Refresh(expired) err = %v, want ErrTokenExpired", err)
	}
}

func TestRefreshRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	if _, err := svc.Refresh(ctx, "no-such-token", "", netip.Addr{}); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Refresh(unknown) err = %v, want ErrInvalidToken", err)
	}
}

func TestRefreshRejectsSoftDeletedUser(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, email, pw := createUser(t, ctx, s)

	pair, err := svc.Login(ctx, auth.Credentials{Email: email, Password: pw})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := s.Queries().SoftDeleteUser(ctx, uid); err != nil {
		t.Fatalf("SoftDeleteUser: %v", err)
	}

	if _, err := svc.Refresh(ctx, pair.RefreshToken, "", netip.Addr{}); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Refresh(soft-deleted user) err = %v, want ErrInvalidToken", err)
	}
}

func TestLogoutIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	_, email, pw := createUser(t, ctx, s)

	pair, err := svc.Login(ctx, auth.Credentials{Email: email, Password: pw})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.Logout(ctx, pair.RefreshToken); err != nil {
		t.Errorf("Logout: %v", err)
	}
	if err := svc.Logout(ctx, pair.RefreshToken); err != nil {
		t.Errorf("re-Logout: %v", err)
	}
	if err := svc.Logout(ctx, "totally-unknown-token"); err != nil {
		t.Errorf("Logout(unknown): %v", err)
	}

	// After logout, the token cannot be used to refresh.
	if _, err := svc.Refresh(ctx, pair.RefreshToken, "", netip.Addr{}); !errors.Is(err, auth.ErrTokenReplay) {
		t.Errorf("Refresh(post-logout) err = %v, want ErrTokenReplay (revoked = replay signal)", err)
	}

	_ = s
}

func TestLogoutAll(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, email, pw := createUser(t, ctx, s)

	// Three concurrent sessions.
	pairs := make([]*auth.TokenPair, 3)
	for i := range pairs {
		p, err := svc.Login(ctx, auth.Credentials{Email: email, Password: pw})
		if err != nil {
			t.Fatalf("Login %d: %v", i, err)
		}
		pairs[i] = p
	}

	if err := svc.LogoutAll(ctx, uid); err != nil {
		t.Fatalf("LogoutAll: %v", err)
	}

	for i, p := range pairs {
		if _, err := svc.Refresh(ctx, p.RefreshToken, "", netip.Addr{}); !errors.Is(err, auth.ErrTokenReplay) {
			t.Errorf("Refresh(pair[%d]) err = %v, want ErrTokenReplay after LogoutAll", i, err)
		}
	}

	_ = s
}

func TestVerifyAPITokenHappyPath(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, _, _ := createUser(t, ctx, s)

	plaintext, hash, prefix, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID: uuid.New(), UserID: uid, Name: "ci",
		TokenHash: hash, Prefix: prefix,
	}); err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}

	user, err := svc.VerifyAPIToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("VerifyAPIToken: %v", err)
	}
	if user.ID != uid {
		t.Errorf("user.ID = %v, want %v", user.ID, uid)
	}
	if user.Type != auth.TypeAPIToken {
		t.Errorf("user.Type = %q, want %q", user.Type, auth.TypeAPIToken)
	}
}

func TestVerifyAPITokenRejectsRevokedAndUnknown(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, _, _ := createUser(t, ctx, s)

	plaintext, hash, prefix, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	tokenID := uuid.New()
	if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID: tokenID, UserID: uid, Name: "rev",
		TokenHash: hash, Prefix: prefix,
	}); err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}
	if err := s.Queries().RevokeApiToken(ctx, tokenID); err != nil {
		t.Fatalf("RevokeApiToken: %v", err)
	}

	if _, err := svc.VerifyAPIToken(ctx, plaintext); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("VerifyAPIToken(revoked) err = %v, want ErrInvalidToken", err)
	}

	if _, err := svc.VerifyAPIToken(ctx, "otx_unknown"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("VerifyAPIToken(unknown) err = %v, want ErrInvalidToken", err)
	}

	if _, err := svc.VerifyAPIToken(ctx, "not-an-otx-token"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("VerifyAPIToken(non-otx) err = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyAccessTokenIgnoresDB(t *testing.T) {
	ctx := context.Background()
	svc, s := newService(t)
	uid, email, pw := createUser(t, ctx, s)

	pair, err := svc.Login(ctx, auth.Credentials{Email: email, Password: pw})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Soft-delete the user. The JWT remains structurally valid until
	// expiry — that's the documented trade-off, asserted here so a
	// future change that adds DB checks to VerifyAccessToken doesn't
	// silently flip it.
	if err := s.Queries().SoftDeleteUser(ctx, uid); err != nil {
		t.Fatalf("SoftDeleteUser: %v", err)
	}
	if _, err := svc.VerifyAccessToken(pair.AccessToken); err != nil {
		t.Errorf("VerifyAccessToken after soft-delete err = %v, want nil (stateless)", err)
	}
}

func TestVerifyAccessTokenRejectsUnknownRole(t *testing.T) {
	// A JWT carrying a role outside the enum (older revision, forged
	// claim, schema rename) must be rejected at verification, not
	// silently produce a User with no permissions.
	svc, _ := newService(t)
	tok, err := auth.IssueAccessToken(
		[]byte("test-secret-32-bytes-padding-pad-"),
		uuid.New(),
		auth.Role("ghost"),
		15*time.Minute,
	)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	_, err = svc.VerifyAccessToken(tok)
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("VerifyAccessToken(unknown role) err = %v, want ErrInvalidToken", err)
	}
}
