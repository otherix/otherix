// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
)

func TestStartIncomingMigration_HappyPath(t *testing.T) {
	t.Parallel()

	vmName := "demo"
	migrationID := uuid.New()
	wantAuthToken := "tok-" + uuid.NewString()
	wantListen := "10.0.0.7:49152"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/"+vmName+"/migrations/incoming", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"migration_id":    migrationID,
			"auth_token":      wantAuthToken,
			"listen_endpoint": wantListen,
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	req := agentapi.MigrationIncomingRequest{
		MigrationID: migrationID,
		Mode:        agentapi.MigrationIncomingRequestMode("live"),
	}
	got, err := newClientAutoDir(t).StartIncomingMigration(context.Background(), baseURL, vmName, req)
	if err != nil {
		t.Fatalf("StartIncomingMigration: %v", err)
	}
	if got.MigrationID != migrationID {
		t.Errorf("migration_id = %s, want %s", got.MigrationID, migrationID)
	}
	if got.AuthToken != wantAuthToken {
		t.Errorf("auth_token = %q, want %q", got.AuthToken, wantAuthToken)
	}
	if got.ListenEndpoint != wantListen {
		t.Errorf("listen_endpoint = %q, want %q", got.ListenEndpoint, wantListen)
	}
}

func TestStartIncomingMigration_Conflict409(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/demo/migrations/incoming", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "conflict", "message": "no migration ports free"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).StartIncomingMigration(
		context.Background(), baseURL, "demo", agentapi.MigrationIncomingRequest{MigrationID: uuid.New()})
	if err == nil {
		t.Fatalf("expected error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v; want AgentError 409", err)
	}
}

func TestStartOutgoingMigration_HappyPath(t *testing.T) {
	t.Parallel()

	vmName := "demo"
	migrationID := uuid.New()
	wantTaskID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/"+vmName+"/migrations/outgoing", func(w http.ResponseWriter, r *http.Request) {
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

	req := agentapi.MigrationOutgoingRequest{
		MigrationID:    migrationID,
		Mode:           agentapi.MigrationOutgoingRequestMode("live"),
		AuthToken:      "tok",
		TargetEndpoint: "10.0.0.7:49152",
	}
	gotTaskID, err := newClientAutoDir(t).StartOutgoingMigration(context.Background(), baseURL, vmName, req)
	if err != nil {
		t.Fatalf("StartOutgoingMigration: %v", err)
	}
	if gotTaskID != wantTaskID.String() {
		t.Errorf("task_id = %s, want %s", gotTaskID, wantTaskID)
	}
}

func TestStartOutgoingMigration_MalformedAccepted(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/demo/migrations/outgoing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("not json"))
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).StartOutgoingMigration(
		context.Background(), baseURL, "demo", agentapi.MigrationOutgoingRequest{MigrationID: uuid.New()})
	if err == nil {
		t.Fatalf("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode AsyncTaskAccepted") {
		t.Errorf("err = %v; want decode-error wrapper", err)
	}
}

func TestGetMigration_HappyPath(t *testing.T) {
	t.Parallel()

	vmName := "demo"
	migrationID := uuid.New()
	vmUUID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/"+vmName+"/migrations/"+migrationID.String(), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"migration_id":      migrationID,
			"vm_uuid":           vmUUID,
			"role":              "source",
			"mode":              "live",
			"phase":             "active",
			"progress_percent":  42,
			"bytes_transferred": 1024,
			"created_at":        "2026-05-09T12:00:00Z",
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	got, err := newClientAutoDir(t).GetMigration(context.Background(), baseURL, vmName, migrationID.String())
	if err != nil {
		t.Fatalf("GetMigration: %v", err)
	}
	if got.MigrationID != migrationID {
		t.Errorf("migration_id = %s, want %s", got.MigrationID, migrationID)
	}
	if got.Phase != agentapi.MigrationPhaseActive {
		t.Errorf("phase = %q, want %q", got.Phase, agentapi.MigrationPhaseActive)
	}
	if got.ProgressPercent != 42 {
		t.Errorf("progress_percent = %d, want 42", got.ProgressPercent)
	}
}

func TestGetMigration_NotFound(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/demo/migrations/", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "migration not found"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).GetMigration(context.Background(), baseURL, "demo", uuid.NewString())
	if err == nil {
		t.Fatalf("expected 404 error")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("err = %v; want AgentError 404", err)
	}
}

func TestCancelMigration_HappyPath(t *testing.T) {
	t.Parallel()

	vmName := "demo"
	migrationID := uuid.New()
	vmUUID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/"+vmName+"/migrations/"+migrationID.String()+"/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"migration_id":      migrationID,
			"vm_uuid":           vmUUID,
			"role":              "source",
			"mode":              "live",
			"phase":             "cancelled",
			"progress_percent":  10,
			"bytes_transferred": 0,
			"created_at":        "2026-05-09T12:00:00Z",
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	got, err := newClientAutoDir(t).CancelMigration(context.Background(), baseURL, vmName, migrationID.String())
	if err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}
	if got.MigrationID != migrationID {
		t.Errorf("migration_id = %s, want %s", got.MigrationID, migrationID)
	}
	if got.Phase != agentapi.MigrationPhaseCancelled {
		t.Errorf("phase = %q, want %q", got.Phase, agentapi.MigrationPhaseCancelled)
	}
}

func TestCancelMigration_Conflict409(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/demo/migrations/", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "conflict", "message": "already completed"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).CancelMigration(context.Background(), baseURL, "demo", uuid.NewString())
	if err == nil {
		t.Fatalf("expected error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v; want AgentError 409", err)
	}
}

func TestPostHeartbeatNudge(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/heartbeat/nudge", func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	_, baseURL := startAgentTLSServer(t, mux)

	if err := newClientAutoDir(t).PostHeartbeatNudge(context.Background(), baseURL); err != nil {
		t.Fatalf("PostHeartbeatNudge: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/heartbeat/nudge" {
		t.Errorf("got %s %s, want POST /v1/heartbeat/nudge", gotMethod, gotPath)
	}
}
