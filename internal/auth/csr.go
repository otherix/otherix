// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// LeafCertValidity is the lifetime of an agent cert issued by the
// Step 2 redemption flow. 1 year matches the dev/scripts/generate-
// certs.sh template — Step 4 dev env migration replaces those certs
// with redemption-issued certs of identical lifetime.
const LeafCertValidity = 365 * 24 * time.Hour

// minRSABits is the lower bound enforced on incoming CSR public keys.
// 2048 bits is the modern weak-end; RSA 1024 was deprecated by NIST in
// 2013. Whitelist also accepts ECDSA P-256 and P-384.
const minRSABits = 2048

// Sentinel errors returned by ValidateCSR. The redemption handler
// type-switches on these to surface specific operator-facing messages
// inside the wrapped 400 envelope.
var (
	// ErrCSRPEM is returned when the input bytes are not a valid
	// "CERTIFICATE REQUEST" PEM block.
	ErrCSRPEM = errors.New("csr: not a CERTIFICATE REQUEST PEM block")

	// ErrCSRParse is returned when x509.ParseCertificateRequest
	// rejects the DER contents.
	ErrCSRParse = errors.New("csr: malformed DER contents")

	// ErrCSRSignature is returned when csr.CheckSignature fails —
	// the agent did not actually own the private key.
	ErrCSRSignature = errors.New("csr: signature verification failed")

	// ErrCSRKeyType is returned when the public key falls outside
	// the whitelist (RSA 2048+, ECDSA P-256/P-384).
	ErrCSRKeyType = errors.New("csr: key type not allowed")

	// ErrCSRForbidExt is returned when the CSR carries a CA-creating
	// extension (BasicConstraints CA:TRUE or KeyUsage with certSign /
	// crlSign). Defense-in-depth — the cert template always overrides
	// these on signing, but rejecting them upfront keeps the audit
	// trail clean.
	ErrCSRForbidExt = errors.New("csr: forbidden extension present")
)

// Extension OIDs used by validateExtensions to inspect CSR-attached
// extensions without a full x509 parse. Sourced from RFC 5280.
var (
	oidBasicConstraints = []int{2, 5, 29, 19}
	oidKeyUsage         = []int{2, 5, 29, 15}
)

// ValidateCSR parses + validates an incoming CSR per Otherix policy.
// Returns the parsed *x509.CertificateRequest on success; the CSR is
// ready for SignCSR. On rejection returns one of the Err* sentinels
// wrapped with context (use errors.Is to dispatch).
//
// The CSR Subject and SAN are NOT examined here — SignCSR ignores them
// entirely and sets the cert template from validated request parameters
// (defense-in-depth against CN injection).
func ValidateCSR(pemBytes []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, ErrCSRPEM
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCSRParse, err)
	}

	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCSRSignature, err)
	}

	if err := validateKeyType(csr.PublicKey); err != nil {
		return nil, err
	}

	if err := validateExtensions(csr); err != nil {
		return nil, err
	}

	return csr, nil
}

// validateKeyType accepts RSA 2048+ and ECDSA P-256 / P-384. Ed25519
// rejected in the first slice - future expansion easy (add to
// the type switch).
func validateKeyType(key crypto.PublicKey) error {
	switch k := key.(type) {
	case *rsa.PublicKey:
		bits := k.N.BitLen()
		if bits < minRSABits {
			return fmt.Errorf("%w: RSA key size %d below minimum %d", ErrCSRKeyType, bits, minRSABits)
		}
		return nil
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384():
			return nil
		}
		return fmt.Errorf("%w: ECDSA curve %s not allowed (want P-256 or P-384)", ErrCSRKeyType, k.Curve.Params().Name)
	default:
		return fmt.Errorf("%w: type %T not allowed (want RSA 2048+ or ECDSA P-256/P-384)", ErrCSRKeyType, key)
	}
}

// validateExtensions rejects CA-creating extensions: BasicConstraints
// CA:TRUE and KeyUsage with certSign or crlSign bits. The cert template
// downstream of validation always overrides these, but rejecting
// upfront keeps the audit trail honest about what the agent actually
// submitted.
func validateExtensions(csr *x509.CertificateRequest) error {
	for _, ext := range csr.Extensions {
		switch {
		case ext.Id.Equal(oidBasicConstraints):
			if isCABasicConstraints(ext.Value) {
				return fmt.Errorf("%w: BasicConstraints CA:TRUE", ErrCSRForbidExt)
			}
		case ext.Id.Equal(oidKeyUsage):
			if hasCAKeyUsageBits(ext.Value) {
				return fmt.Errorf("%w: KeyUsage with certSign or crlSign", ErrCSRForbidExt)
			}
		}
	}
	return nil
}

