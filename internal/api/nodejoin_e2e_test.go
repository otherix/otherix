// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// generateE2ECSR returns a PEM-encoded CERTIFICATE REQUEST signed by a
// fresh ECDSA P-256 keypair. The Subject CN is set to "node-test" —
// the handler ignores Subject but we set it for realism.
func generateE2ECSR(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-test"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// mintE2EToken creates a join token via the management endpoint and
// returns the plaintext. The mint always succeeds — admin caller via
// the existing loginAs helper has node:manage permission.
func mintE2EToken(t *testing.T, h *e2eHarness, body map[string]any) (plaintext string, id string) {
	t.Helper()
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/nodes/join-tokens", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint token status = %d, want 201", resp.StatusCode)
	}
	var bundle struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeJSON(t, resp, &bundle)
	return bundle.Token, bundle.ID
}

// uniqueNodeName produces test-distinct node names so concurrent
// tests don't collide on the global nodes.uq_nodes_name index.
func uniqueNodeName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// joinBody builds a valid /v1/nodes/join body. Caller can override
// individual fields via keyword args.
func joinBody(token, csr, nodeName string) map[string]any {
	return map[string]any{
		"token":                      token,
		"csr_pem":                    csr,
		"node_name":                  nodeName,
		"architecture":               "arm64",
		"advertised_endpoint":        "https://10.0.0.1:9443",
		"migration_host":             "10.0.0.1",
		"migration_port_range_start": 49152,
		"migration_port_range_end":   49251,
	}
}

func TestE2E_NodeJoin_HappyPath_Anonymous(t *testing.T) {
	h := newE2E(t)
	token, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
	csr := generateE2ECSR(t)
	nodeName := uniqueNodeName("happy")

	resp := h.post(t, "/v1/nodes/join", joinBody(token, csr, nodeName), "") // anonymous
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		NodeID    string `json:"node_id"`
		CertPEM   string `json:"cert_pem"`
		CACertPEM string `json:"ca_cert_pem"`
	}
	decodeJSON(t, resp, &body)

	if _, err := uuid.Parse(body.NodeID); err != nil {
		t.Errorf("node_id not a UUID: %v", err)
	}
	if !strings.Contains(body.CertPEM, "BEGIN CERTIFICATE") {
		t.Error("cert_pem missing BEGIN CERTIFICATE")
	}
	if !strings.Contains(body.CACertPEM, "BEGIN CERTIFICATE") {
		t.Error("ca_cert_pem missing BEGIN CERTIFICATE")
	}

	// Verify chain.
	caCert, _, err := auth.ParseClusterCACert([]byte(body.CACertPEM))
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	block, _ := pem.Decode([]byte(body.CertPEM))
	if block == nil {
		t.Fatal("decode issued cert PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("leaf cert chain verification failed: %v", err)
	}
	wantCN := "node-" + nodeName
	if leaf.Subject.CommonName != wantCN {
		t.Errorf("issued CN = %q, want %q", leaf.Subject.CommonName, wantCN)
	}

	// Database state — agent_cert row + consumption audit.
	ctx := context.Background()
	if _, err := h.store.Queries().GetActiveCACert(ctx); err != nil {
		t.Fatalf("active CA missing: %v", err)
	}
	var fpRow struct {
		NodeID    uuid.UUID
		RevokedAt pgtype.Timestamptz
	}
	if err := h.store.Pool().QueryRow(ctx,
		`select node_id, revoked_at from agent_certs where node_id = $1`,
		body.NodeID).Scan(&fpRow.NodeID, &fpRow.RevokedAt); err != nil {
		t.Errorf("agent_certs row missing: %v", err)
	}
	if fpRow.RevokedAt.Valid {
		t.Error("agent_certs row is revoked on insert")
	}
	var consumptionCount int
	if err := h.store.Pool().QueryRow(ctx,
		`select count(*) from join_token_consumptions where consumed_by_node_id = $1`,
		body.NodeID).Scan(&consumptionCount); err != nil {
		t.Errorf("consumption row missing: %v", err)
	}
	if consumptionCount != 1 {
		t.Errorf("consumption count = %d, want 1", consumptionCount)
	}
}

