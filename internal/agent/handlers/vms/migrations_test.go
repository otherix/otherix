// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/config"
)

// testPoolRoot is the absolute filesystem path the seeded pool is
// registered at; minimalVMSpec points its boot disk's storage_pool_path
// at this value so the handler resolves it back to the pool name.
var testPoolRoot string

// newTestVMsServer builds the real vms.Handler over a real vm.Manager
// (with the migration port range configured and a single pool seeded)
// mounted under /v1/vms on a chi router. The migration seams stay the
// production qemu funcs; the handler tests here exercise only the decode /
// validation / 404 routes that never reach qemu (the happy path is covered
// by the package-vm manager test and the Lima smoke).
func newTestVMsServer(t *testing.T) *httptest.Server {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	testPoolRoot = filepath.Join(tmp, "pool")

	cfg := &config.AgentConfig{StatePath: stateDir}
	cfg.Migration.Host = "127.0.0.1"
	cfg.Migration.PortRangeStart = 49152
	cfg.Migration.PortRangeEnd = 49160

	m, err := vm.New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("vm.New: %v", err)
	}
	if err := m.AddPool("default", testPoolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	h := New(m, console.NewTokenStore(), discardLogger(), cfg.Migration.Host)
	r := chi.NewRouter()
	r.Route("/v1/vms", h.Mount)
	return httptest.NewServer(r)
}

// minimalVMSpec returns a VMSpec whose boot disk (position 0) carries a
// non-zero size_gib and a storage_pool_path pointing at the seeded pool,
// the minimum the incoming handler needs to size the destination disk
// and resolve the pool name.
func minimalVMSpec(t *testing.T) agentapi.VMSpec {
	t.Helper()
	return agentapi.VMSpec{
		VMUUID:       uuid.New(),
		Name:         "demo",
		CPUCores:     1,
		MemoryMib:    512,
		Architecture: "amd64",
		Disks: []agentapi.VMSpecDisk{
			{
				SizeGib:         10,
				StoragePoolPath: testPoolRoot,
			},
		},
	}
}

// The StartIncoming 200 happy path exec's a real qemu-nbd and now waits for it
// to listen (WaitNBDListening); a unit test cannot stand up a working TLS NBD
// server without real node PKI, so that path is covered by the package-vm
// manager test (TestStartIncomingReservesPortAndReturnsEndpoint, fake seams)
// and the two-node Lima smoke. The handler tests below cover its decode /
// validation / error-mapping / routing, which is the handler's own logic.

// TestStartIncomingHandlerRejectsBadBody asserts a malformed JSON body
// is rejected at the decode edge with 400 before any qemu work.
func TestStartIncomingHandlerRejectsBadBody(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/vms/demo/migrations/incoming",
		"application/json", bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestStartIncomingHandlerRejectsMissingSourceIdentity asserts the
// validation edge fires for an absent source_node_identity (the target
// must pin the connecting source before opening the NBD channel).
func TestStartIncomingHandlerRejectsMissingSourceIdentity(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	body, _ := json.Marshal(agentapi.MigrationIncomingRequest{
		MigrationID: uuid.New(),
		Mode:        agentapi.MigrationIncomingRequestModeOffline,
		VMSpec:      minimalVMSpec(t),
	})
	resp, err := http.Post(srv.URL+"/v1/vms/demo/migrations/incoming",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestStartIncomingHandlerRejectsMissingBootDisk asserts an empty Disks
// slice is rejected (no boot disk to size the destination from).
func TestStartIncomingHandlerRejectsMissingBootDisk(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	spec := minimalVMSpec(t)
	spec.Disks = nil
	body, _ := json.Marshal(agentapi.MigrationIncomingRequest{
		MigrationID:        uuid.New(),
		Mode:               agentapi.MigrationIncomingRequestModeOffline,
		SourceNodeIdentity: strptr("CN=node-src"),
		VMSpec:             spec,
	})
	resp, err := http.Post(srv.URL+"/v1/vms/demo/migrations/incoming",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestStartOutgoingHandlerUnknownVM asserts outgoing on a VM that does
// not exist on this node returns 404 - this path never reaches qemu.
func TestStartOutgoingHandlerUnknownVM(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	body, _ := json.Marshal(agentapi.MigrationOutgoingRequest{
		MigrationID:        uuid.New(),
		Mode:               agentapi.Offline,
		TargetEndpoint:     "10.0.0.2:49152",
		TargetNodeIdentity: strptr("node-dst.agents.otherix.local"),
		AuthToken:          "tok",
	})
	resp, err := http.Post(srv.URL+"/v1/vms/ghost/migrations/outgoing",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestStartOutgoingHandlerMissingTargetIdentity asserts an absent
// target_node_identity is rejected at the validation edge with 400.
func TestStartOutgoingHandlerMissingTargetIdentity(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	body, _ := json.Marshal(agentapi.MigrationOutgoingRequest{
		MigrationID:    uuid.New(),
		Mode:           agentapi.Offline,
		TargetEndpoint: "10.0.0.2:49152",
		AuthToken:      "tok",
	})
	resp, err := http.Post(srv.URL+"/v1/vms/demo/migrations/outgoing",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestGetMigrationHandlerBadUUID asserts a non-UUID migration_id path
// segment returns 404.
func TestGetMigrationHandlerBadUUID(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/vms/demo/migrations/not-a-uuid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestGetMigrationHandlerUnknown asserts a well-formed but unknown
// migration_id returns 404.
func TestGetMigrationHandlerUnknown(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/vms/demo/migrations/" + uuid.New().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestCancelMigrationHandlerUnknown asserts cancel of an unknown
// migration returns 404.
func TestCancelMigrationHandlerUnknown(t *testing.T) {
	srv := newTestVMsServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/vms/demo/migrations/"+uuid.New().String()+"/cancel",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func strptr(s string) *string { return &s }
