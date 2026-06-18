// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
)

func TestServeBlob_HappyPath(t *testing.T) {
	t.Parallel()

	wantEndpoint := "https://10.0.0.7:49300"
	wantExpiry := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/blobs/serve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"serve_endpoint": wantEndpoint,
			"expires_at":     wantExpiry,
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	req := agentapi.BlobServeRequest{
		Digest:         "abc123",
		Token:          "tok",
		ConsumerNodeID: uuid.New(),
	}
	got, err := newClientAutoDir(t).ServeBlob(context.Background(), baseURL, req)
	if err != nil {
		t.Fatalf("ServeBlob: %v", err)
	}
	if got.ServeEndpoint != wantEndpoint {
		t.Errorf("serve_endpoint = %q, want %q", got.ServeEndpoint, wantEndpoint)
	}
	if !got.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, wantExpiry)
	}
}

func TestServeBlob_Conflict409(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/blobs/serve", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "conflict", "message": "no artifact ports free"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).ServeBlob(
		context.Background(), baseURL, agentapi.BlobServeRequest{Digest: "abc", ConsumerNodeID: uuid.New()})
	if err == nil {
		t.Fatalf("expected error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v; want AgentError 409", err)
	}
}

func TestPullBlob_HappyPath(t *testing.T) {
	t.Parallel()

	wantTaskID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/blobs/pull", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		jsonWrite(t, w, http.StatusAccepted, map[string]any{
			"task_id": wantTaskID,
			"status":  "pending",
			"links":   map[string]any{"self": "/v1/tasks/" + wantTaskID.String()},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	req := agentapi.BlobPullRequest{
		Digest:         "abc123",
		Token:          "tok",
		HolderEndpoint: "https://10.0.0.7:49300",
	}
	gotTaskID, err := newClientAutoDir(t).PullBlob(context.Background(), baseURL, req)
	if err != nil {
		t.Fatalf("PullBlob: %v", err)
	}
	if gotTaskID != wantTaskID.String() {
		t.Errorf("task_id = %s, want %s", gotTaskID, wantTaskID)
	}
}

func TestPullBlob_Conflict409(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/blobs/pull", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "conflict", "message": "already pulling"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).PullBlob(
		context.Background(), baseURL, agentapi.BlobPullRequest{Digest: "abc"})
	if err == nil {
		t.Fatalf("expected error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v; want AgentError 409", err)
	}
}