func TestE2E_NodeJoin_UnknownToken_401(t *testing.T) {
	h := newE2E(t)
	csr := generateE2ECSR(t)
	resp := h.post(t, "/v1/nodes/join",
		joinBody("otx_join_aaaaaaaaaaaaaaaa", csr, uniqueNodeName("nope")), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeUnauthenticated {
		t.Errorf("code = %q, want unauthenticated", body.Error.Code)
	}
}

func TestE2E_NodeJoin_ExpiredToken_401(t *testing.T) {
	h := newE2E(t)
	// Mint a token then expire it via direct UPDATE — bypasses TTL
	// minimum (60s) and avoids a time.Sleep in the test path.
	token, id := mintE2EToken(t, h, map[string]any{"ttl_seconds": 60})
	if _, err := h.store.Pool().Exec(context.Background(),
		`update join_tokens set expires_at = now() - interval '1 hour' where id = $1`, id); err != nil {
		t.Fatalf("expire token: %v", err)
	}

	csr := generateE2ECSR(t)
	resp := h.post(t, "/v1/nodes/join", joinBody(token, csr, uniqueNodeName("exp")), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestE2E_NodeJoin_PreboundMismatch_400(t *testing.T) {
	h := newE2E(t)
	expectedName := uniqueNodeName("bound")
	token, _ := mintE2EToken(t, h, map[string]any{
		"ttl_seconds":        600,
		"intended_node_name": expectedName,
		"max_uses":           1,
	})
	csr := generateE2ECSR(t)

	resp := h.post(t, "/v1/nodes/join",
		joinBody(token, csr, uniqueNodeName("wrong")), "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want validation_failed", body.Error.Code)
	}
}

func TestE2E_NodeJoin_PreboundMatch_201(t *testing.T) {
	h := newE2E(t)
	nodeName := uniqueNodeName("match")
	token, _ := mintE2EToken(t, h, map[string]any{
		"ttl_seconds":        600,
		"intended_node_name": nodeName,
		"max_uses":           1,
	})
	csr := generateE2ECSR(t)

	resp := h.post(t, "/v1/nodes/join", joinBody(token, csr, nodeName), "")
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestE2E_NodeJoin_ExistingActiveCert_409(t *testing.T) {
	h := newE2E(t)
	nodeName := uniqueNodeName("dup")

	// First redemption — succeeds.
	token1, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
	csr := generateE2ECSR(t)
	resp1 := h.post(t, "/v1/nodes/join", joinBody(token1, csr, nodeName), "")
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first redemption status = %d, want 201", resp1.StatusCode)
	}

	// Second redemption — same node_name with a fresh token. Active
	// cert from first redemption blocks it.
	token2, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
	csr2 := generateE2ECSR(t)
	resp2 := h.post(t, "/v1/nodes/join", joinBody(token2, csr2, nodeName), "")
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp2.StatusCode)
	}
}

func TestE2E_NodeJoin_RevokedCert_ReuseRow_201(t *testing.T) {
	h := newE2E(t)
	nodeName := uniqueNodeName("revoke")

	// Bootstrap once.
	token1, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
	csr := generateE2ECSR(t)
	resp1 := h.post(t, "/v1/nodes/join", joinBody(token1, csr, nodeName), "")
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first redemption status = %d", resp1.StatusCode)
	}
	var bundle1 struct {
		NodeID string `json:"node_id"`
	}
	decodeJSON(t, resp1, &bundle1)

	// Revoke all existing certs for this node.
	if _, err := h.store.Pool().Exec(context.Background(),
		`update agent_certs set revoked_at = now() where node_id = $1`, bundle1.NodeID); err != nil {
		t.Fatalf("revoke cert: %v", err)
	}

	// Re-bootstrap — node row should be reused, fresh cert issued.
	token2, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
	csr2 := generateE2ECSR(t)
	resp2 := h.post(t, "/v1/nodes/join", joinBody(token2, csr2, nodeName), "")
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("second redemption status = %d, want 201", resp2.StatusCode)
	}
	var bundle2 struct {
		NodeID string `json:"node_id"`
	}
	decodeJSON(t, resp2, &bundle2)
	if bundle1.NodeID != bundle2.NodeID {
		t.Errorf("node_id changed: %s vs %s — row should have been reused", bundle1.NodeID, bundle2.NodeID)
	}
}