// isCABasicConstraints reports whether the encoded BasicConstraints
// extension flags CA:TRUE. The DER encoding is a SEQUENCE — the BOOLEAN
// `cA` value lives at byte 4 when present (SEQUENCE tag, length, BOOLEAN
// tag, length, value). False / absent CA bit ⇒ false.
func isCABasicConstraints(value []byte) bool {
	// 0x30 = SEQUENCE; 0x01 = BOOLEAN; 0xFF = TRUE
	// Minimal "CA:TRUE" encoding: 30 03 01 01 FF
	if len(value) >= 5 && value[0] == 0x30 && value[2] == 0x01 && value[3] == 0x01 && value[4] == 0xFF {
		return true
	}
	return false
}

// hasCAKeyUsageBits reports whether the encoded KeyUsage extension
// contains certSign (bit 5) or crlSign (bit 6). The DER form is a
// BIT STRING; the first content byte signals unused bits, and
// subsequent bytes carry the flags in MSB-first order.
//
// Bit positions per RFC 5280 §4.2.1.3:
//
//	digitalSignature (0) | nonRepudiation (1) | keyEncipherment (2) |
//	dataEncipherment (3) | keyAgreement (4)   | keyCertSign (5)     |
//	cRLSign (6)          | encipherOnly (7)   | decipherOnly (8)
func hasCAKeyUsageBits(value []byte) bool {
	// Find the BIT STRING contents. Outer wrapper is OCTET STRING (0x04)
	// containing the BIT STRING (0x03). We scan for the bit-string
	// header rather than depending on a full ASN.1 parse.
	var idx int
	switch {
	case len(value) > 0 && value[0] == 0x03:
		// raw BIT STRING (no OCTET wrap)
		idx = 0
	case len(value) > 2 && value[0] == 0x04 && value[2] == 0x03:
		// skip OCTET STRING wrapper
		idx = 2
	default:
		return false
	}
	if idx+2 >= len(value) {
		return false
	}
	bitStringLen := int(value[idx+1])
	if idx+2+bitStringLen > len(value) || bitStringLen < 2 {
		return false
	}
	// value[idx+2] = unused bits; flags start at idx+3.
	flags := value[idx+3:]
	// Bit 5 (keyCertSign) lives in byte 0 at mask 0x04 (MSB-first counting).
	// Bit 6 (cRLSign) lives in byte 0 at mask 0x02.
	if len(flags) >= 1 {
		if flags[0]&0x04 != 0 {
			return true
		}
		if flags[0]&0x02 != 0 {
			return true
		}
	}
	return false
}

// SignCSR signs a validated CSR using the cluster CA. Returns the
// issued cert in PEM form plus the parsed *x509.Certificate for the
// caller's metadata-extraction needs (serial, fingerprint, subject DN).
//
// Cert template is server-authoritative — the CSR's Subject / SAN /
// extensions are ignored entirely. Template fields:
//
//   - Subject CN: "node-<nodeName>" derived from the validated request.
//   - SAN DNS: "node-<nodeName>.agents.otherix.local", "localhost".
//   - SAN IP: 127.0.0.1.
//   - Validity: LeafCertValidity (1 year), with a 1m skew tolerance on
//     NotBefore so freshly-issued certs don't fail validation on
//     callers with slightly-fast clocks.
//   - KeyUsage: DigitalSignature | KeyEncipherment.
//   - ExtKeyUsage: serverAuth | clientAuth (agents serve CP→agent
//     requests and act as clients on the heartbeat path).
//   - Serial: cryptographically-random 64-bit positive integer.
//   - SignatureAlgorithm: ECDSAWithSHA384 (matches the CA's P-384 key).
func SignCSR(csr *x509.CertificateRequest, nodeName string, caCert *x509.Certificate, caKey crypto.Signer, now time.Time) (certPEM []byte, cert *x509.Certificate, err error) {
	serial, err := randomLeafSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %v", err)
	}

	notBefore := now.UTC().Add(-1 * time.Minute) // skew tolerance
	notAfter := now.UTC().Add(LeafCertValidity)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "node-" + nodeName,
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		DNSNames: []string{
			"node-" + nodeName + ".agents.otherix.local",
			"localhost",
		},
		IPAddresses:        []net.IP{net.ParseIP("127.0.0.1")},
		SignatureAlgorithm: x509.ECDSAWithSHA384,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %v", err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse issued cert: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if certPEM == nil {
		return nil, nil, errors.New("encode cert PEM: nil block")
	}

	return certPEM, parsed, nil
}

// randomLeafSerial returns a cryptographically-random positive 64-bit
// integer suitable for a leaf cert serial. 64 bits gives ~10^19
// distinct values — collision probability is negligible across any
// realistic fleet size (RFC 5280 allows up to 20 octets / 160 bits;
// 64 bits is plenty for a homelab CA's lifetime).
func randomLeafSerial() (*big.Int, error) {
	maxSerial := new(big.Int).Lsh(big.NewInt(1), 63)
	n, err := rand.Int(rand.Reader, maxSerial)
	if err != nil {
		return nil, err
	}
	// Add 1 to guarantee positive non-zero (rand.Int returns [0, max)).
	return n.Add(n, big.NewInt(1)), nil
}
