// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
)

// generateKeypairAndCSR builds an ECDSA P-384 keypair and a CSR signed
// by it. ECDSA P-384 matches the cluster CA's algorithm
// (auth.GenerateClusterCA) so the issued cert chains naturally
// without forcing the CA to handle multiple key families.
//
// cnPrefix is the identity prefix the Subject CN and SAN DNS carry.
// Bootstrap always passes "node-": both a hypervisor agent and an ingress
// gateway redeem for a unified node-<name> leaf. The CP discards the CSR
// Subject entirely and derives the issued cert's identity from the validated
// request body and the join token's kind (defense-in-depth against CN
// injection), so the prefix is populated for realism and operator audit, not
// enforcement.
//
// CSR fields:
//
//   - Subject CN: "<cnPrefix><name>".
//   - SAN DNS: "<cnPrefix><name>.agents.otherix.local" + "localhost".
//   - SAN IP: 127.0.0.1.
//
// Key encoding: PKCS#8 PEM ("PRIVATE KEY" block). Matches the cluster
// CA's storage format and stays portable across Go x509 revisions.
//
// The returned *ecdsa.PrivateKey is only useful if the caller wants
// to assert something on the keypair (unit tests do this — production
// agent code discards it once the CSR is built; the key is loaded
// from disk for subsequent mTLS handshakes).
func generateKeypairAndCSR(cnPrefix, name string) (keyPEM, csrPEM []byte, priv *ecdsa.PrivateKey, err error) {
	priv, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate ecdsa key: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal pkcs8 key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if keyPEM == nil {
		return nil, nil, nil, errors.New("encode key PEM: nil block")
	}

	template := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cnPrefix + name},
		DNSNames: []string{
			cnPrefix + name + ".agents.otherix.local",
			"localhost",
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, template, priv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create csr: %v", err)
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	if csrPEM == nil {
		return nil, nil, nil, errors.New("encode csr PEM: nil block")
	}

	return keyPEM, csrPEM, priv, nil
}

// verifyResponseChain enforces two invariants on the /v1/nodes/join
// response before any file is persisted to disk:
//
//  1. sha256(returned_ca_cert.Raw) == operator-pinned fingerprint.
//     Defense-in-depth — a MITM on the join request could otherwise
//     swap in a malicious CA + matching leaf, and the agent would
//     trust both.
//  2. The returned leaf cert must chain back to the returned CA
//     using x509.Certificate.Verify. Without this, the issued cert
//     could carry any CA in `ca_cert_pem` while pointing the trust
//     anchor at a different one.
//
// Either failure aborts before persistence — caller short-circuits
// the bootstrap and the token (now consumed) is wasted, but no
// poisoned state lands on disk.
func verifyResponseChain(certPEM, caCertPEM, pinnedFingerprint string) error {
	caBlock, _ := pem.Decode([]byte(caCertPEM))
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return errors.New("ca_cert_pem is not a CERTIFICATE PEM block")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse ca_cert_pem: %v", err)
	}

	caFP := sha256.Sum256(caCert.Raw)
	caFPHex := hex.EncodeToString(caFP[:])
	if caFPHex != pinnedFingerprint {
		return &FingerprintMismatchError{Expected: pinnedFingerprint, Computed: caFPHex}
	}

	leafBlock, _ := pem.Decode([]byte(certPEM))
	if leafBlock == nil || leafBlock.Type != "CERTIFICATE" {
		return errors.New("cert_pem is not a CERTIFICATE PEM block")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse cert_pem: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	// ExtKeyUsageAny — the agent will eventually present this cert as
	// both a server (CP→agent path) and a client (heartbeat path), so
	// the standard ServerAuth-only constraint is too narrow here.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return fmt.Errorf("issued cert does not chain to returned CA: %v", err)
	}
	return nil
}