func TestE2E_NodeJoin_MalformedCSR_400(t *testing.T) {
	h := newE2E(t)
	token, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})

	resp := h.post(t, "/v1/nodes/join",
		joinBody(token, "not a pem block", uniqueNodeName("mal")), "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2E_NodeJoin_MissingFields_400(t *testing.T) {
	h := newE2E(t)
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"no_token", func(b map[string]any) { delete(b, "token") }},
		{"no_csr", func(b map[string]any) { delete(b, "csr_pem") }},
		{"no_node_name", func(b map[string]any) { delete(b, "node_name") }},
		{"no_architecture", func(b map[string]any) { delete(b, "architecture") }},
		{"bad_architecture", func(b map[string]any) { b["architecture"] = "ppc64" }},
		{"no_advertised_endpoint", func(b map[string]any) { delete(b, "advertised_endpoint") }},
		{"no_migration_host", func(b map[string]any) { delete(b, "migration_host") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
			body := joinBody(token, generateE2ECSR(t), uniqueNodeName("miss"))
			c.mutate(body)
			resp := h.post(t, "/v1/nodes/join", body, "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestE2E_NodeJoin_MultiUse_WithinCap(t *testing.T) {
	h := newE2E(t)
	token, _ := mintE2EToken(t, h, map[string]any{
		"ttl_seconds": 600,
		"max_uses":    3,
	})

	for i := 0; i < 3; i++ {
		csr := generateE2ECSR(t)
		resp := h.post(t, "/v1/nodes/join",
			joinBody(token, csr, uniqueNodeName("multi")), "")
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("redemption %d status = %d, want 201", i+1, resp.StatusCode)
		}
	}
}

func TestE2E_NodeJoin_MultiUse_Exhausted_401(t *testing.T) {
	h := newE2E(t)
	token, _ := mintE2EToken(t, h, map[string]any{
		"ttl_seconds": 600,
		"max_uses":    2,
	})

	for i := 0; i < 2; i++ {
		csr := generateE2ECSR(t)
		resp := h.post(t, "/v1/nodes/join",
			joinBody(token, csr, uniqueNodeName("exh")), "")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("warmup %d: status = %d, want 201", i+1, resp.StatusCode)
		}
	}

	// 3rd redemption — capped.
	csr := generateE2ECSR(t)
	resp := h.post(t, "/v1/nodes/join",
		joinBody(token, csr, uniqueNodeName("exh")), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestE2E_NodeJoin_Race_ConcurrentRedemptions(t *testing.T) {
	h := newE2E(t)
	const concurrency = 10
	const cap = 5
	token, _ := mintE2EToken(t, h, map[string]any{
		"ttl_seconds": 600,
		"max_uses":    cap,
	})

	var (
		successes int32
		wg        sync.WaitGroup
	)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			csr := generateE2ECSR(t)
			resp := h.post(t, "/v1/nodes/join",
				joinBody(token, csr, uniqueNodeName("race")), "")
			if resp.StatusCode == http.StatusCreated {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()

	if successes != cap {
		t.Errorf("successes = %d, want %d (max_uses enforced under race)", successes, cap)
	}

	// Audit count matches successes.
	var auditCount int
	if err := h.store.Pool().QueryRow(context.Background(),
		`select count(*) from join_token_consumptions
		 where join_token_id in (select id from join_tokens where token_hash = $1)`,
		auth.HashToken(token)).Scan(&auditCount); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if int32(auditCount) != successes {
		t.Errorf("audit count = %d, want %d", auditCount, successes)
	}
}

func TestE2E_NodeJoin_NodePending_OnFreshCreate(t *testing.T) {
	h := newE2E(t)
	token, _ := mintE2EToken(t, h, map[string]any{"ttl_seconds": 600})
	csr := generateE2ECSR(t)
	nodeName := uniqueNodeName("pending")

	resp := h.post(t, "/v1/nodes/join", joinBody(token, csr, nodeName), "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		NodeID string `json:"node_id"`
	}
	decodeJSON(t, resp, &body)

	var status string
	if err := h.store.Pool().QueryRow(context.Background(),
		`select status from nodes where id = $1`, body.NodeID).Scan(&status); err != nil {
		t.Fatalf("status lookup: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending (heartbeat reconciler flips to ready)", status)
	}

	// Brief to keep linter happy + ensure DB has settled.
	time.Sleep(10 * time.Millisecond)
}
