// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CPCertValidity is the lifetime of a freshly generated per-replica
// CP server cert. 1 year matches LeafCertValidity (Step 2 agent
// certs) — cert rotation lifecycle (manual + near-expiry) tracked in
// ROADMAP Planned, deferred to a follow-up iteration.
const CPCertValidity = 365 * 24 * time.Hour

// CPCertNearExpiryBuffer is the window before NotAfter at which a
// cached cert is considered stale enough to regenerate. 30 days lets
// operators schedule replica restarts without surprise TLS handshake
// failures under steady-state.
const CPCertNearExpiryBuffer = 30 * 24 * time.Hour

// hostnameFn is the indirection point for os.Hostname so tests can
// inject deterministic values. Production callers leave it pointing
// at the real syscall. Mirrors the package-level testability seam
// pattern established elsewhere in the codebase.
var hostnameFn = os.Hostname

// GenerateReplicaCert mints a per-replica CP server cert signed by
// the cluster CA. Template parameters:
//
//   - Subject CN: "otherix-cp-replica" + Organization "Otherix".
//   - Validity: validity from NotBefore (with 1m skew tolerance) to
//     NotBefore+validity.
//   - KeyUsage: DigitalSignature | KeyEncipherment.
//   - ExtKeyUsage: serverAuth | clientAuth — single leaf cert serves
//     both directions (agent_server listener + agent_client dialer).
//   - SAN: dnsNames + ips. Both can be empty individually but at least
//     one entry total is required so the cert has a bindable identity.
//   - Serial: cryptographically-random 64-bit positive integer
//     (matches Step 2 SignCSR serial generation).
//   - SignatureAlgorithm: ECDSAWithSHA384 (matches caKey's P-384 curve).
//
// PEM encoding mirrors GenerateClusterCA: CERTIFICATE block for cert,
// PKCS#8 PRIVATE KEY block for key (more portable than SEC1).
func GenerateReplicaCert(caCert *x509.Certificate, caKey crypto.Signer, dnsNames []string, ips []net.IP, validity time.Duration, now time.Time) (certPEM, keyPEM []byte, err error) {
	if validity <= 0 {
		validity = CPCertValidity
	}
	return signLeafCert(caCert, caKey, "otherix-cp-replica", dnsNames, ips, validity, now)
}

// signLeafCert mints an ECDSA P-384 leaf signed by the cluster CA, shared
// by the CP-replica and peer cert generators. The Subject CN is the only
// per-caller difference; every leaf is serverAuth+clientAuth (both
// directions of an mTLS handshake), carries the given SAN set (at least
// one entry required so the cert has a bindable identity), and uses a
// 64-bit random serial with a 1m backdated NotBefore for clock skew.
//
// PEM encoding mirrors GenerateClusterCA: CERTIFICATE block for cert,
// PKCS#8 PRIVATE KEY block for key.
func signLeafCert(caCert *x509.Certificate, caKey crypto.Signer, commonName string, dnsNames []string, ips []net.IP, validity time.Duration, now time.Time) (certPEM, keyPEM []byte, err error) {
	if caCert == nil {
		return nil, nil, errors.New("cp_cert: caCert is nil")
	}
	if caKey == nil {
		return nil, nil, errors.New("cp_cert: caKey is nil")
	}
	if len(dnsNames) == 0 && len(ips) == 0 {
		return nil, nil, errors.New("cp_cert: at least one DNS name or IP required for SAN")
	}

	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ecdsa key: %v", err)
	}

	serial, err := randomLeafSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %v", err)
	}

	notBefore := now.UTC().Add(-1 * time.Minute)
	notAfter := now.UTC().Add(validity)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"Otherix"},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		DNSNames:           dnsNames,
		IPAddresses:        ips,
		SignatureAlgorithm: x509.ECDSAWithSHA384,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if certPEM == nil {
		return nil, nil, errors.New("encode cert PEM: nil block")
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal pkcs8 key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if keyPEM == nil {
		return nil, nil, errors.New("encode key PEM: nil block")
	}

	return certPEM, keyPEM, nil
}

