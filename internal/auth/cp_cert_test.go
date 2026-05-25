// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// genTestCA mints a fresh cluster CA for tests that need a signing
// key. Mirrors the production GenerateClusterCA path so chains line
// up exactly.
func genTestCA(t *testing.T) (*x509.Certificate, crypto.Signer) {
	t.Helper()
	now := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	res, err := GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	cert, _, err := ParseClusterCACert(res.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	key, err := ParseClusterCAKey(res.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		t.Fatalf("ParseClusterCAKey: not a crypto.Signer (got %T)", key)
	}
	return cert, signer
}

func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("pem.Decode: no block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

func TestGenerateReplicaCert_Template(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	dnsNames := []string{"localhost", "replica-1.cluster.local"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}

	certPEM, keyPEM, err := GenerateReplicaCert(caCert, caKey, dnsNames, ips, CPCertValidity, now)
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatalf("empty PEM output")
	}

	cert := parseCert(t, certPEM)
	if got, want := cert.Subject.CommonName, "otherix-cp-replica"; got != want {
		t.Errorf("Subject.CommonName = %q, want %q", got, want)
	}
	if got := cert.Subject.Organization; len(got) != 1 || got[0] != "Otherix" {
		t.Errorf("Subject.Organization = %v, want [Otherix]", got)
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("KeyUsage missing DigitalSignature")
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Error("KeyUsage missing KeyEncipherment")
	}
	if cert.IsCA {
		t.Error("IsCA = true, want false")
	}

	wantEKU := map[x509.ExtKeyUsage]bool{x509.ExtKeyUsageServerAuth: false, x509.ExtKeyUsageClientAuth: false}
	for _, u := range cert.ExtKeyUsage {
		if _, ok := wantEKU[u]; ok {
			wantEKU[u] = true
		}
	}
	for usage, seen := range wantEKU {
		if !seen {
			t.Errorf("ExtKeyUsage missing %v", usage)
		}
	}

	if cert.SignatureAlgorithm != x509.ECDSAWithSHA384 {
		t.Errorf("SignatureAlgorithm = %v, want ECDSAWithSHA384", cert.SignatureAlgorithm)
	}

	if !cert.NotBefore.Equal(now.UTC().Add(-1 * time.Minute)) {
		t.Errorf("NotBefore = %v, want %v (now-1m)", cert.NotBefore, now.UTC().Add(-1*time.Minute))
	}
	if !cert.NotAfter.Equal(now.UTC().Add(CPCertValidity)) {
		t.Errorf("NotAfter = %v, want %v (now+validity)", cert.NotAfter, now.UTC().Add(CPCertValidity))
	}
}

func TestGenerateReplicaCert_ChainsToCA(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	certPEM, _, err := GenerateReplicaCert(caCert, caKey, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, CPCertValidity, now)
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	cert := parseCert(t, certPEM)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now.Add(time.Hour),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestGenerateReplicaCert_ECDSAP384(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	certPEM, _, err := GenerateReplicaCert(caCert, caKey, []string{"localhost"}, nil, CPCertValidity, now)
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	cert := parseCert(t, certPEM)
	if cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("PublicKeyAlgorithm = %v, want ECDSA", cert.PublicKeyAlgorithm)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("PublicKey type = %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if pub.Curve != elliptic.P384() {
		t.Errorf("Curve = %s, want P-384", pub.Curve.Params().Name)
	}
}

func TestGenerateReplicaCert_SANDistribution(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	dnsNames := []string{"localhost", "replica-1"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5"), net.ParseIP("2001:db8::1")}

	certPEM, _, err := GenerateReplicaCert(caCert, caKey, dnsNames, ips, CPCertValidity, now)
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	cert := parseCert(t, certPEM)

	if len(cert.DNSNames) != 2 || cert.DNSNames[0] != "localhost" || cert.DNSNames[1] != "replica-1" {
		t.Errorf("DNSNames = %v, want [localhost replica-1]", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 3 {
		t.Fatalf("IPAddresses = %v (len %d), want 3", cert.IPAddresses, len(cert.IPAddresses))
	}
	wantIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5"), net.ParseIP("2001:db8::1")}
	for i, want := range wantIPs {
		if !cert.IPAddresses[i].Equal(want) {
			t.Errorf("IPAddresses[%d] = %v, want %v", i, cert.IPAddresses[i], want)
		}
	}
}

func TestGenerateReplicaCert_NoSANs(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_, _, err := GenerateReplicaCert(caCert, caKey, nil, nil, CPCertValidity, now)
	if err == nil {
		t.Fatal("GenerateReplicaCert with empty SANs: expected error, got nil")
	}
}

func TestAutoDetectSANs_ListenAddrParsing(t *testing.T) {
	// Pin hostname to a known value for deterministic baseline.
	prev := hostnameFn
	t.Cleanup(func() { hostnameFn = prev })
	hostnameFn = func() (string, error) { return "test-replica", nil }

	cases := []struct {
		name     string
		listen   string
		extraDNS []string // expected on top of baseline {localhost, test-replica}
		extraIPs []net.IP // expected on top of baseline {127.0.0.1}
	}{
		{"empty", "", nil, nil},
		{"port-only", ":8443", nil, nil},
		{"wildcard ipv4", "0.0.0.0:8443", nil, nil},
		{"wildcard ipv6", "[::]:8443", nil, nil},
		{"ipv4 literal", "10.0.0.1:8443", nil, []net.IP{net.ParseIP("10.0.0.1")}},
		{"dns name", "replica1.cluster:8443", []string{"replica1.cluster"}, nil},
		{"ipv6 literal", "[2001:db8::1]:8443", nil, []net.IP{net.ParseIP("2001:db8::1")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dns, ips := AutoDetectSANs(tc.listen)
			wantDNS := append([]string{"localhost", "test-replica"}, tc.extraDNS...)
			wantIPs := append([]net.IP{net.ParseIP("127.0.0.1")}, tc.extraIPs...)
			if !equalStringSlice(dns, wantDNS) {
				t.Errorf("DNS = %v, want %v", dns, wantDNS)
			}
			if !equalIPSlice(ips, wantIPs) {
				t.Errorf("IPs = %v, want %v", ips, wantIPs)
			}
		})
	}
}

func TestAutoDetectSANs_HostnameEmpty(t *testing.T) {
	prev := hostnameFn
	t.Cleanup(func() { hostnameFn = prev })
	hostnameFn = func() (string, error) { return "", nil }

	dns, ips := AutoDetectSANs(":8443")
	wantDNS := []string{"localhost"}
	wantIPs := []net.IP{net.ParseIP("127.0.0.1")}
	if !equalStringSlice(dns, wantDNS) {
		t.Errorf("DNS = %v, want %v (hostname empty → only localhost)", dns, wantDNS)
	}
	if !equalIPSlice(ips, wantIPs) {
		t.Errorf("IPs = %v, want %v", ips, wantIPs)
	}
}

func TestAutoDetectSANs_HostnameError(t *testing.T) {
	prev := hostnameFn
	t.Cleanup(func() { hostnameFn = prev })
	hostnameFn = func() (string, error) { return "", errors.New("no hostname") }

	dns, _ := AutoDetectSANs(":8443")
	if equalStringSlice(dns, nil) {
		t.Fatal("expected baseline DNS, got nil")
	}
	for _, d := range dns {
		if d == "" {
			t.Error("empty DNS name slipped past hostname error")
		}
	}
}

func TestClassifySANs(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		wantDNS []string
		wantIPs []net.IP
	}{
		{"empty", nil, nil, nil},
		{"only dns", []string{"a.example.com", "b.example.com"}, []string{"a.example.com", "b.example.com"}, nil},
		{"only ips", []string{"1.2.3.4", "::1"}, nil, []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("::1")}},
		{
			"mixed",
			[]string{"cp.example.com", "10.0.0.1", "fdab::1", "alt.example.com"},
			[]string{"cp.example.com", "alt.example.com"},
			[]net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("fdab::1")},
		},
		{"skips empty", []string{"a.example.com", "", "1.2.3.4"}, []string{"a.example.com"}, []net.IP{net.ParseIP("1.2.3.4")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dns, ips := ClassifySANs(tc.input)
			if !equalStringSlice(dns, tc.wantDNS) {
				t.Errorf("DNS = %v, want %v", dns, tc.wantDNS)
			}
			if !equalIPSlice(ips, tc.wantIPs) {
				t.Errorf("IPs = %v, want %v", ips, tc.wantIPs)
			}
		})
	}
}

func TestMergeUniqueSANs(t *testing.T) {
	autoDNS := []string{"localhost", "replica-1"}
	autoIPs := []net.IP{net.ParseIP("127.0.0.1")}
	addDNS := []string{"REPLICA-1", "cp.example.com"}
	addIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}

	dns, ips := MergeUniqueSANs(autoDNS, autoIPs, addDNS, addIPs)

	wantDNS := []string{"localhost", "replica-1", "cp.example.com"}
	if !equalStringSlice(dns, wantDNS) {
		t.Errorf("DNS = %v, want %v (REPLICA-1 case-folded duplicate of replica-1)", dns, wantDNS)
	}
	wantIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}
	if !equalIPSlice(ips, wantIPs) {
		t.Errorf("IPs = %v, want %v", ips, wantIPs)
	}
}

