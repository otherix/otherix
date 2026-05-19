// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/agentmock"
)

// TestSmoke_MockAgentHealth is the entry-level CP-↔-agent integration
// signal: the mock fixture starts, serves /v1/health, and returns the
// codegen-typed response. Anything richer (heartbeat receiver, mTLS
// handshake, VM-lifecycle pulls) belongs in dedicated test files.
func TestSmoke_MockAgentHealth(t *testing.T) {
	mock := agentmock.Start(t, agentmock.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mock.URL()+"/v1/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body agentapi.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != agentapi.Ok {
		t.Errorf("status = %q, want %q", body.Status, agentapi.Ok)
	}
}
