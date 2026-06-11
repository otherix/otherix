// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// stubVerifier is a hand-rolled test double; see Google Go Style — no
// assertion libraries, hand-rolled doubles named with the *Stub suffix.
type stubVerifier struct {
	jwtUser *auth.User
	jwtErr  error
	apiUser *auth.User
	apiErr  error

	gotJWT string
	gotAPI string
}

func (s *stubVerifier) VerifyAccessToken(raw string) (*auth.User, error) {
	s.gotJWT = raw
	return s.jwtUser, s.jwtErr
}

func (s *stubVerifier) VerifyAPIToken(_ context.Context, raw string) (*auth.User, error) {
	s.gotAPI = raw
	return s.apiUser, s.apiErr
}

// captureUser is a downstream handler that records the authenticated
// principal it sees on the request context.
type captureUser struct{ got *auth.User }

func (c *captureUser) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.got = auth.UserFromContext(r.Context())
}

func decodeError(t *testing.T, resp *http.Response) response.ErrorBody {
	t.Helper()
	var body response.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func TestAuthnMissingHeader(t *testing.T) {
	v := &stubVerifier{}
	cap := &captureUser{}
	h := middleware.Authn(v)(cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	body := decodeError(t, rec.Result())
	if body.Error.Code != response.CodeUnauthenticated {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeUnauthenticated)
	}
	if cap.got != nil {
		t.Errorf("downstream handler ran on missing-header request: got user = %+v", cap.got)
	}
}

func TestAuthnMalformedHeader(t *testing.T) {
	tests := []struct {
		name string
		auth string
	}{
		{name: "no scheme", auth: "abc"},
		{name: "wrong scheme", auth: "Basic abc"},
		{name: "scheme only", auth: "Bearer"},
		{name: "bearer empty", auth: "Bearer "},
		{name: "bearer whitespace", auth: "Bearer    "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := middleware.Authn(&stubVerifier{})(&captureUser{})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestAuthnBearerSchemeCaseInsensitive covers RFC 7235: the auth-scheme
// is case-insensitive, so `bearer`, `BEARER`, etc. must authenticate.
// The token after the scheme must reach the verifier verbatim - the
// downstream otx_ prefix match is case-sensitive.
func TestAuthnBearerSchemeCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			v := &stubVerifier{
				jwtUser: &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT},
			}
			cap := &captureUser{}
			h := middleware.Authn(v)(cap)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", scheme+" eyJ.fake.jwt")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if v.gotJWT != "eyJ.fake.jwt" {
				t.Errorf("VerifyAccessToken got %q, want %q", v.gotJWT, "eyJ.fake.jwt")
			}
			if cap.got == nil {
				t.Error("downstream handler saw no principal")
			}
		})
	}
}

func TestAuthnDispatchesByPrefix(t *testing.T) {
	uid := uuid.New()
	v := &stubVerifier{
		jwtUser: &auth.User{ID: uid, Role: auth.RoleAdmin, Type: auth.TypeJWT},
		apiUser: &auth.User{ID: uid, Role: auth.RoleViewer, Type: auth.TypeAPIToken},
	}
	cap := &captureUser{}
	h := middleware.Authn(v)(cap)

	t.Run("jwt path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer eyJ.fake.jwt")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if v.gotJWT != "eyJ.fake.jwt" {
			t.Errorf("VerifyAccessToken got %q, want %q", v.gotJWT, "eyJ.fake.jwt")
		}
		if v.gotAPI != "" {
			t.Errorf("API path called for JWT input: got %q", v.gotAPI)
		}
		if cap.got == nil || cap.got.Type != auth.TypeJWT {
			t.Errorf("downstream user = %+v, want type=jwt", cap.got)
		}
	})

	v.gotJWT, v.gotAPI = "", ""
	cap.got = nil

	t.Run("api token path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer otx_abcdef")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if v.gotAPI != "otx_abcdef" {
			t.Errorf("VerifyAPIToken got %q, want %q", v.gotAPI, "otx_abcdef")
		}
		if v.gotJWT != "" {
			t.Errorf("JWT path called for otx_ input: got %q", v.gotJWT)
		}
		if cap.got == nil || cap.got.Type != auth.TypeAPIToken {
			t.Errorf("downstream user = %+v, want type=api_token", cap.got)
		}
	})
}

func TestAuthnMapsErrorsToCodes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode response.ErrorCode
		wantMsg  string
	}{
		{name: "expired", err: auth.ErrTokenExpired, wantCode: response.CodeUnauthenticated, wantMsg: "token expired"},
		{name: "revoked", err: auth.ErrTokenRevoked, wantCode: response.CodeTokenRevoked, wantMsg: "token revoked"},
		{name: "invalid", err: auth.ErrInvalidToken, wantCode: response.CodeUnauthenticated, wantMsg: "invalid token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &stubVerifier{jwtErr: tc.err}
			h := middleware.Authn(v)(&captureUser{})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer eyJ.x")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			body := decodeError(t, rec.Result())
			if body.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message != tc.wantMsg {
				t.Errorf("error.message = %q, want %q", body.Error.Message, tc.wantMsg)
			}
		})
	}
}
