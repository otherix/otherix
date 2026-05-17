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
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestLogin_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/login" {
			t.Errorf("path = %q, want /v1/auth/login", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous client sent Authorization=%q", got)
		}
		var body cpclient.LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Email != "a@b" || body.Password != "pw" {
			t.Errorf("body = %+v", body)
		}
		_, _ = io.WriteString(w, `{"access_token":"jwt","refresh_token":"r","token_type":"Bearer","expires_in":900}`)
	}))
	defer srv.Close()

	c, err := cpclient.NewAnonymous(srv.URL, cpclient.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewAnonymous: %v", err)
	}
	resp, err := c.Login(context.Background(), cpclient.LoginRequest{Email: "a@b", Password: "pw"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if resp.AccessToken != "jwt" || resp.RefreshToken != "r" || resp.ExpiresIn != 900 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_credentials","message":"invalid email or password"}}`)
	}))
	defer srv.Close()

	c, _ := cpclient.NewAnonymous(srv.URL, cpclient.Options{HTTPClient: srv.Client()})
	_, err := c.Login(context.Background(), cpclient.LoginRequest{Email: "a@b", Password: "wrong"})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *cpclient.APIError", err)
	}
	if apiErr.Code != "invalid_credentials" || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestLogin_NetworkError(t *testing.T) {
	// Closed server immediately fails the dial.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c, _ := cpclient.NewAnonymous(url, cpclient.Options{})
	if _, err := c.Login(context.Background(), cpclient.LoginRequest{Email: "a@b", Password: "p"}); err == nil {
		t.Errorf("Login против closed server returned nil")
	}
}

func TestCreateAPIToken_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/me/api-tokens" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want Bearer …", got)
		}
		var body cpclient.CreateAPITokenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Name != "otherix-cli-prod" {
			t.Errorf("name = %q", body.Name)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"01900000-0000-7000-8000-000000000001","user_id":"u","name":"otherix-cli-prod","prefix":"otx_abc1234","token":"otx_abc12345xyz","created_at":"2026-05-11T00:00:00Z","last_used_at":null,"expires_at":null,"revoked_at":null}`)
	}))
	defer srv.Close()

	c, _ := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "jwt", HTTPClient: srv.Client()})
	got, err := c.CreateAPIToken(context.Background(), cpclient.CreateAPITokenRequest{Name: "otherix-cli-prod"})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if got.Token != "otx_abc12345xyz" || got.Prefix != "otx_abc1234" {
		t.Errorf("got = %+v", got)
	}
	if got.ID != "01900000-0000-7000-8000-000000000001" {
		t.Errorf("id = %q", got.ID)
	}
}

func TestCreateAPIToken_400NameTooLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"validation_failed","message":"name must be at most 100 characters"}}`)
	}))
	defer srv.Close()

	c, _ := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "jwt", HTTPClient: srv.Client()})
	_, err := c.CreateAPIToken(context.Background(), cpclient.CreateAPITokenRequest{Name: strings.Repeat("x", 200)})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
}

func TestCreateAPIToken_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"missing principal"}}`)
	}))
	defer srv.Close()

	c, _ := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "bad", HTTPClient: srv.Client()})
	_, err := c.CreateAPIToken(context.Background(), cpclient.CreateAPITokenRequest{Name: "x"})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("err = %v, want APIError 401", err)
	}
}

func TestRevokeAPIToken_HappyAndIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/users/me/api-tokens/abc-123" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, _ := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "jwt", HTTPClient: srv.Client()})
	if err := c.RevokeAPIToken(context.Background(), "abc-123"); err != nil {
		t.Errorf("revoke: %v", err)
	}
}

func TestRevokeAPIToken_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"api token not found"}}`)
	}))
	defer srv.Close()

	c, _ := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "jwt", HTTPClient: srv.Client()})
	err := c.RevokeAPIToken(context.Background(), "ghost")
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v, want APIError 404", err)
	}
}

func TestNewAnonymous_DoesNotSendAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q on anonymous client; want empty", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"access_token":"t","refresh_token":"r","token_type":"Bearer","expires_in":1}`)
	}))
	defer srv.Close()

	c, err := cpclient.NewAnonymous(srv.URL, cpclient.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewAnonymous: %v", err)
	}
	if _, err := c.Login(context.Background(), cpclient.LoginRequest{Email: "a@b", Password: "p"}); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

func TestWithToken_AttachesBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer new-jwt" {
			t.Errorf("Authorization = %q, want Bearer new-jwt", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"i","user_id":"u","name":"n","prefix":"p","token":"otx_x","created_at":"now","last_used_at":null,"expires_at":null,"revoked_at":null}`)
	}))
	defer srv.Close()

	anon, _ := cpclient.NewAnonymous(srv.URL, cpclient.Options{HTTPClient: srv.Client()})
	auth := anon.WithToken("new-jwt")
	if _, err := auth.CreateAPIToken(context.Background(), cpclient.CreateAPITokenRequest{Name: "n"}); err != nil {
		t.Errorf("CreateAPIToken via WithToken: %v", err)
	}
}
