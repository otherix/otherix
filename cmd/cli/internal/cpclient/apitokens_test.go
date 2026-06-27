// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewAnonymous(url, Options{})
	if err != nil {
		t.Fatalf("NewAnonymous: %v", err)
	}
	return c.WithToken("test-jwt")
}

func TestCreateAPITokenFor_SelfPath(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "ci" {
			t.Errorf("name = %v, want ci", body["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id1","user_id":"u1","name":"ci","prefix":"otx_ab12","token":"otx_secret","created_at":"2026-06-27T10:00:00Z"}`))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).CreateAPITokenFor(context.Background(), "", CreateAPITokenRequest{Name: "ci"})
	if err != nil {
		t.Fatalf("CreateAPITokenFor: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/users/me/api-tokens" {
		t.Errorf("request = %s %s, want POST /v1/users/me/api-tokens", gotMethod, gotPath)
	}
	if got.Token != "otx_secret" {
		t.Errorf("Token = %q, want otx_secret", got.Token)
	}
}

func TestCreateAPITokenFor_AdminOnBehalfPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id1","user_id":"u9","name":"ci","prefix":"otx_ab12","token":"otx_x","created_at":"2026-06-27T10:00:00Z"}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv.URL).CreateAPITokenFor(context.Background(), "u9", CreateAPITokenRequest{Name: "ci"}); err != nil {
		t.Fatalf("CreateAPITokenFor: %v", err)
	}
	if gotPath != "/v1/users/u9/api-tokens" {
		t.Errorf("path = %s, want /v1/users/u9/api-tokens", gotPath)
	}
}

func TestListAPITokensFor_QueryAndPath(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"id1","user_id":"u1","name":"ci","prefix":"otx_ab12","created_at":"2026-06-27T10:00:00Z"}],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	page, err := newTestClient(t, srv.URL).ListAPITokensFor(context.Background(), "", ListAPITokensParams{Limit: 5, IncludeRevoked: true})
	if err != nil {
		t.Fatalf("ListAPITokensFor: %v", err)
	}
	if gotPath != "/v1/users/me/api-tokens" {
		t.Errorf("path = %s, want /v1/users/me/api-tokens", gotPath)
	}
	if gotQuery != "include_revoked=true&limit=5" {
		t.Errorf("query = %s, want include_revoked=true&limit=5", gotQuery)
	}
	if len(page.Data) != 1 || page.Data[0].Prefix != "otx_ab12" {
		t.Errorf("page.Data = %+v, want one token with prefix otx_ab12", page.Data)
	}
}

func TestRevokeAPITokenFor_Path(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL).RevokeAPITokenFor(context.Background(), "u9", "tok1"); err != nil {
		t.Fatalf("RevokeAPITokenFor: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/users/u9/api-tokens/tok1" {
		t.Errorf("request = %s %s, want DELETE /v1/users/u9/api-tokens/tok1", gotMethod, gotPath)
	}
}
