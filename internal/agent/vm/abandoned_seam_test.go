// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/handlers/vms"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agentapi"
)

// abandonedSeam is a real agent VM handler over a real Manager holding a
// stopped copy of a VM adopted by the offline migration old.
type abandonedSeam struct {
	srv      *httptest.Server
	poolRoot string
	vmID     uuid.UUID
	old      uuid.UUID
	disk     string
}

func newAbandonedSeam(t *testing.T) *abandonedSeam {
	t.Helper()
	m, _ := vm.NewManagerForSeamTest(t)
	s := &abandonedSeam{vmID: uuid.New(), old: uuid.New()}
	for _, p := range m.ListPools() {
		if p.Name == m.DefaultPoolForSeamTest() {
			s.poolRoot = p.Root
		}
	}
	v, err := m.AdoptForMigration(vm.AdoptSpec{
		UUID: s.vmID, Name: "cold", VCPUs: 1, MemoryMib: 512,
		PoolName: m.DefaultPoolForSeamTest(), Architecture: "amd64", MigrationID: s.old,
	})
	if err != nil {
		t.Fatalf("AdoptForMigration: %v", err)
	}
	s.disk = v.DiskPath
	if err := os.MkdirAll(filepath.Dir(s.disk), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.disk, []byte("abandoned copy"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	h := vms.New(m, console.NewTokenStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "10.0.0.2", nil)
	r := chi.NewRouter()
	r.Route("/v1/vms", h.Mount)
	s.srv = httptest.NewServer(r)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *abandonedSeam) postIncoming(t *testing.T, abandoned *[]uuid.UUID) int {
	t.Helper()
	identity := "CN=node-src"
	body, err := json.Marshal(agentapi.MigrationIncomingRequest{
		MigrationID:           uuid.New(),
		Mode:                  agentapi.MigrationIncomingRequestModeOffline,
		SourceNodeIdentity:    &identity,
		AbandonedMigrationIds: abandoned,
		VMSpec: agentapi.VMSpec{
			VMUUID: s.vmID, Name: "cold", CPUCores: 1, MemoryMib: 512, Architecture: "amd64",
			Disks: []agentapi.VMSpecDisk{{SizeGib: 1, StoragePoolPath: s.poolRoot}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(s.srv.URL+"/v1/vms/cold/migrations/incoming", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST incoming: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestIncomingRequestMovesAnAbandonedCopyAside(t *testing.T) {
	s := newAbandonedSeam(t)

	if got := s.postIncoming(t, &[]uuid.UUID{s.old}); got != http.StatusOK {
		t.Fatalf("POST incoming naming the abandoned migration = %d, want 200", got)
	}
	kept := filepath.Join(s.poolRoot, "abandoned", s.vmID.String()+"-"+s.old.String(), "disk.qcow2")
	if got, err := os.ReadFile(kept); err != nil || string(got) != "abandoned copy" {
		t.Errorf("ReadFile(%s) = (%q, %v), want the moved copy", kept, got, err)
	}
}

func TestIncomingRequestWithoutAbandonedIDsMovesNothing(t *testing.T) {
	s := newAbandonedSeam(t)

	if got := s.postIncoming(t, nil); got != http.StatusInternalServerError {
		t.Errorf("POST incoming without abandoned ids = %d, want 500 (already present)", got)
	}
	if got, err := os.ReadFile(s.disk); err != nil || string(got) != "abandoned copy" {
		t.Errorf("ReadFile(%s) = (%q, %v), want the copy untouched", s.disk, got, err)
	}
	if _, err := os.Stat(filepath.Join(s.poolRoot, "abandoned")); !os.IsNotExist(err) {
		t.Errorf("abandoned/: stat err = %v, want none", err)
	}
}
