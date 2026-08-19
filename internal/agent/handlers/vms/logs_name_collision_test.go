// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/state"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/config"
)

// serialSocketStub listens on a Unix socket and greets every connection
// with marker, standing in for a running qemu's -serial chardev. It lets
// a test tell two VMs' consoles apart by their bytes.
func serialSocketStub(t *testing.T, marker string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "oxsock")
	if err != nil {
		t.Skipf("cannot create short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				_, _ = conn.Write([]byte(marker + "\n"))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return path
}

// TestLogs_SameNameVMsServeTheNewestConsole drives the real
// name-addressed HTTP entry point over a manager that holds two running
// VMs sharing one name - the state the agent lands in when the control
// plane force-deletes a VM (leaving the agent an orphan it no longer
// declares) and the operator recreates one under the same name. The
// stream must carry the newest VM's serial output, on every run.
func TestLogs_SameNameVMsServeTheNewestConsole(t *testing.T) {
	// The defect this pins was a coin flip on Go's map iteration order,
	// both in name resolution and in the multiplexer registry. Repeat the
	// whole setup so a lucky ordering cannot make a broken build pass.
	for range 8 {
		body := logsBodyForDuplicateNames(t)
		if !strings.Contains(body, "NEWEST") {
			t.Fatalf("logs body = %q, want the newest same-named VM's console output", body)
		}
		if strings.Contains(body, "ORPHAN") {
			t.Fatalf("logs body = %q, leaked the orphaned same-named VM's console output", body)
		}
	}
}

// logsBodyForDuplicateNames builds a manager over two running VMs named
// "dup" (an older orphan and a newer recreation), issues one
// GET /v1/vms/dup/logs through the real router, and returns the streamed
// body.
func logsBodyForDuplicateNames(t *testing.T) string {
	t.Helper()

	stateDir := t.TempDir()
	writeRunningMeta(t, stateDir, "dup", time.Now().UTC().Add(-time.Hour), serialSocketStub(t, "ORPHAN"))
	writeRunningMeta(t, stateDir, "dup", time.Now().UTC(), serialSocketStub(t, "NEWEST"))

	cfg := &config.AgentConfig{
		StatePath: stateDir,
		Migration: config.MigrationConfig{
			Host:           "127.0.0.1",
			PortRangeStart: 49152,
			PortRangeEnd:   49251,
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := vm.New(cfg, &netfabric.FakeFabric{}, log)
	if err != nil {
		t.Fatalf("vm.New: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/v1/vms/{vm_name}/logs", New(m, nil, log, "127.0.0.1", nil).Logs)

	// The multiplexer pumps the socket bytes asynchronously; retry until
	// history is available rather than racing the first read.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/vms/dup/logs", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/vms/dup/logs = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); strings.TrimSpace(body) != "" {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatal("no serial history streamed within 3s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeRunningMeta(t *testing.T, stateDir, name string, createdAt time.Time, consoleSocket string) {
	t.Helper()
	id := uuid.New()
	if err := state.WriteMeta(filepath.Join(stateDir, id.String()), &state.VMMeta{
		VMID:          id,
		Name:          name,
		VCPUs:         1,
		MemoryMib:     512,
		PoolName:      "default",
		Architecture:  string(qemu.HostArch()),
		ConsoleSocket: consoleSocket,
		Status:        string(vm.StatusRunning),
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}); err != nil {
		t.Fatalf("WriteMeta(%s): %v", name, err)
	}
}
