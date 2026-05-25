// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// TestE2E_Heartbeat_Success — full happy-path: mock pushes a
// heartbeat through mTLS to the CP's agent listener, the receiver
// projects the report into nodes / node_firmwares.
func TestE2E_Heartbeat_Success(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	fwID := seedFirmware(t, h, "OVMF_CODE", store.CpuArchAmd64, store.FirmwareTypeUefi)

	mock := startMockAgentForCP(t, h, node, "amd64")
	mock.AddFirmware(agentmock.Firmware{
		Name:         "OVMF_CODE",
		Architecture: "amd64",
		Type:         "uefi",
		CodePath:     "/usr/share/OVMF/OVMF_CODE.fd",
	})

	status, err := mock.SendHeartbeatNow(ctx)
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	got, err := h.store.Queries().GetNodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if got.LastHeartbeatAt == nil {
		t.Errorf("LastHeartbeatAt = nil after heartbeat, want set")
	}
	if got.CPUModel == nil {
		t.Errorf("CpuModel = nil after heartbeat, want populated")
	}

	listed, err := h.store.Queries().ListNodeFirmwares(ctx, store.ListNodeFirmwaresParams{
		NodeID: node.ID, LimitCount: 50,
	})
	if err != nil {
		t.Fatalf("ListNodeFirmwares: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(node_firmwares) = %d, want 1", len(listed))
	}
	if listed[0].Firmware.ID != fwID {
		t.Errorf("firmware id = %v, want %v", listed[0].Firmware.ID, fwID)
	}
}

