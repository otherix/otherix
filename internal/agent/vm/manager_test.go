// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/config"
)

func newTestConfig(t *testing.T) (*config.AgentConfig, string, string) {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "vms")
	poolRoot := filepath.Join(tmp, "pools", "default")
	poolName := "default"
	cfg := &config.AgentConfig{
		StatePath: stateDir,
		QEMU: config.QEMUConfig{
			AArch64FirmwarePath: "/usr/share/AAVMF/AAVMF_CODE.fd",
		},
	}
	return cfg, poolRoot, poolName
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestManager_New_ValidatesStatePath(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*config.AgentConfig)
		wantErr bool
	}{
		{"empty state path", func(c *config.AgentConfig) { c.StatePath = "" }, true},
		{"valid", func(*config.AgentConfig) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTestConfig(t)
			tc.mut(cfg)
			_, err := New(cfg, discardLogger())
			if tc.wantErr && err == nil {
				t.Fatalf("New(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestManager_AddPool_Validates(t *testing.T) {
	cases := []struct {
		name    string
		pool    string
		root    func(t *testing.T) string
		wantErr bool
	}{
		{"empty name", "", func(*testing.T) string { return "/tmp" }, true},
		{"empty root", "p", func(*testing.T) string { return "" }, true},
		{"relative root", "p", func(*testing.T) string { return "relative" }, true},
		{"valid", "p", func(t *testing.T) string { return t.TempDir() }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTestConfig(t)
			m, err := New(cfg, discardLogger())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = m.AddPool(tc.pool, tc.root(t))
			if tc.wantErr && err == nil {
				t.Fatalf("AddPool(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("AddPool(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestManager_Create_ValidationErrors(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	validChecksum := "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	cases := []struct {
		name string
		spec CreateSpec
	}{
		{"empty name", CreateSpec{Name: "", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"low vcpus", CreateSpec{Name: "x", VCPUs: 0, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"high vcpus", CreateSpec{Name: "x", VCPUs: 200, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"low memory", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 64, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"empty checksum", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: ""}},
		{"non-hex checksum", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}},
		{"empty pool", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: "", TemplateChecksum: validChecksum}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Create(t.Context(), tc.spec)
			if err == nil {
				t.Fatalf("Create(%s) = nil, want error", tc.name)
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("Create(%s) error = %v, want ErrInvalidSpec", tc.name, err)
			}
		})
	}
}

func TestManager_Create_UnknownPool(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	validChecksum := "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	_, err = m.Create(t.Context(), CreateSpec{
		Name:             "x",
		VCPUs:            2,
		MemoryMB:         1024,
		PoolName:         "not-the-configured-pool",
		TemplateChecksum: validChecksum,
	})
	if !errors.Is(err, ErrPoolUnknown) {
		t.Errorf("err = %v, want ErrPoolUnknown", err)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Get(uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestManager_List_Empty(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := m.List(); len(got) != 0 {
		t.Errorf("List() = %d entries, want 0", len(got))
	}
}

func TestManager_InFlightGuard_AcquireReleaseAndQuery(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = true before any acquire")
	}
	release, ok := m.inFlightAcquire("foo")
	if !ok {
		t.Fatalf("first acquire on foo failed")
	}
	if !m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = false after acquire")
	}
	if _, ok := m.inFlightAcquire("foo"); ok {
		t.Errorf("second acquire on foo succeeded (must reject)")
	}
	if _, ok := m.inFlightAcquire("bar"); !ok {
		t.Errorf("acquire on independent name bar failed")
	}
	release()
	if m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = true after release")
	}
	if _, ok := m.inFlightAcquire("foo"); !ok {
		t.Errorf("re-acquire on foo after release failed")
	}
}

func TestManager_InFlightGuard_EmptyName_IsNoOp(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	release, ok := m.inFlightAcquire("")
	if !ok {
		t.Errorf("empty name acquire returned ok=false (must be no-op true)")
	}
	if m.HasInFlight("") {
		t.Errorf("HasInFlight(\"\") = true (must be no-op false)")
	}
	release()
}
