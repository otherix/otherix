// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// mintToken creates a join token of the given kind via the admin mint endpoint
// and returns the plaintext.
func mintToken(t *testing.T, h *harness, adminTok, kind string) string {
	t.Helper()
	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{"kind": kind}, adminTok)
	if resp.StatusCode != 201 {
		t.Fatalf("mint %s token status = %d, want 201", kind, resp.StatusCode)
	}
	var body struct {
		Kind  string `json:"kind"`
		Token string `json:"token"`
	}
	decodeJSON(t, resp, &body)
	if body.Kind != kind {
		t.Fatalf("minted kind = %q, want %q", body.Kind, kind)
	}
	if body.Token == "" {
		t.Fatal("mint returned empty token")
	}
	return body.Token
}

func TestClusterJoinReturnsCAKeyPair(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)

	token := mintToken(t, h, adminTok, "cluster")

	resp := h.postAgent(t, "/v1/cluster/join", map[string]string{"token": token})
	if resp.StatusCode != 201 {
		t.Fatalf("/v1/cluster/join status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		CACertPEM string `json:"ca_cert_pem"`
		CAKeyPEM  string `json:"ca_key_pem"`
	}
	decodeJSON(t, resp, &body)
	if !strings.Contains(body.CACertPEM, "BEGIN CERTIFICATE") {
		t.Errorf("ca_cert_pem is not PEM: %q", body.CACertPEM)
	}
	if !strings.Contains(body.CAKeyPEM, "BEGIN PRIVATE KEY") {
		t.Errorf("ca_key_pem is not a PKCS#8 PEM key: %q", body.CAKeyPEM)
	}
}

func TestClusterJoinRejectsNodeToken(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)

	// A node-kind token must not redeem at the cluster endpoint.
	nodeToken := mintToken(t, h, adminTok, "node")
	resp := h.postAgent(t, "/v1/cluster/join", map[string]string{"token": nodeToken})
	if resp.StatusCode != 401 {
		t.Fatalf("node token at /v1/cluster/join status = %d, want 401", resp.StatusCode)
	}
}

func TestClusterJoinRejectsGarbageToken(t *testing.T) {
	h := newE2E(t)
	resp := h.postAgent(t, "/v1/cluster/join", map[string]string{"token": "not-a-join-token"})
	if resp.StatusCode != 400 {
		t.Fatalf("garbage token status = %d, want 400", resp.StatusCode)
	}
}
