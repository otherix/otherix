// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

// Config configures the auth.Service.
type Config struct {
	// JWTSecret is the symmetric HS256 signing key. Must be at least
	// 32 bytes; the api binary validates this at startup.
	JWTSecret []byte
	// JWTAccessTTL is the lifetime of issued access tokens.
	JWTAccessTTL time.Duration
	// RefreshTTL is the lifetime of issued refresh tokens.
	RefreshTTL time.Duration
}

// Validate reports whether c is fit to construct a Service.
func (c Config) Validate() error {
	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("jwt_secret must be at least 32 bytes (got %d)", len(c.JWTSecret))
	}
	if c.JWTAccessTTL <= 0 {
		return errors.New("jwt_access_ttl must be > 0")
	}
	if c.RefreshTTL <= 0 {
		return errors.New("refresh_ttl must be > 0")
	}
	return nil
}

// Service is the in-process auth surface used by HTTP handlers and
// the authn middleware. One instance per binary; safe for concurrent
// use.
type Service struct {
	cfg   Config
	store *store.Store
}

// NewService constructs a Service. Caller is expected to have validated
// cfg via Config.Validate; NewService re-checks defensively.
func NewService(cfg Config, s *store.Store) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("store is nil")
	}
	return &Service{cfg: cfg, store: s}, nil
}

// Credentials is the input to Login. UserAgent and IP are optional; they
// are stored on the issued refresh token for audit purposes only.
type Credentials struct {
	Email     string
	Password  string
	UserAgent string
	IP        netip.Addr
}

// TokenPair is what Login and Refresh return. AccessExpiresIn is the
// access-token lifetime in seconds — that shape mirrors the OpenAPI
// LoginResponse.expires_in field exactly so handlers can pass it
// through without conversion.
type TokenPair struct {
	AccessToken     string
	AccessExpiresIn int
	RefreshToken    string
}

// Login verifies creds against the users table and issues a fresh token
// pair starting a new refresh-token family. Returns ErrInvalidCredentials
// for both "no such user" and "wrong password" — the endpoint must not
// distinguish.
func (s *Service) Login(ctx context.Context, creds Credentials) (*TokenPair, error) {
	user, err := s.store.Queries().GetUserByEmail(ctx, creds.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("lookup user: %v", err)
	}

	ok, verr := VerifyPassword(user.PasswordHash, creds.Password)
	if verr != nil || !ok {
		return nil, ErrInvalidCredentials
	}

	role, err := ParseRole(user.Role)
	if err != nil {
		return nil, ErrInvalidCredentials
	}

	pair, err := s.issueTokenPair(ctx, user.ID, role, uuid.New(), nil, creds.UserAgent, creds.IP)
	if err != nil {
		return nil, err
	}

	// Best-effort: a TouchUserLastLogin failure should not undo a
	// successful login. Log nothing here — Service has no logger by
	// design; the handler will record the request via slog.
	_ = s.store.Queries().TouchUserLastLogin(ctx, user.ID)

	return pair, nil
}

// Refresh rotates the presented refresh token. On success, the presented
// token is revoked and a new pair is issued in the same family chained
// off the parent. On replay (presenting an already-revoked token in the
// family), the entire family is revoked and ErrTokenReplay is returned.
func (s *Service) Refresh(ctx context.Context, plaintext, ua string, ip netip.Addr) (*TokenPair, error) {
	row, err := s.store.Queries().GetRefreshTokenByHash(ctx, HashRefreshToken(plaintext))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("lookup refresh token: %v", err)
	}

	if row.RevokedAt != nil {
		// Detected theft: a revoked refresh was presented. Burn the
		// whole family. Surface the family-revoke error only if the
		// downstream sentinel didn't already carry the meaning.
		if revErr := s.store.Queries().RevokeRefreshTokenFamily(ctx, row.FamilyID); revErr != nil {
			return nil, fmt.Errorf("revoke family on replay: %v", revErr)
		}
		return nil, ErrTokenReplay
	}

	if row.ExpiresAt.Before(time.Now()) {
		return nil, ErrTokenExpired
	}

	user, err := s.store.Queries().GetUserByID(ctx, row.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// User soft-deleted while their refresh token was still
			// outstanding. Revoke this token so a retry doesn't redo
			// the same dance, then report invalid.
			_ = s.store.Queries().RevokeRefreshToken(ctx, row.ID)
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("lookup user: %v", err)
	}

	role, err := ParseRole(user.Role)
	if err != nil {
		return nil, ErrInvalidToken
	}

	var pair *TokenPair
	err = s.store.InTx(ctx, func(q *store.Queries) error {
		if err := q.RevokeRefreshToken(ctx, row.ID); err != nil {
			return fmt.Errorf("revoke parent: %v", err)
		}
		var issueErr error
		pair, issueErr = issueTokenPairTx(ctx, q, s.cfg, user.ID, role,
			row.FamilyID, &row.ID, ua, ip)
		return issueErr
	})
	if err != nil {
		return nil, err
	}

	// Touch outside the tx — non-critical and reduces tx hold time.
	_ = s.store.Queries().TouchRefreshToken(ctx, row.ID)
	return pair, nil
}

