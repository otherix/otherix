// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // SHA-1 used only for RFC 5280 SubjectKeyIdentifier, not security
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// ClusterCAValidity is the lifetime of а freshly generated cluster
// CA cert. 10 years - operator-friendly default for
// homelab / on-prem deployments. Rotation (planned ROADMAP) lets
// operators issue fresher CAs before this window closes.
const ClusterCAValidity = 10 * 365 * 24 * time.Hour

// ClusterCAResult is the bundle GenerateClusterCA returns. CertPEM /
// KeyPEM are the persisted forms (bytea в ca_certs); Fingerprint is
// sha256(cert.Raw) as 32 raw bytes — convert к hex via
// hex.EncodeToString at the API surface.
type ClusterCAResult struct {
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint []byte
	NotBefore   time.Time
	NotAfter    time.Time
}

// GenerateClusterCA produces а fresh self-signed cluster CA на ECDSA
// P-384. Parameters:
//
//   - Subject: CN=otherix-cluster-ca, OU=Otherix CA.
//   - Serial: 128-bit cryptographically-random (RFC 5280 §4.1.2.2).
//   - Validity: ClusterCAValidity (10 years).
//   - BasicConstraints: CA:TRUE, MaxPathLen:0 — signs leaf certs
//     only, no intermediate CAs.
//   - KeyUsage: certSign | crlSign | digitalSignature.
//   - SubjectKeyId derived от the public key per RFC 5280 §4.2.1.2.
//
// PEM encoding: the cert as а CERTIFICATE block, the private key as
// а PKCS#8 PRIVATE KEY block (more portable than SEC1).
func GenerateClusterCA(now time.Time) (ClusterCAResult, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("generate ecdsa key: %v", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("generate serial: %v", err)
	}

	notBefore := now.UTC()
	notAfter := notBefore.Add(ClusterCAValidity)

	skid, err := subjectKeyID(&priv.PublicKey)
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("compute SKI: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         "otherix-cluster-ca",
			OrganizationalUnit: []string{"Otherix CA"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		SubjectKeyId:          skid,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if certPEM == nil {
		return ClusterCAResult{}, errors.New("encode cert PEM: nil block")
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("marshal pkcs8 key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if keyPEM == nil {
		return ClusterCAResult{}, errors.New("encode key PEM: nil block")
	}

	fp := sha256.Sum256(der)

	return ClusterCAResult{
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		Fingerprint: fp[:],
		NotBefore:   notBefore,
		NotAfter:    notAfter,
	}, nil
}

// ParseClusterCACert lifts а PEM-encoded cert blob back into the
// parsed x509 form so callers can read fields (validity, fingerprint
// recomputation in tests). Returns the parsed cert и the underlying
// DER bytes (so fingerprints can be recomputed without re-marshalling).
func ParseClusterCACert(certPEM []byte) (*x509.Certificate, []byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, errors.New("no PEM block found")
	}
	if block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("unexpected PEM type %q", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %v", err)
	}
	return cert, block.Bytes, nil
}

// ParseClusterCAKey lifts а PEM-encoded PKCS#8 private key blob back
// into the parsed crypto form. Step 2 CSR signing path uses it к load
// the signing key from ca_certs.key_pem.
func ParseClusterCAKey(keyPEM []byte) (any, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("unexpected PEM type %q", block.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse pkcs8 key: %v", err)
	}
	return key, nil
}

// randomSerial draws а 128-bit random integer suitable for а cert
// serial (RFC 5280 §4.1.2.2 — positive integer up to 20 octets).
func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// subjectKeyID computes the RFC 5280 §4.2.1.2 SubjectKeyIdentifier:
// SHA-1 of the DER-encoded SubjectPublicKeyInfo. crypto/x509 already
// fills this field automatically when the template's SubjectKeyId is
// empty, but explicit computation keeps the cert-template self-
// describing для tests that assert на the field. SHA-1 here is purely
// для а stable identifier, not а security primitive.
func subjectKeyID(pub *ecdsa.PublicKey) ([]byte, error) {
	spkiDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	h := sha1.Sum(spkiDER) //nolint:gosec // SHA-1 is the standard RFC 5280 §4.2.1.2 hash для SubjectKeyIdentifier, not а cryptographic signature primitive
	return h[:], nil
}
