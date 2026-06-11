// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/validation"
	coreauth "github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/ratelimit"
	"github.com/otherix/otherix/internal/store"
)

// serviceSpy implements Service and records Login invocations so tests
// can assert the rate limiter short-circuits BEFORE the argon2 path
// (every Login call on the real service pays an argon2id verify).
type serviceSpy struct {
	loginCalls int
	loginErr   error // returned by Login when non-nil; nil means success
}

func (s *serviceSpy) Login(_ context.Context, _ coreauth.Credentials) (*coreauth.TokenPair, error) {
	s.loginCalls++
	if s.loginErr != nil {
		return nil, s.loginErr
	}
	return &coreauth.TokenPair{
		AccessToken:     "access",
		AccessExpiresIn: 900,
		RefreshToken:    "refresh",
	}, nil
}

func (s *serviceSpy) Refresh(_ context.Context, _, _ string, _ netip.Addr) (*coreauth.TokenPair, error) {
	return nil, coreauth.ErrInvalidToken
}

func (s *serviceSpy) Logout(_ context.Context, _ string) error { return nil }

func (s *serviceSpy) LogoutAll(_ context.Context, _ uuid.UUID) error { return nil }

// storeStub satisfies Store with a fixed user row for the post-login
// view lookup.
type storeStub struct{}

