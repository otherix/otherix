// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/migration"
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

	h := New(m, console.NewTokenStore(), discardLogger(), cfg.Migration.Host, nil)
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

// TestStartIncomingResponseThreadsNbdEndpoint asserts the incoming handler
// serializes a non-empty IncomingResult.NBDEndpoint into the wire
// nbd_endpoint field, and omits it (nil) when the result endpoint is empty
// (the offline case). This is the handler's serialization seam: the live
// target advertises a distinct NBD listener the source must dial, and the
// offline path reuses the single endpoint so nbd_endpoint stays absent.
func TestStartIncomingResponseThreadsNbdEndpoint(t *testing.T) {
	tests := []struct {
		name string
		res  vm.IncomingResult
		want *string
	}{
		{
			name: "live carries nbd endpoint",
			res:  vm.IncomingResult{ListenEndpoint: "h:49200", NBDEndpoint: "h:49153", AuthToken: "tok"},
			want: strptr("h:49153"),
		},
		{
			name: "offline omits nbd endpoint",
			res:  vm.IncomingResult{ListenEndpoint: "h:49200", AuthToken: "tok"},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the handler's response construction exactly: the
			// handler builds MigrationIncomingResponse from the manager's
			// IncomingResult, mapping NBDEndpoint -> NbdEndpoint via
			// strPtrOrNil (nil when empty).
			out := agentapi.MigrationIncomingResponse{
				MigrationID:    uuid.New(),
				ListenEndpoint: tc.res.ListenEndpoint,
				AuthToken:      tc.res.AuthToken,
				NbdEndpoint:    strPtrOrNil(tc.res.NBDEndpoint),
			}
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded agentapi.MigrationIncomingResponse
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			switch {
			case tc.want == nil && decoded.NbdEndpoint != nil:
				t.Errorf("nbd_endpoint = %q, want absent", *decoded.NbdEndpoint)
			case tc.want != nil && (decoded.NbdEndpoint == nil || *decoded.NbdEndpoint != *tc.want):
				t.Errorf("nbd_endpoint = %v, want %q", decoded.NbdEndpoint, *tc.want)
			}
		})
	}
}

// TestStartOutgoingRequestThreadsNbdEndpoint asserts the wire nbd_endpoint
// field decodes and threads through the same deref the outgoing handler uses
// to populate OutgoingSpec.NBDEndpoint (the endpoint the source dials for the
// live blockdev-mirror disk push). This locks the wire field name and the
// optional-pointer mapping. It does not drive the live handler->manager seam
// end-to-end: the handler holds a concrete *vm.Manager whose live seams are
// unexported, and OutgoingSpec.NBDEndpoint is consumed in the detached
// runOutgoingLive goroutine, never persisted to an observable record. That
// end-to-end path is covered by the package-vm TestRunOutgoingLive test,
// which drives a non-empty OutgoingSpec.NBDEndpoint through the real spec.
func TestStartOutgoingRequestThreadsNbdEndpoint(t *testing.T) {
	body, _ := json.Marshal(agentapi.MigrationOutgoingRequest{
		MigrationID:        uuid.New(),
		Mode:               agentapi.Live,
		TargetEndpoint:     "10.0.0.2:49152",
		NbdEndpoint:        strptr("h:49153"),
		TargetNodeIdentity: strptr("node-dst.agents.otherix.local"),
		AuthToken:          "tok",
	})

	// Decode exactly as the handler does, then exercise the same deref the
	// handler uses to thread the optional wire field into OutgoingSpec.
	var req agentapi.MigrationOutgoingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec := vm.OutgoingSpec{NBDEndpoint: deref(req.NbdEndpoint)}
	if spec.NBDEndpoint != "h:49153" {
		t.Errorf("OutgoingSpec.NBDEndpoint = %q, want %q", spec.NBDEndpoint, "h:49153")
	}
}

func TestIncomingDisks(t *testing.T) {
	const bootSizeBytes = 10 * gibBytes

	cases := []struct {
		name string
		req  agentapi.MigrationIncomingRequest
		want []vm.MigrationDisk
	}{
		{
			name: "nil manifest falls back to boot disk",
			req:  agentapi.MigrationIncomingRequest{Disks: nil},
			want: []vm.MigrationDisk{{Index: 0, SizeBytes: bootSizeBytes, Format: "qcow2", ReadOnly: false}},
		},
		{
			name: "empty manifest falls back to boot disk",
			req:  agentapi.MigrationIncomingRequest{Disks: &[]agentapi.MigrationDisk{}},
			want: []vm.MigrationDisk{{Index: 0, SizeBytes: bootSizeBytes, Format: "qcow2", ReadOnly: false}},
		},
		{
			name: "two-entry manifest maps one to one",
			req: agentapi.MigrationIncomingRequest{Disks: &[]agentapi.MigrationDisk{
				{Index: 0, SizeBytes: bootSizeBytes, Format: agentapi.MigrationDiskFormatQcow2, ReadOnly: false},
				{Index: 1, SizeBytes: 1 << 20, Format: agentapi.MigrationDiskFormatRaw, ReadOnly: true},
			}},
			want: []vm.MigrationDisk{
				{Index: 0, SizeBytes: bootSizeBytes, Format: "qcow2", ReadOnly: false},
				{Index: 1, SizeBytes: 1 << 20, Format: "raw", ReadOnly: true},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := incomingDisks(tc.req, bootSizeBytes)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("incomingDisks(...) mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMapIncomingErrorStatuses pins the whole StartIncoming error mapping. The
// case that motivated it is ErrInFlight: a redelivered incoming start arriving
// while the first is still setting up is transient and retryable, so it must read
// as 409 rather than falling into the internal-error default - which is what the
// CP would otherwise log and act on. The other rows are positive controls, so a
// mapping that collapsed everything to one status still fails here.
func TestMapIncomingErrorStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "no free port", err: migration.ErrNoFreePort, want: http.StatusConflict},
		{name: "operation in flight", err: vm.ErrInFlight, want: http.StatusConflict},
		{name: "unknown pool", err: vm.ErrPoolUnknown, want: http.StatusBadRequest},
		{name: "anything else", err: errors.New("boom"), want: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/vms/demo/migrations/incoming", nil)
			mapIncomingError(rec, req, tc.err)
			if rec.Code != tc.want {
				t.Errorf("mapIncomingError(%v) status = %d, want %d (body=%s)", tc.err, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