// AutoDetectSANs builds the baseline SAN set every replica's cert
// carries. Composition:
//
//   - "localhost" + 127.0.0.1 — always present (local dial-back).
//   - os.Hostname() — added as DNS name when non-empty.
//   - listenAddr's host portion — added if non-wildcard (parsed
//     via net.SplitHostPort). IP literal goes to ips slice, anything
//     else goes to dnsNames.
//
// Wildcards skipped: "" (port-only), "0.0.0.0", "::", "[::]" (the
// last form arrives during net.SplitHostPort returning unbracketed "::"
// but the constant is included for defense-in-depth).
//
// Operator extends with CPCertConfig.AdditionalSANs at the caller; the
// merge happens in the orchestrator (LoadOrGenerateCPCert) so this
// helper stays focused on the auto-detected baseline only.
func AutoDetectSANs(listenAddr string) (dnsNames []string, ips []net.IP) {
	dnsNames = []string{"localhost"}
	ips = []net.IP{net.ParseIP("127.0.0.1")}

	if hostname, err := hostnameFn(); err == nil && hostname != "" {
		dnsNames = append(dnsNames, hostname)
	}

	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil || host == "" {
		return dnsNames, ips
	}
	switch host {
	case "0.0.0.0", "::", "[::]":
		return dnsNames, ips
	}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		dnsNames = append(dnsNames, host)
	}
	return dnsNames, ips
}

// ClassifySANs splits an operator-supplied SAN list (raw strings
// from config) into DNS names + IP addresses. Parsing via net.ParseIP
// distinguishes the two — anything that fails IP parsing falls back
// to a DNS name (no further validation; openssl / x509 surface
// malformed entries at cert generation OR handshake time).
//
// Used by LoadOrGenerateCPCert to merge CPCertConfig.AdditionalSANs
// onto the AutoDetectSANs baseline.
func ClassifySANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, san := range sans {
		if san == "" {
			continue
		}
		if ip := net.ParseIP(san); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, san)
	}
	return dnsNames, ips
}

// MergeUniqueSANs combines auto-detected baseline with operator-supplied
// additions, preserving order and suppressing duplicates. DNS names
// case-folded for the dedup key (DNS is case-insensitive); IP addresses
// compared via net.IP.Equal so 127.0.0.1 + "127.000.000.001" dedup
// correctly.
//
// Used by LoadOrGenerateCPCert; exposed at package level so tests can
// assert dedup behavior without going through the orchestrator.
func MergeUniqueSANs(autoDNS []string, autoIPs []net.IP, addDNS []string, addIPs []net.IP) (dnsNames []string, ips []net.IP) {
	dnsSeen := map[string]struct{}{}
	for _, d := range append(autoDNS, addDNS...) {
		key := strings.ToLower(d)
		if _, ok := dnsSeen[key]; ok {
			continue
		}
		dnsSeen[key] = struct{}{}
		dnsNames = append(dnsNames, d)
	}
	for _, ip := range append(autoIPs, addIPs...) {
		if ip == nil {
			continue
		}
		duplicate := false
		for _, existing := range ips {
			if existing.Equal(ip) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ips = append(ips, ip)
		}
	}
	return dnsNames, ips
}