func (storeStub) UserByEmail(_ context.Context, email string) (store.User, error) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	return store.User{
		ID:          uuid.New(),
		Email:       email,
		DisplayName: "Test User",
		Role:        "viewer",
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func postLogin(t *testing.T, h *Handler, remoteAddr, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"email":` + strconv.Quote(email) + `,"password":` + strconv.Quote(password) + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	return rec
}

func TestLogin_RateLimited429BeforeArgon2(t *testing.T) {
	spy := &serviceSpy{loginErr: coreauth.ErrInvalidCredentials}
	limiter := ratelimit.New(ratelimit.Config{
		MaxFailures: 2,
		Window:      15 * time.Minute,
		MaxKeys:     100,
	})
	h := New(spy, storeStub{}, limiter)

	// Two failed attempts from the same IP, email case varied: the
	// email key must normalise to lowercase.
	if rec := postLogin(t, h, "192.0.2.10:4444", "Admin@Example.com", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("attempt 1 status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec := postLogin(t, h, "192.0.2.10:4444", "admin@example.COM", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("attempt 2 status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if spy.loginCalls != 2 {
		t.Fatalf("Login calls after 2 attempts = %d, want 2", spy.loginCalls)
	}

	// Third attempt: different source IP, so only the email key can
	// block it. Must be 429 and must NOT reach the service (argon2).
	rec := postLogin(t, h, "192.0.2.99:5555", "ADMIN@example.com", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("attempt 3 status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if spy.loginCalls != 2 {
		t.Errorf("Login calls after blocked attempt = %d, want 2 (argon2 path must not run)", spy.loginCalls)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatalf("Retry-After header missing on 429 response")
	}
	secs, err := strconv.Atoi(retryAfter)
	if err != nil || secs <= 0 {
		t.Errorf("Retry-After = %q, want a positive integer", retryAfter)
	}

	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal 429 body: %v", err)
	}
	if got, want := envelope.Error.Code, "rate_limited"; got != want {
		t.Errorf("error.code = %q, want %q", got, want)
	}
	if _, ok := envelope.Error.Details["retry_after_seconds"]; !ok {
		t.Errorf("error.details.retry_after_seconds missing, got %v", envelope.Error.Details)
	}
}

func TestLogin_PerIPKeyBlocksAcrossEmails(t *testing.T) {
	spy := &serviceSpy{loginErr: coreauth.ErrInvalidCredentials}
	limiter := ratelimit.New(ratelimit.Config{
		MaxFailures: 2,
		Window:      15 * time.Minute,
		MaxKeys:     100,
	})
	h := New(spy, storeStub{}, limiter)

	// Same IP, distinct emails: the ip key alone must block.
	postLogin(t, h, "192.0.2.20:1000", "a@example.com", "wrong")
	postLogin(t, h, "192.0.2.20:1001", "b@example.com", "wrong")

	rec := postLogin(t, h, "192.0.2.20:1002", "c@example.com", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("attempt 3 status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if spy.loginCalls != 2 {
		t.Errorf("Login calls = %d, want 2", spy.loginCalls)
	}
}

func TestLogin_SuccessesAreNeverCounted(t *testing.T) {
	spy := &serviceSpy{} // every Login succeeds
	limiter := ratelimit.New(ratelimit.Config{
		MaxFailures: 2,
		Window:      15 * time.Minute,
		MaxKeys:     100,
	})
	h := New(spy, storeStub{}, limiter)

	for i := 0; i < 5; i++ {
		rec := postLogin(t, h, "192.0.2.30:2000", "ok@example.com", "right")
		if rec.Code != http.StatusOK {
			t.Fatalf("successful attempt %d status = %d, want %d", i+1, rec.Code, http.StatusOK)
		}
	}
	if spy.loginCalls != 5 {
		t.Errorf("Login calls = %d, want 5 (successes must never be throttled)", spy.loginCalls)
	}
}

func TestLogin_NilLimiterNeverThrottles(t *testing.T) {
	spy := &serviceSpy{loginErr: coreauth.ErrInvalidCredentials}
	h := New(spy, storeStub{}, nil)

	for i := 0; i < 20; i++ {
		rec := postLogin(t, h, "192.0.2.40:3000", "user@example.com", "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d with nil limiter status = %d, want %d (never 429)",
				i+1, rec.Code, http.StatusUnauthorized)
		}
	}
	if spy.loginCalls != 20 {
		t.Errorf("Login calls = %d, want 20 (nil limiter must not change behavior)", spy.loginCalls)
	}
}

func TestLogin_OverlongEmailRejectedBeforeServiceAndLimiter(t *testing.T) {
	spy := &serviceSpy{loginErr: coreauth.ErrInvalidCredentials}
	limiter := ratelimit.New(ratelimit.Config{
		MaxFailures: 2,
		Window:      15 * time.Minute,
		MaxKeys:     100,
	})
	h := New(spy, storeStub{}, limiter)

	// An email longer than EmailMaxLength can never match a stored user
	// (every stored email passed validation, <= EmailMaxLength), so it
	// must be rejected at the edge with 400 validation_failed BEFORE the
	// argon2 path runs and BEFORE its bytes are retained as a limiter key.
	longEmail := strings.Repeat("a", validation.EmailMaxLength+1) + "@x.io"
	rec := postLogin(t, h, "192.0.2.50:6000", longEmail, "whatever")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if spy.loginCalls != 0 {
		t.Errorf("Login calls = %d, want 0 (argon2 path must not run for an over-long email)", spy.loginCalls)
	}

	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal 400 body: %v", err)
	}
	if got, want := envelope.Error.Code, "validation_failed"; got != want {
		t.Errorf("error.code = %q, want %q", got, want)
	}

	// The over-long email must NOT have been retained as a limiter key:
	// the limiter is a bounded resource and must not grow from input that
	// never reached the credential path. With MaxFailures=2, even a single
	// recorded failure would not block yet, so repeat the request and
	// confirm the key never accumulates toward a block.
	for i := 0; i < 5; i++ {
		postLogin(t, h, "192.0.2.50:6000", longEmail, "whatever")
	}
	if blocked, _ := limiter.Blocked("email:" + strings.ToLower(longEmail)); blocked {
		t.Errorf("limiter blocked the over-long email key, want no entry ever recorded")
	}
}

func TestLogin_InvalidRemoteAddrStillThrottledByEmail(t *testing.T) {
	spy := &serviceSpy{loginErr: coreauth.ErrInvalidCredentials}
	limiter := ratelimit.New(ratelimit.Config{
		MaxFailures: 2,
		Window:      15 * time.Minute,
		MaxKeys:     100,
	})
	h := New(spy, storeStub{}, limiter)

	// Unparsable RemoteAddr: the ip key is skipped (zero netip.Addr),
	// the email key still carries the weight. No panic, no degradation
	// to a shared "zero IP" bucket.
	postLogin(t, h, "not-an-ip", "x@example.com", "wrong")
	postLogin(t, h, "not-an-ip", "x@example.com", "wrong")

	rec := postLogin(t, h, "not-an-ip", "x@example.com", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("attempt 3 status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if spy.loginCalls != 2 {
		t.Errorf("Login calls = %d, want 2", spy.loginCalls)
	}
}