// TestE2E_Heartbeat_UnregisteredFingerprint — chain validates but no
// agent_certs row → 401 cert_san_unknown.
func TestE2E_Heartbeat_UnregisteredFingerprint(t *testing.T) {
	h := newCPAgentHarness(t)
	node := seedNodeOnly(t, h, store.CpuArchAmd64, store.NodeStatusReady)
	mock := startMockAgentForCP(t, h, node, "amd64")

	status, err := mock.SendHeartbeatNow(context.Background())
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// TestE2E_Heartbeat_NodeURLMismatch — cert bound to node A but URL
// targets node B → 401 cert_san_unknown.
func TestE2E_Heartbeat_NodeURLMismatch(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	_ = seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	otherNode := seedNodeOnly(t, h, store.CpuArchAmd64, store.NodeStatusReady)

	resp, err := postHeartbeatWithMockTLS(ctx, h, otherNode.Name, minimalHeartbeatBody("amd64"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body := decodeError(t, resp)
	if got, _ := body.Error.Details["reason"].(string); got != "cert_san_unknown" {
		t.Errorf("reason = %q, want cert_san_unknown", got)
	}
}

// TestE2E_Heartbeat_ArchitectureMismatch — node arm64, mock amd64 →
// 409 architecture_mismatch.
func TestE2E_Heartbeat_ArchitectureMismatch(t *testing.T) {
	h := newCPAgentHarness(t)
	node := seedNodeWithCert(t, h, store.CpuArchArm64, store.NodeStatusReady, false)
	mock := startMockAgentForCP(t, h, node, "amd64")

	status, err := mock.SendHeartbeatNow(context.Background())
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
}

// TestE2E_Heartbeat_NodeGone — node status='gone' → 409 node_gone.
func TestE2E_Heartbeat_NodeGone(t *testing.T) {
	h := newCPAgentHarness(t)
	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusGone, false)
	mock := startMockAgentForCP(t, h, node, "amd64")

	status, err := mock.SendHeartbeatNow(context.Background())
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
}

// TestE2E_Heartbeat_RevokedCert — agent_certs.revoked_at set → 401
// cert_revoked.
func TestE2E_Heartbeat_RevokedCert(t *testing.T) {
	h := newCPAgentHarness(t)
	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, true)
	mock := startMockAgentForCP(t, h, node, "amd64")

	status, err := mock.SendHeartbeatNow(context.Background())
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// TestE2E_Heartbeat_UnmatchedFirmware — firmware in heartbeat without
// a matching catalogue row → 200 + node_firmwares stays empty.
func TestE2E_Heartbeat_UnmatchedFirmware(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	mock := startMockAgentForCP(t, h, node, "amd64")
	mock.AddFirmware(agentmock.Firmware{
		Name:         "GhostFirmware",
		Architecture: "amd64",
		Type:         "uefi",
		CodePath:     "/no/such/path",
	})

	status, err := mock.SendHeartbeatNow(ctx)
	if err != nil {
		t.Fatalf("SendHeartbeatNow: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}

	listed, err := h.store.Queries().ListNodeFirmwares(ctx, store.ListNodeFirmwaresParams{
		NodeID: node.ID, LimitCount: 50,
	})
	if err != nil {
		t.Fatalf("ListNodeFirmwares: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("len(node_firmwares) = %d, want 0 (unmatched firmware skipped)", len(listed))
	}
}

// TestE2E_Heartbeat_MigrationCapabilityChanged — change mock's
// migration capability between heartbeats; CP picks up the new
// triple in nodes.migration_*.
func TestE2E_Heartbeat_MigrationCapabilityChanged(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	mock := startMockAgentForCP(t, h, node, "amd64")

	if status, err := mock.SendHeartbeatNow(ctx); err != nil || status != http.StatusOK {
		t.Fatalf("first heartbeat: status=%d err=%v", status, err)
	}

	mock.SetMigrationCapability(agentmock.MigrationCapability{
		Host:           "10.99.99.1",
		PortRangeStart: 50000,
		PortRangeEnd:   50099,
	})
	if status, err := mock.SendHeartbeatNow(ctx); err != nil || status != http.StatusOK {
		t.Fatalf("second heartbeat: status=%d err=%v", status, err)
	}

	got, err := h.store.Queries().GetNodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if got.MigrationHost != "10.99.99.1" {
		t.Errorf("MigrationHost = %q, want 10.99.99.1", got.MigrationHost)
	}
	if got.MigrationPortRangeStart != 50000 || got.MigrationPortRangeEnd != 50099 {
		t.Errorf("port range = [%d, %d], want [50000, 50099]",
			got.MigrationPortRangeStart, got.MigrationPortRangeEnd)
	}
}

// --- helpers ---

// seededNode bundles the (id, name) pair returned from seedNodeOnly so
// callers can both seed agent_certs (by ID) and target the heartbeat
// path (by name) without re-querying the DB.
type seededNode struct {
	ID   uuid.UUID
	Name string
}

func seedNodeOnly(t *testing.T, h *cpAgentHarness, arch store.CPUArch, status store.NodeStatus) seededNode {
	t.Helper()
	id := uuid.New()
	name := fmt.Sprintf("e2e-node-%s", uuid.NewString())
	if _, err := h.store.Queries().CreateNode(context.Background(), store.CreateNodeParams{
		ID:                      id,
		Name:                    name,
		Architecture:            arch,
		AdvertisedEndpoint:      "https://" + name + ".otherix.local:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  status,
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	return seededNode{ID: id, Name: name}
}

func seedNodeWithCert(t *testing.T, h *cpAgentHarness, arch store.CPUArch, status store.NodeStatus, revoked bool) seededNode {
	t.Helper()
	node := seedNodeOnly(t, h, arch, status)
	fp, err := agentmock.NodeCertFingerprint()
	if err != nil {
		t.Fatalf("NodeCertFingerprint: %v", err)
	}
	insertAgentCert(t, h, node.ID, fp, revoked)
	return node
}

// insertAgentCert seeds (or rebinds) an agent_certs row directly via
// the pool. There is no sqlc query for inserting agent_certs yet —
// cert issuance is a future slice (Manual node-registration via
// mTLS in ROADMAP). The fingerprint comes from a single shared mock
// cert across the whole package, so the helper rebinds the row to
// the requesting test's node id rather than failing on the
// fingerprint unique index.
func insertAgentCert(t *testing.T, h *cpAgentHarness, nodeID uuid.UUID, fingerprint []byte, revoked bool) {
	t.Helper()
	now := time.Now().UTC()
	q := `insert into agent_certs (
            id, node_id, serial, fingerprint_sha256, subject_dn,
            not_before, not_after, issued_at, revoked_at
        ) values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        on conflict (fingerprint_sha256) do update
        set node_id    = excluded.node_id,
            serial     = excluded.serial,
            revoked_at = excluded.revoked_at`
	var revokedAt any
	if revoked {
		revokedAt = now
	}
	if _, err := h.store.Pool().Exec(context.Background(), q,
		uuid.New(), nodeID,
		[]byte("e2e-serial-"+uuid.NewString()),
		fingerprint,
		"CN=node-mock-1.agents.otherix.local",
		now.Add(-time.Hour), now.Add(365*24*time.Hour), now, revokedAt,
	); err != nil {
		t.Fatalf("insert agent_certs: %v", err)
	}
}

func seedFirmware(t *testing.T, h *cpAgentHarness, name string, arch store.CPUArch, fwType store.FirmwareType) uuid.UUID {
	t.Helper()
	row, err := h.store.Queries().CreateFirmware(context.Background(), store.CreateFirmwareParams{
		ID:           uuid.New(),
		Name:         name,
		Architecture: arch,
		Type:         fwType,
		IsDefault:    false,
	})
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	return row.ID
}

func startMockAgentForCP(t *testing.T, h *cpAgentHarness, node seededNode, arch string) *agentmock.Mock {
	t.Helper()
	m := agentmock.Start(t, agentmock.Options{
		NodeID:          node.ID,
		NodeName:        node.Name,
		Architecture:    arch,
		ControlPlaneURL: h.agentSrv.URL,
		TLS:             agentmock.TLSEnabled,
	})
	mustSet := func(path string, v any) {
		if err := m.SetCapability(path, v); err != nil {
			t.Fatalf("SetCapability(%s): %v", path, err)
		}
	}
	mustSet("cpu_model", "test-cpu")
	mustSet("cpu_cores_total", 8)
	mustSet("cpu_cores_available", 8)
	mustSet("memory_total_mib", int64(8192))
	mustSet("memory_available_mib", int64(8000))
	mustSet("kernel_version", "6.6.0")
	mustSet("qemu_version", "9.0.0")
	return m
}

// postHeartbeatWithMockTLS posts a body to the harness's agent
// listener under the given node name, using a TLS client carrying
// mock-agent's node.crt as its identity. Used by tests that bypass
// the mock's built-in heartbeat-loop URL targeting (URL/cert
// mismatch case). Path is name-based.
func postHeartbeatWithMockTLS(ctx context.Context, h *cpAgentHarness, nodeName string, body []byte) (*http.Response, error) {
	cli, err := mockTLSClient()
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(h.agentSrv.URL, "/") + "/v1/nodes/" + nodeName + "/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return cli.Do(req)
}

func mockTLSClient() (*http.Client, error) {
	caPEM, err := agentmock.CACertPEM()
	if err != nil {
		return nil, err
	}
	nodePEM, err := agentmock.NodeCertPEM()
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile("../../internal/agentmock/testdata/certs/node.key")
	if err != nil {
		return nil, fmt.Errorf("read node.key: %v", err)
	}
	pair, err := tls.X509KeyPair(nodePEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ca pool empty")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{pair},
				RootCAs:      pool,
				MinVersion:   tls.VersionTLS12,
			},
		},
	}, nil
}

func minimalHeartbeatBody(arch string) []byte {
	body := map[string]any{
		"agent_version": "mock-0.1.0",
		"architecture":  arch,
		"capabilities": map[string]any{
			"cpu_model":        "test-cpu",
			"cpu_flags":        []string{},
			"cpu_cores_total":  4,
			"memory_total_mib": int64(4096),
			"kernel_version":   "6.6.0",
			"qemu_version":     "9.0.0",
			"kvm_available":    true,
			"nested_virt":      false,
			"qemu_binaries":    map[string]string{},
			"firmwares":        []any{},
		},
		"resources": map[string]any{
			"cpu_cores_available":  4,
			"memory_available_mib": int64(4000),
		},
		"vms": []any{},
	}
	out, _ := json.Marshal(body)
	return out
}

func decodeError(t *testing.T, resp *http.Response) response.ErrorBody {
	t.Helper()
	var body response.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return body
}
