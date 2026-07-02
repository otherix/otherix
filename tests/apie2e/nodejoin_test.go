// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// TestJoinIPAdvertisedEndpointInCertSAN drives the real join sequence (chi
// router -> nodejoin handler -> redeem -> auth.SignCSR -> etcdstore) and proves
// the IP-SAN wiring end to end: a node advertising an IP endpoint must yield
// a serving cert whose SAN IPAddresses include that IP. The seam has teeth -
// reverting redeem.go to pass "" for advertised_endpoint makes the IP
// assertion fail.
func TestJoinIPAdvertisedEndpointInCertSAN(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)
	tok := mintToken(t, h, adminTok, "node")

	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-384 key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-ip"},
	}, priv)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	resp := h.post(t, "/v1/nodes/join", map[string]any{
		"token":                      tok,
		"csr_pem":                    string(csrPEM),
		"node_name":                  "node-ip",
		"architecture":               "amd64",
		"advertised_endpoint":        "https://10.77.0.5:9443",
		"migration_host":             "10.77.0.5",
		"migration_port_range_start": 49152,
		"migration_port_range_end":   49251,
	}, "")
	if resp.StatusCode != 201 {
		t.Fatalf("join status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		NodeID    string `json:"node_id"`
		CertPEM   string `json:"cert_pem"`
		CACertPEM string `json:"ca_cert_pem"`
	}
	decodeJSON(t, resp, &body)

	block, _ := pem.Decode([]byte(body.CertPEM))
	if block == nil {
		t.Fatalf("cert_pem is not a PEM block: %q", body.CertPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}

	wantIP := net.ParseIP("10.77.0.5")
	foundIP := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(wantIP) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("issued cert IPAddresses = %v, want 10.77.0.5", cert.IPAddresses)
	}

	foundDNS := false
	for _, d := range cert.DNSNames {
		if d == "node-node-ip.agents.otherix.local" {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Errorf("issued cert DNSNames = %v, missing fixed FQDN node-node-ip.agents.otherix.local", cert.DNSNames)
	}
}

// TestJoinGatewayNodeCertUnifiedIdentity drives the real join sequence for a
// gateway-role join token and proves the unified cert identity: the issued leaf
// carries the ordinary node-<name> Subject CN (not a gateway- CN) and its SAN
// covers BOTH the control host and the ingress host, so the CLI ingress dial can
// pin ServerName to the ingress host and still cluster-CA-verify.
func TestJoinGatewayNodeCertUnifiedIdentity(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)
	tok := mintToken(t, h, adminTok, "gateway")

	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-384 key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-gw"},
	}, priv)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	resp := h.post(t, "/v1/nodes/join", map[string]any{
		"token":                       tok,
		"csr_pem":                     string(csrPEM),
		"node_name":                   "gw",
		"architecture":                "amd64",
		"advertised_endpoint":         "https://gw-ctl.example:9443",
		"ingress_advertised_endpoint": "https://gw-ingress.example:9444",
		"migration_host":              "10.77.0.6",
		"migration_port_range_start":  49152,
		"migration_port_range_end":    49251,
	}, "")
	if resp.StatusCode != 201 {
		t.Fatalf("join status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		CertPEM string `json:"cert_pem"`
	}
	decodeJSON(t, resp, &body)

	block, _ := pem.Decode([]byte(body.CertPEM))
	if block == nil {
		t.Fatalf("cert_pem is not a PEM block: %q", body.CertPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}

	if cert.Subject.CommonName != "node-gw" {
		t.Errorf("issued cert CN = %q, want node-gw", cert.Subject.CommonName)
	}

	hasDNS := func(want string) bool {
		for _, d := range cert.DNSNames {
			if d == want {
				return true
			}
		}
		return false
	}
	if !hasDNS("gw-ingress.example") {
		t.Errorf("issued cert DNSNames = %v, missing ingress host gw-ingress.example", cert.DNSNames)
	}
	if !hasDNS("gw-ctl.example") {
		t.Errorf("issued cert DNSNames = %v, missing control host gw-ctl.example", cert.DNSNames)
	}
}