// Logout revokes the presented refresh token. Idempotent: an unknown or
// already-revoked token returns nil — the user's intent (this token must
// no longer work) is satisfied either way.
func (s *Service) Logout(ctx context.Context, plaintext string) error {
	row, err := s.store.Queries().GetRefreshTokenByHash(ctx, HashRefreshToken(plaintext))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("lookup refresh token: %v", err)
	}
	return s.store.Queries().RevokeRefreshToken(ctx, row.ID)
}

// LogoutAll revokes every active refresh token for userID.
func (s *Service) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	return s.store.Queries().RevokeAllUserRefreshTokens(ctx, userID)
}

// VerifyAccessToken validates a JWT (signature, issuer, expiry) and
// returns the principal. It does NOT consult the database — access
// tokens are stateless and short-lived; if the user was disabled, the
// access token outlives the disable by at most JWTAccessTTL. Refresh
// rotations and API-token verification close that window for those
// flows.
func (s *Service) VerifyAccessToken(raw string) (*User, error) {
	claims, err := ParseAccessToken(s.cfg.JWTSecret, raw)
	if err != nil {
		return nil, err
	}

	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		return nil, ErrInvalidToken
	}
	jid, err := uuid.Parse(claims.ID)
	if err != nil {
		return nil, ErrInvalidToken
	}
	role, err := ParseRole(claims.Role)
	if err != nil {
		// A JWT carrying a role outside the enum (older revision,
		// forged claim) must not silently land as a principal with
		// no permissions.
		return nil, ErrInvalidToken
	}
	return &User{
		ID:    uid,
		Role:  role,
		Type:  TypeJWT,
		JWTID: &jid,
	}, nil
}

// VerifyAPIToken hashes plaintext, looks it up in api_tokens, and
// returns the principal. Soft-deleted users cause ErrInvalidToken; the
// SQL-level filter on revoked_at and expires_at means only currently
// valid rows are returned. last_used_at is touched best-effort.
func (s *Service) VerifyAPIToken(ctx context.Context, plaintext string) (*User, error) {
	if !IsAPITokenFormat(plaintext) {
		return nil, ErrInvalidToken
	}

	row, err := s.store.Queries().GetApiTokenByHash(ctx, HashAPIToken(plaintext))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("lookup api token: %v", err)
	}

	user, err := s.store.Queries().GetUserByID(ctx, row.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("lookup user: %v", err)
	}

	_ = s.store.Queries().TouchApiToken(ctx, row.ID)

	return &User{
		ID:         user.ID,
		Role:       Role(user.Role),
		Type:       TypeAPIToken,
		APITokenID: &row.ID,
	}, nil
}

// issueTokenPair runs issueTokenPairTx inside a fresh transaction.
func (s *Service) issueTokenPair(
	ctx context.Context,
	userID uuid.UUID, role Role,
	familyID uuid.UUID, parentID *uuid.UUID,
	ua string, ip netip.Addr,
) (*TokenPair, error) {
	var pair *TokenPair
	err := s.store.InTx(ctx, func(q *store.Queries) error {
		var issueErr error
		pair, issueErr = issueTokenPairTx(ctx, q, s.cfg, userID, role, familyID, parentID, ua, ip)
		return issueErr
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// issueTokenPairTx is the transactional core: sign an access token,
// generate a refresh token, persist the refresh row. Lives at package
// level rather than as a method on Service so Refresh can call it
// inside its own transaction without re-entering InTx.
func issueTokenPairTx(
	ctx context.Context,
	q *store.Queries,
	cfg Config,
	userID uuid.UUID, role Role,
	familyID uuid.UUID, parentID *uuid.UUID,
	ua string, ip netip.Addr,
) (*TokenPair, error) {
	access, err := IssueAccessToken(cfg.JWTSecret, userID, role, cfg.JWTAccessTTL)
	if err != nil {
		return nil, fmt.Errorf("issue access token: %v", err)
	}

	plainRefresh, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, err
	}

	uaPtr := nullableString(ua)
	ipPtr := nullableAddr(ip)

	if _, err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID:        uuid.New(),
		UserID:    userID,
		TokenHash: refreshHash,
		FamilyID:  familyID,
		ParentID:  parentID,
		UserAgent: uaPtr,
		IpAddress: ipPtr,
		ExpiresAt: time.Now().Add(cfg.RefreshTTL),
	}); err != nil {
		return nil, fmt.Errorf("persist refresh token: %v", err)
	}

	return &TokenPair{
		AccessToken:     access,
		AccessExpiresIn: int(cfg.JWTAccessTTL.Seconds()),
		RefreshToken:    plainRefresh,
	}, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableAddr(a netip.Addr) *netip.Addr {
	if !a.IsValid() {
		return nil
	}
	return &a
}
