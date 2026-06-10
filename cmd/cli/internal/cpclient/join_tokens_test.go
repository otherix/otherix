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

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newTestClient(t *testing.T, url string) *cpclient.Client {
	t.Helper()
	c, err := cpclient.New(cpclient.Options{
		BaseURL: url,
		Token:   "test-token",
	})
	if err != nil {
		t.Fatalf("cpclient.New: %v", err)
	}
	return c
}

func TestCreateJoinToken_HappyPath(t *testing.T) {
	t.Parallel()
	tokenID := uuid.NewString()
	createdBy := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/nodes/join-tokens" {
			t.Errorf("path = %s, want /v1/nodes/join-tokens", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                    tokenID,
			"intended_node_name":    nil,
			"expires_at":            "2026-05-13T13:00:00Z",
			"max_uses":              nil,
			"consumption_count":     0,
			"created_by_user_id":    createdBy,
			"created_at":            "2026-05-13T12:00:00Z",
			"token":                 "otx_join_abcdef",
			"ca_fingerprint_sha256": strings.Repeat("a", 64),
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	out, err := c.CreateJoinToken(context.Background(), cpclient.CreateJoinTokenRequest{})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	if out.Token != "otx_join_abcdef" {
		t.Errorf("Token = %q, want otx_join_abcdef", out.Token)
	}
	if out.CAFingerprintSHA256 != strings.Repeat("a", 64) {
		t.Errorf("CAFingerprintSHA256 = %q, want 64 'a' chars", out.CAFingerprintSHA256)
	}
	if out.ConsumptionCount != 0 {
		t.Errorf("ConsumptionCount = %d, want 0", out.ConsumptionCount)
	}
}

func TestCreateJoinToken_SendsKind(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                    uuid.NewString(),
			"intended_node_name":    nil,
			"expires_at":            "2026-06-10T00:00:00Z",
			"max_uses":              1,
			"consumption_count":     0,
			"created_by_user_id":    nil,
			"created_at":            "2026-06-10T00:00:00Z",
			"token":                 "otx_join_x",
			"ca_fingerprint_sha256": strings.Repeat("a", 64),
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	kind := "cluster"
	if _, err := c.CreateJoinToken(context.Background(), cpclient.CreateJoinTokenRequest{Kind: &kind}); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	if got := gotBody["kind"]; got != "cluster" {
		t.Errorf("request body kind = %v, want %q", got, "cluster")
	}
}

func TestListJoinTokens_QueryParams(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("include_expired"); got != "true" {
			t.Errorf("include_expired = %q, want true", got)
		}
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Errorf("limit = %q, want 10", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ListJoinTokens(context.Background(), cpclient.ListJoinTokensParams{
		Limit:          10,
		IncludeExpired: true,
	})
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
}

func TestDeleteJoinToken_HappyPath(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, id.String()) {
			t.Errorf("path = %s, want suffix %s", r.URL.Path, id)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DeleteJoinToken(context.Background(), id); err != nil {
		t.Errorf("DeleteJoinToken: %v", err)
	}
}

func TestDeleteJoinToken_AlreadyExpired(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "conflict",
				"message": "join token already expired",
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.DeleteJoinToken(context.Background(), id)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want 409", apiErr.StatusCode)
	}
}

func TestListJoinTokenConsumptions_Empty(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/consumptions") {
			t.Errorf("path = %s, want suffix /consumptions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	out, err := c.ListJoinTokenConsumptions(context.Background(), id, cpclient.ListJoinTokenConsumptionsParams{})
	if err != nil {
		t.Fatalf("ListJoinTokenConsumptions: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("Data length = %d, want 0", len(out.Data))
	}
}
