// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeCP captures incoming heartbeat POSTs for inspection.
type fakeCP struct {
	mu       sync.Mutex
	requests [][]byte
	srv      *httptest.Server
}

func newFakeCP(t *testing.T) *fakeCP {
	t.Helper()
	cp := &fakeCP{}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		cp.mu.Lock()
		cp.requests = append(cp.requests, body)
		cp.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received_at":"2026-05-06T00:00:00Z"}`))
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (f *fakeCP) snapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.requests))
	copy(out, f.requests)
	return out
}

func TestSendHeartbeatNow_PushesBody(t *testing.T) {
	cp := newFakeCP(t)
	m := Start(t, Options{
		ControlPlaneURL:   cp.srv.URL,
		HeartbeatInterval: time.Hour, // long; only manual SendHeartbeatNow fires.
	})
	if err := m.SetCapability("kvm_available", true); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	m.SuppressHeartbeats() // belt-and-suspenders against the long ticker.

	status, err := m.SendHeartbeatNow(context.Background())
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}

	got := cp.snapshot()
	if len(got) != 1 {
		t.Fatalf("len(cp.requests) = %d, want 1", len(got))
	}
	var hb heartbeatRequest
	if err := json.Unmarshal(got[0], &hb); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, got[0])
	}
	if !hb.Capabilities.KvmAvailable {
		t.Errorf("kvm_available = false, want true (preloaded via SetCapability)")
	}
	if hb.Architecture != "amd64" {
		t.Errorf("architecture = %q, want amd64", hb.Architecture)
	}
}

func TestHeartbeat_MigrationOnlyWhenChanged(t *testing.T) {
	cp := newFakeCP(t)
	m := Start(t, Options{
		ControlPlaneURL:   cp.srv.URL,
		HeartbeatInterval: time.Hour,
	})
	m.SuppressHeartbeats()

	if _, err := m.SendHeartbeatNow(context.Background()); err != nil {
		t.Fatalf("SendHeartbeatNow #1: %v", err)
	}
	if _, err := m.SendHeartbeatNow(context.Background()); err != nil {
		t.Fatalf("SendHeartbeatNow #2: %v", err)
	}
	m.SetMigrationCapability(MigrationCapability{
		Host:           "10.0.0.42",
		PortRangeStart: 60000,
		PortRangeEnd:   60100,
	})
	if _, err := m.SendHeartbeatNow(context.Background()); err != nil {
		t.Fatalf("SendHeartbeatNow #3: %v", err)
	}

	got := cp.snapshot()
	if len(got) != 3 {
		t.Fatalf("len(cp.requests) = %d, want 3", len(got))
	}
	first := unmarshalHeartbeat(t, got[0])
	second := unmarshalHeartbeat(t, got[1])
	third := unmarshalHeartbeat(t, got[2])
	if first.Migration == nil {
		t.Errorf("first heartbeat: migration=nil; want present (initial value differs from <unset>)")
	}
	if second.Migration != nil {
		t.Errorf("second heartbeat: migration set despite no change; want nil")
	}
	if third.Migration == nil {
		t.Errorf("third heartbeat: migration=nil after capability change; want present")
	} else if third.Migration.Host != "10.0.0.42" {
		t.Errorf("third migration.Host = %q, want 10.0.0.42", third.Migration.Host)
	}
}

func TestHeartbeat_GoroutineFires(t *testing.T) {
	cp := newFakeCP(t)
	m := Start(t, Options{
		ControlPlaneURL:   cp.srv.URL,
		HeartbeatInterval: 50 * time.Millisecond,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cp.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(cp.snapshot()); got < 2 {
		t.Errorf("captured %d heartbeats in 2s, want >= 2", got)
	}
	_ = m
}

func TestHeartbeat_SuppressedSkipsTicks(t *testing.T) {
	cp := newFakeCP(t)
	m := Start(t, Options{
		ControlPlaneURL:   cp.srv.URL,
		HeartbeatInterval: 30 * time.Millisecond,
	})
	m.SuppressHeartbeats()

	time.Sleep(200 * time.Millisecond)
	if got := len(cp.snapshot()); got > 0 {
		t.Errorf("captured %d heartbeats while suppressed, want 0", got)
	}

	m.ResumeHeartbeats()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cp.snapshot()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(cp.snapshot()); got < 1 {
		t.Errorf("captured %d heartbeats after Resume, want >= 1", got)
	}
}

func unmarshalHeartbeat(t *testing.T, body []byte) heartbeatRequest {
	t.Helper()
	var hb heartbeatRequest
	if err := json.Unmarshal(body, &hb); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	return hb
}
