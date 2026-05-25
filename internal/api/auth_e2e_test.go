// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// e2eHarness wires a real Postgres + Store + auth.Service + chi router
// into an httptest.Server so each test can talk to the API the way a
// real client does (full middleware stack, real JSON encoding, real
// error envelopes).
type e2eHarness struct {
	srv   *httptest.Server
	store *store.Store
	svc   *auth.Service
}

func (h *e2eHarness) close() {
	h.srv.Close()
	h.store.Close()
}

func (h *e2eHarness) post(t *testing.T, path string, body any, bearer string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// postWithHeaders is post with extra request headers — used by tests
// that exercise Idempotency-Key replay or other header-driven middleware.
func (h *e2eHarness) postWithHeaders(t *testing.T, path string, body any, bearer string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func (h *e2eHarness) get(t *testing.T, path, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func (h *e2eHarness) delete(t *testing.T, path, bearer string) *http.Response {
	t.Helper()
	return h.deleteWithIdempotency(t, path, bearer, "")
}

func (h *e2eHarness) deleteWithIdempotency(t *testing.T, path, bearer, idempotencyKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

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

func newE2E(t *testing.T) *e2eHarness {
	return newE2EWithImageDeleter(t, nil)
}

// newE2EWithImageDeleter is the customisation point for tests that need
// to inject an ImageDeleter for storage_image.delete. The production
// wiring constructs *agentclient.Client; e2e tests pass a fake or nil
// to exercise the count > 0 / agent_unreachable branches.
func newE2EWithImageDeleter(t *testing.T, deleter storagepoolshandlers.ImageDeleter) *e2eHarness {
	t.Helper()
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN: sharedHarness.DSN, MaxConns: 4, MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}

	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	// Build a river client without Start: tasks.cancel needs JobCancelTx
	// (transactional update on river_job) but the e2e suite does not
	// drive workers — leaving the queue dormant keeps test runs
	// deterministic. Tests that need worker dispatch own their own
	// client lifecycle (see tests/integration/storage_pool_scan_*).
	riverClient, err := api.BuildRiverClient(api.RiverDeps{
		Pool:   sharedHarness.Pool,
		Cfg:    config.WorkersConfig{Enabled: false, MaxWorkers: 1},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  s,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}

	// Cluster CA bootstrap mirrors the production boot hook so any
	// e2e test that touches /v1/ca or /v1/nodes/join-tokens sees an
	// active CA row. Idempotent — repeat calls by the shared DB
	// observe the existing row and no-op (race-safety).
	if err := api.BootstrapClusterCA(context.Background(), s,
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("BootstrapClusterCA: %v", err)
	}

	router := api.NewRouter(api.RouterDeps{
		Store:        s,
		AuthService:  svc,
		RiverClient:  riverClient,
		ImageDeleter: deleter,
		StoragePools: config.StoragePoolsConfig{
			AllowedPathPrefixes: []string{"/opt/otherix/pools/"},
		},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestTimeout: 10 * time.Second,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	t.Cleanup(s.Close)

	return &e2eHarness{srv: srv, store: s, svc: svc}
}

// seedUser creates a developer user with a real argon2id-hashed
// password and returns id, email, plaintext.
func seedUser(t *testing.T, ctx context.Context, s *store.Store) (uuid.UUID, string, string) {
	t.Helper()
	return seedUserWithRole(t, ctx, s, auth.RoleDeveloper)
}

// seedUserWithRole is the parameterised form of seedUser.
func seedUserWithRole(t *testing.T, ctx context.Context, s *store.Store, role auth.Role) (uuid.UUID, string, string) {
	t.Helper()
	const pw = "correct-horse-battery-staple"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	id := uuid.New()
	email := fmt.Sprintf("e2e-%s-%s@example.test", role, uuid.NewString())
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID: id, Email: email, PasswordHash: hash, Role: string(role),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id, email, pw
}

func TestE2E_NotFoundEnvelope(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/v1/no-such-endpoint", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeNotFound {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeNotFound)
	}
}

func TestE2E_LoginRefreshLogoutHappyPath(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	uid, email, pw := seedUser(t, ctx, h.store)

	// 1. Login.
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var login struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	decodeJSON(t, resp, &login)
	if login.AccessToken == "" || login.RefreshToken == "" {
		t.Fatal("login returned empty tokens")
	}
	if login.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", login.TokenType)
	}
	if login.ExpiresIn != 900 {
		t.Errorf("expires_in = %d, want 900", login.ExpiresIn)
	}
	if login.User.ID != uid.String() || login.User.Email != email {
		t.Errorf("user = %+v, want id=%v email=%v", login.User, uid, email)
	}

	// 2. /v1/users/me with the access token.
	resp = h.get(t, "/v1/users/me", login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("users.getMe status = %d, want 200", resp.StatusCode)
	}
	var me struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	decodeJSON(t, resp, &me)
	if me.ID != uid.String() {
		t.Errorf("getMe.id = %q, want %v", me.ID, uid)
	}

	// 3. Refresh rotates.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": login.RefreshToken}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	decodeJSON(t, resp, &refreshed)
	if refreshed.RefreshToken == login.RefreshToken {
		t.Errorf("refresh did not rotate")
	}

	// 4. Logout (refresh_token branch).
	resp = h.post(t, "/v1/auth/logout",
		map[string]any{"refresh_token": refreshed.RefreshToken},
		refreshed.AccessToken,
	)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("logout status = %d, want 204", resp.StatusCode)
	}

	// 5. Refresh after logout fails (replay-detection on revoked token).
	resp = h.post(t, "/v1/auth/refresh",
		map[string]string{"refresh_token": refreshed.RefreshToken},
		"",
	)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("post-logout refresh status = %d, want 401", resp.StatusCode)
	}
}

func TestE2E_LoginRejectsBadCredentials(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, email, _ := seedUser(t, ctx, h.store)

	resp := h.post(t, "/v1/auth/login",
		map[string]string{"email": email, "password": "wrong"}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeInvalidCredentials {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeInvalidCredentials)
	}
}

func TestE2E_GetMeRejectsMissingHeader(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/v1/users/me", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeUnauthenticated {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeUnauthenticated)
	}
}

func TestE2E_APITokenLifecycle(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, email, pw := seedUser(t, ctx, h.store)

	// Login to acquire JWT.
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, resp, &login)

	// Create an API token under the JWT.
	resp = h.post(t, "/v1/users/me/api-tokens",
		map[string]string{"name": "ci"}, login.AccessToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	decodeJSON(t, resp, &created)
	if !strings.HasPrefix(created.Token, "otx_") {
		t.Errorf("plaintext = %q, want otx_ prefix", created.Token)
	}
	if !strings.HasPrefix(created.Token, created.Prefix) {
		t.Errorf("token does not start with prefix %q", created.Prefix)
	}

	// The token authenticates /v1/users/me.
	resp = h.get(t, "/v1/users/me", created.Token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getMe(api-token) status = %d, want 200", resp.StatusCode)
	}

	// List shows the token (without plaintext).
	resp = h.get(t, "/v1/users/me/api-tokens", created.Token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Token string `json:"token"` // expected absent → empty
		} `json:"data"`
	}
	decodeJSON(t, resp, &list)
	if len(list.Data) != 1 {
		t.Errorf("list size = %d, want 1", len(list.Data))
	}
	if list.Data[0].Token != "" {
		t.Errorf("list leaked plaintext token: %q", list.Data[0].Token)
	}

	// Revoke.
	resp = h.delete(t, "/v1/users/me/api-tokens/"+created.ID, login.AccessToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", resp.StatusCode)
	}

	// Revoked token no longer authenticates.
	resp = h.get(t, "/v1/users/me", created.Token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked-token getMe status = %d, want 401", resp.StatusCode)
	}
}

func TestE2E_DeleteOtherUsersToken404(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, emailA, pwA := seedUser(t, ctx, h.store)
	_, emailB, pwB := seedUser(t, ctx, h.store)

	resp := h.post(t, "/v1/auth/login", map[string]string{"email": emailA, "password": pwA}, "")
	var loginA struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, resp, &loginA)

	resp = h.post(t, "/v1/users/me/api-tokens", map[string]string{"name": "a"}, loginA.AccessToken)
	var tokenA struct {
		ID string `json:"id"`
	}
	decodeJSON(t, resp, &tokenA)

	resp = h.post(t, "/v1/auth/login", map[string]string{"email": emailB, "password": pwB}, "")
	var loginB struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, resp, &loginB)

	// User B tries to delete user A's token. Must be 404 (never leak existence).
	resp = h.delete(t, "/v1/users/me/api-tokens/"+tokenA.ID, loginB.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user delete status = %d, want 404", resp.StatusCode)
	}
}
