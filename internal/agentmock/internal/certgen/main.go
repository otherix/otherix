// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command certgen regenerates the static mTLS test certificates
// consumed by the agent-mock fixture. It is run on demand
// (`make agentmock-certs`) — the output is committed to git so the
// test suite never has to keygen at runtime.
//
// Three certificates are emitted, all ECDSA P-256, signed by a
// self-signed test CA, validity 2026-01-01 → 2076-01-01:
//
//   - ca.{crt,key}             CN otherix-test-ca
//   - node.{crt,key}            CN node-mock-1.agents.otherix.local
//   - controlplane.{crt,key}    CN control-plane.otherix.local
//
// The leaf certificates carry both server-auth and client-auth
// extended key usage so the same material can be used for the
// mock's TLS listener and for its outbound heartbeat client.
//
// The CA private key (ca.key) ships in testdata/ because its only
// purpose is to sign the two test leaves; nothing it signs is
// trusted by production code.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	certPerm = 0o644
	keyPerm  = 0o600
)

var (
	notBefore = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter  = time.Date(2076, 1, 1, 0, 0, 0, 0, time.UTC)
)

func main() {
	out := flag.String("out", "internal/agentmock/testdata/certs",
		"directory the regenerated certificates are written to")
	flag.Parse()
	if err := run(*out); err != nil {
		log.Fatalf("certgen: %v", err)
	}
}

// run regenerates the CA, node, and control-plane certificate pairs
// and writes them under outDir. Existing files are overwritten.
func run(outDir string) error {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %v", outDir, err)
	}
	caCert, caKey, err := generateCA()
	if err != nil {
		return fmt.Errorf("generate CA: %v", err)
	}
	if err := writeCertPair(outDir, "ca", caCert, caKey); err != nil {
		return err
	}
	nodeCert, nodeKey, err := generateLeaf(caCert, caKey, leafSpec{
		serial: 2,
		cn:     "node-mock-1.agents.otherix.local",
		dns:    []string{"node-mock-1.agents.otherix.local", "localhost"},
		ips:    []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	})
	if err != nil {
		return fmt.Errorf("generate node leaf: %v", err)
	}
	if err := writeCertPair(outDir, "node", nodeCert, nodeKey); err != nil {
		return err
	}
	cpCert, cpKey, err := generateLeaf(caCert, caKey, leafSpec{
		serial: 3,
		cn:     "control-plane.otherix.local",
		dns:    []string{"control-plane.otherix.local", "localhost"},
		ips:    []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	})
	if err != nil {
		return fmt.Errorf("generate control-plane leaf: %v", err)
	}
	if err := writeCertPair(outDir, "controlplane", cpCert, cpKey); err != nil {
		return err
	}
	return nil
}

// leafSpec carries the per-leaf parameters that vary between the
// node and control-plane certificates.
type leafSpec struct {
	serial int64
	cn     string
	dns    []string
	ips    []net.IP
}

// generateCA returns a freshly-issued self-signed test CA along with
// its private key.
func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ecdsa keygen: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "otherix-test-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("ParseCertificate: %v", err)
	}
	return cert, key, nil
}

// generateLeaf returns a leaf certificate signed by the supplied CA,
// suitable for both server and client TLS authentication.
func generateLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, spec leafSpec) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ecdsa keygen: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(spec.serial),
		Subject:      pkix.Name{CommonName: spec.cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		DNSNames:    spec.dns,
		IPAddresses: spec.ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("ParseCertificate: %v", err)
	}
	return cert, key, nil
}

// writeCertPair persists cert as `<name>.crt` and key as `<name>.key`
// under outDir, both PEM-encoded. The certificate file is
// world-readable; the key file is owner-only.
func writeCertPair(outDir, name string, cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	certPath := filepath.Join(outDir, name+".crt")
	if err := writePEM(certPath, "CERTIFICATE", cert.Raw, certPerm); err != nil {
		return fmt.Errorf("write %s: %v", certPath, err)
	}
	keyPath := filepath.Join(outDir, name+".key")
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key for %s: %v", name, err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", der, keyPerm); err != nil {
		return fmt.Errorf("write %s: %v", keyPath, err)
	}
	return nil
}

// writePEM PEM-encodes der under the given block type and writes it
// to path with the supplied permissions.
func writePEM(path, blockType string, der []byte, perm os.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // CLI tool: output path is the operator-supplied argument.
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