func TestCertCacheValid(t *testing.T) {
	caCert, caKey := genTestCA(t)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	expectedDNS := []string{"localhost", "replica-1"}
	expectedIPs := []net.IP{net.ParseIP("127.0.0.1")}

	freshPEM, _, err := GenerateReplicaCert(caCert, caKey, expectedDNS, expectedIPs, CPCertValidity, now)
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	fresh := parseCert(t, freshPEM)

	t.Run("valid", func(t *testing.T) {
		if err := CertCacheValid(fresh, expectedDNS, expectedIPs, caCert, now.Add(time.Hour)); err != nil {
			t.Errorf("CertCacheValid: %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		futureNow := now.Add(CPCertValidity + time.Hour)
		err := CertCacheValid(fresh, expectedDNS, expectedIPs, caCert, futureNow)
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Errorf("expected expired error, got %v", err)
		}
	})

	t.Run("near-expiry", func(t *testing.T) {
		// Time advance so NotAfter - now is less than buffer (30d).
		nearExpiry := now.Add(CPCertValidity - 15*24*time.Hour)
		err := CertCacheValid(fresh, expectedDNS, expectedIPs, caCert, nearExpiry)
		if err == nil || !strings.Contains(err.Error(), "expiry") {
			t.Errorf("expected near-expiry error, got %v", err)
		}
	})

	t.Run("missing dns", func(t *testing.T) {
		err := CertCacheValid(fresh, []string{"localhost", "replica-1", "new-name"}, expectedIPs, caCert, now.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "DNS SAN") {
			t.Errorf("expected missing DNS error, got %v", err)
		}
	})

	t.Run("missing ip", func(t *testing.T) {
		err := CertCacheValid(fresh, expectedDNS, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}, caCert, now.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "IP SAN") {
			t.Errorf("expected missing IP error, got %v", err)
		}
	})

	t.Run("ca mismatch", func(t *testing.T) {
		otherCA, _ := genTestCA(t)
		err := CertCacheValid(fresh, expectedDNS, expectedIPs, otherCA, now.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "chain") {
			t.Errorf("expected chain error, got %v", err)
		}
	})

	t.Run("nil cert", func(t *testing.T) {
		if err := CertCacheValid(nil, expectedDNS, expectedIPs, caCert, now); err == nil {
			t.Error("expected error on nil cert")
		}
	})

	t.Run("nil ca", func(t *testing.T) {
		if err := CertCacheValid(fresh, expectedDNS, expectedIPs, nil, now); err == nil {
			t.Error("expected error on nil ca")
		}
	})
}