// CertCacheValid reports whether the cached cert satisfies all reuse
// invariants required for restart efficiency. Returns nil on valid;
// returns a descriptive error otherwise so the caller's structured
// log carries the rejection reason (operator debugging).
//
// Invariants checked:
//
//  1. NotAfter > now + CPCertNearExpiryBuffer — proactive rotation
//     window. Operators see expiring certs in time restart instead of
//     mid-handshake.
//  2. cert.DNSNames superset of expectedDNS — config may have grown
//     additional_sans since cert generation. Subset failure (cert
//     missing a required name) triggers regenerate. Operator shrinking
//     additional_sans does NOT invalidate (cert covers more than needed
//     stays usable).
//  3. cert.IPAddresses superset of expectedIPs — same logic for IPs.
//  4. Cert chains to currentCA via x509.Verify with the leaf's intended
//     server-auth usage. CA rotation since cert generation surfaces
//     as a chain mismatch.
func CertCacheValid(cert *x509.Certificate, expectedDNS []string, expectedIPs []net.IP, currentCA *x509.Certificate, now time.Time) error {
	if cert == nil {
		return errors.New("cert is nil")
	}
	if currentCA == nil {
		return errors.New("currentCA is nil")
	}

	if !now.Before(cert.NotAfter) {
		return fmt.Errorf("cert expired at %s (now=%s)", cert.NotAfter.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if cert.NotAfter.Sub(now) < CPCertNearExpiryBuffer {
		return fmt.Errorf("cert within %s of expiry (NotAfter=%s, now=%s) — proactive rotation", CPCertNearExpiryBuffer, cert.NotAfter.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	have := map[string]struct{}{}
	for _, d := range cert.DNSNames {
		have[strings.ToLower(d)] = struct{}{}
	}
	for _, want := range expectedDNS {
		if _, ok := have[strings.ToLower(want)]; !ok {
			return fmt.Errorf("cert missing expected DNS SAN %q", want)
		}
	}
	for _, want := range expectedIPs {
		if want == nil {
			continue
		}
		found := false
		for _, have := range cert.IPAddresses {
			if have.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("cert missing expected IP SAN %s", want)
		}
	}

	pool := x509.NewCertPool()
	pool.AddCert(currentCA)
	opts := x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("cert chain to current CA invalid: %v", err)
	}
	return nil
}

// WriteCertCacheAtomic writes the cert + key to local paths via the
// tempfile + rename pattern, mirroring internal/agent/bootstrap.
// writeFileAtomic. Both files land with the requested permissions from
// moment one (Chmod before rename), and a partial write is never
// observable by concurrent readers.
//
// Modes:
//   - cert: 0644 (world-readable, operator-inspectable).
//   - key:  0600 (owner-only, secret).
//   - parent dir (when created): 0750 (group-readable, world-not).
//
// Cert file written first so that a subsequent boot observing key-
// without-cert can detect partial state, mirroring the agent-side
// bootstrap.Persist ordering convention (key-first there because
// the key is the secret; here the order is reversed because the
// cert is what a chain-check reads first — but neither ordering is
// load-bearing for invariants, both pre-rename atomic writes).
func WriteCertCacheAtomic(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if certPath == "" || keyPath == "" {
		return errors.New("cert_path and key_path are required")
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return errors.New("cert_pem and key_pem must be non-empty")
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert %s: %v", certPath, err)
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key %s: %v", keyPath, err)
	}
	return nil
}

// WriteTrustFileAtomic writes a CA trust bundle (one or more concatenated
// CERTIFICATE PEM blocks) to path at 0644 via the same tempfile + rename
// pattern. Used for the etcd peer TrustedCAFile, which holds the cluster
// CA the peer plane verifies against.
func WriteTrustFileAtomic(path string, pem []byte) error {
	if path == "" {
		return errors.New("trust file path is required")
	}
	if len(pem) == 0 {
		return errors.New("trust bundle must be non-empty")
	}
	if err := writeFileAtomic(path, pem, 0o644); err != nil {
		return fmt.Errorf("write trust file %s: %v", path, err)
	}
	return nil
}

// writeFileAtomic mirrors the bootstrap package's helper — kept
// private to cp_cert so that mode invariants stay co-located with the
// caller. Parent dir is created at 0750 if absent.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("ensure parent dir %s: %v", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %v", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %v", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %v", tmpName, path, err)
	}
	return nil
}
