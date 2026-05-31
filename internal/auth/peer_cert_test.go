// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func TestGeneratePeerCertTemplate(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	dnsNames := []string{"localhost", "node-1.peers.otherix.local"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.10")}

	certPEM, keyPEM, err := GeneratePeerCert(caCert, caKey, dnsNames, ips, PeerCertValidity, now)
	if err != nil {
		t.Fatalf("GeneratePeerCert: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatalf("empty PEM output")
	}

	cert := parseCert(t, certPEM)
	if got, want := cert.Subject.CommonName, "otherix-cp-peer"; got != want {
		t.Errorf("Subject.CommonName = %q, want %q", got, want)
	}
	if got := cert.Subject.Organization; len(got) != 1 || got[0] != "Otherix" {
		t.Errorf("Subject.Organization = %v, want [Otherix]", got)
	}
	if cert.IsCA {
		t.Error("IsCA = true, want false for a peer leaf")
	}

	wantEKU := map[x509.ExtKeyUsage]bool{x509.ExtKeyUsageServerAuth: false, x509.ExtKeyUsageClientAuth: false}
	for _, u := range cert.ExtKeyUsage {
		if _, ok := wantEKU[u]; ok {
			wantEKU[u] = true
		}
	}
	for usage, seen := range wantEKU {
		if !seen {
			t.Errorf("ExtKeyUsage missing %v - a peer cert authenticates both directions of the Raft handshake", usage)
		}
	}

	if cert.SignatureAlgorithm != x509.ECDSAWithSHA384 {
		t.Errorf("SignatureAlgorithm = %v, want ECDSAWithSHA384", cert.SignatureAlgorithm)
	}
	if !cert.NotAfter.Equal(now.UTC().Add(PeerCertValidity)) {
		t.Errorf("NotAfter = %v, want %v", cert.NotAfter, now.UTC().Add(PeerCertValidity))
	}
}

func TestGeneratePeerCertChainsToCA(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	certPEM, _, err := GeneratePeerCert(caCert, caKey, []string{"node-1.peers.otherix.local"}, []net.IP{net.ParseIP("10.0.0.10")}, PeerCertValidity, now)
	if err != nil {
		t.Fatalf("GeneratePeerCert: %v", err)
	}
	cert := parseCert(t, certPEM)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now.Add(time.Hour),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSName:     "node-1.peers.otherix.local",
	}
	if _, err := cert.Verify(opts); err != nil {
		t.Errorf("peer cert does not chain to cluster CA: %v", err)
	}
}

func TestGeneratePeerCertSANDistribution(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	certPEM, _, err := GeneratePeerCert(caCert, caKey,
		[]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.10")},
		PeerCertValidity, now)
	if err != nil {
		t.Fatalf("GeneratePeerCert: %v", err)
	}
	cert := parseCert(t, certPEM)
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "localhost" {
		t.Errorf("DNSNames = %v, want [localhost]", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 2 {
		t.Fatalf("IPAddresses len = %d, want 2", len(cert.IPAddresses))
	}
}

func TestGeneratePeerCertErrors(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Now()

	if _, _, err := GeneratePeerCert(nil, caKey, []string{"localhost"}, nil, PeerCertValidity, now); err == nil {
		t.Error("expected error for nil caCert, got nil")
	}
	if _, _, err := GeneratePeerCert(caCert, nil, []string{"localhost"}, nil, PeerCertValidity, now); err == nil {
		t.Error("expected error for nil caKey, got nil")
	}
	if _, _, err := GeneratePeerCert(caCert, caKey, nil, nil, PeerCertValidity, now); err == nil {
		t.Error("expected error for empty SAN set, got nil")
	}
}
