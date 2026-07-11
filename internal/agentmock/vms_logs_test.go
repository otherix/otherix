// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestHandler_VmsLogs_NotFound exercises the agent-side route gate:
// a GET against /v1/vms/{name}/logs for a VM the mock has never
// materialised must return 404 vm_not_found.
func TestHandler_VmsLogs_NotFound(t *testing.T) {
	m := Start(t, Options{})
	resp := mustGet(t, m.URL()+"/v1/vms/never-existed/logs")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "vm_not_found") {
		t.Errorf("body = %q, want code vm_not_found", body)
	}
}

// TestHandler_VmsLogs_BannerOnKnownVM pins the deterministic banner
// the mock emits so integration tests (CLI + CP-proxy round trips)
// have stable bytes to assert against. follow=false closes after
// the banner without any tick output.
func TestHandler_VmsLogs_BannerOnKnownVM(t *testing.T) {
	m := Start(t, Options{})
	m.SetStoredVM(AgentVM{
		ID:           uuid.New(),
		Name:         "demo",
		VCPUs:        1,
		MemoryMib:    128,
		PoolName:     "pool-a",
		Architecture: "amd64",
		Status:       "running",
	})

	resp := mustGet(t, m.URL()+"/v1/vms/demo/logs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := "MOCK_AGENT_VM_LOGS_READY vm=demo\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// TestHandler_VmsLogs_FollowEmitsTicks drives follow=true and reads
// until the first tick line appears, then cancels the request. Pins
// that the follow path keeps the response open and writes additional
// bytes after the banner.
func TestHandler_VmsLogs_FollowEmitsTicks(t *testing.T) {
	m := Start(t, Options{})
	m.SetStoredVM(AgentVM{
		ID:           uuid.New(),
		Name:         "tickvm",
		VCPUs:        1,
		MemoryMib:    128,
		PoolName:     "pool-a",
		Architecture: "amd64",
		Status:       "running",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.URL()+"/v1/vms/tickvm/logs?follow=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	banner, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if !strings.Contains(banner, "MOCK_AGENT_VM_LOGS_READY") {
		t.Errorf("banner = %q, want to contain MOCK_AGENT_VM_LOGS_READY", banner)
	}
	tick, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read tick: %v", err)
	}
	if !strings.Contains(tick, "MOCK_AGENT_LIVE_LOG_TICK") {
		t.Errorf("tick = %q, want to contain MOCK_AGENT_LIVE_LOG_TICK", tick)
	}
	cancel()
}