func TestWriteCertCacheAtomic(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "subdir", "cp-cert.crt")
	keyPath := filepath.Join(dir, "subdir", "cp-cert.key")

	if err := WriteCertCacheAtomic(certPath, keyPath, []byte("CERT"), []byte("KEY")); err != nil {
		t.Fatalf("WriteCertCacheAtomic: %v", err)
	}

	// Parent dir created at 0750.
	dirInfo, err := os.Stat(filepath.Dir(certPath))
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o750 {
		t.Errorf("parent dir mode = %o, want 0750", mode)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("Stat cert: %v", err)
	}
	if mode := certInfo.Mode().Perm(); mode != 0o644 {
		t.Errorf("cert mode = %o, want 0644", mode)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat key: %v", err)
	}
	if mode := keyInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("key mode = %o, want 0600", mode)
	}

	certBytes, _ := os.ReadFile(certPath)
	if string(certBytes) != "CERT" {
		t.Errorf("cert content = %q, want CERT", string(certBytes))
	}
	keyBytes, _ := os.ReadFile(keyPath)
	if string(keyBytes) != "KEY" {
		t.Errorf("key content = %q, want KEY", string(keyBytes))
	}

	// Overwrite: subsequent write replaces content.
	if err := WriteCertCacheAtomic(certPath, keyPath, []byte("CERT2"), []byte("KEY2")); err != nil {
		t.Fatalf("second WriteCertCacheAtomic: %v", err)
	}
	certBytes, _ = os.ReadFile(certPath)
	if string(certBytes) != "CERT2" {
		t.Errorf("overwrite failed: cert = %q", string(certBytes))
	}
}

func TestWriteCertCacheAtomic_EmptyInputs(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args [4]string // certPath, keyPath, certPEM, keyPEM
	}{
		{"empty cert path", [4]string{"", filepath.Join(dir, "k"), "CERT", "KEY"}},
		{"empty key path", [4]string{filepath.Join(dir, "c"), "", "CERT", "KEY"}},
		{"empty cert pem", [4]string{filepath.Join(dir, "c"), filepath.Join(dir, "k"), "", "KEY"}},
		{"empty key pem", [4]string{filepath.Join(dir, "c"), filepath.Join(dir, "k"), "CERT", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := WriteCertCacheAtomic(tc.args[0], tc.args[1], []byte(tc.args[2]), []byte(tc.args[3])); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// ---------- helpers ----------

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalIPSlice(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}
