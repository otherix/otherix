// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"fmt"
)

// TLSMode controls the agent-side TLS posture chosen at Start.
type TLSMode int

const (
	// TLSDisabled wires the mock through httptest.NewServer (plaintext).
	TLSDisabled TLSMode = iota

	// TLSEnabled wires the mock through httptest.NewUnstartedServer +
	// StartTLS, presenting node.crt and verifying client certificates
	// against ca.crt. The heartbeat client reuses node.crt as its
	// client cert and pins ca.crt as the trust root for the CP
	// server cert (controlplane.crt is signed by the same CA).
	TLSEnabled
)

//go:embed testdata/certs/ca.crt
//go:embed testdata/certs/node.crt
//go:embed testdata/certs/node.key
//go:embed testdata/certs/controlplane.crt
//go:embed testdata/certs/controlplane.key
var certsFS embed.FS

// serverTLSConfig returns a *tls.Config presenting node.crt and
// requiring + verifying client certificates against ca.crt.
func serverTLSConfig() (*tls.Config, error) {
	pair, err := loadKeyPair("testdata/certs/node.crt", "testdata/certs/node.key")
	if err != nil {
		return nil, fmt.Errorf("agentmock: load node cert: %v", err)
	}
	pool, err := caPool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// clientTLSConfig returns a *tls.Config presenting node.crt as a
// client cert and pinning ca.crt as the trust root for verifying the
// CP's server cert.
func clientTLSConfig() (*tls.Config, error) {
	pair, err := loadKeyPair("testdata/certs/node.crt", "testdata/certs/node.key")
	if err != nil {
		return nil, fmt.Errorf("agentmock: load node cert: %v", err)
	}
	pool, err := caPool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func loadKeyPair(certPath, keyPath string) (tls.Certificate, error) {
	cert, err := certsFS.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read %s: %v", certPath, err)
	}
	key, err := certsFS.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read %s: %v", keyPath, err)
	}
	return tls.X509KeyPair(cert, key)
}

func caPool() (*x509.CertPool, error) {
	caBytes, err := certsFS.ReadFile("testdata/certs/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("agentmock: read ca.crt: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("agentmock: ca.crt is not valid PEM")
	}
	return pool, nil
}

// CACertPEM returns the cluster CA certificate that signed both
// node.crt and controlplane.crt, in PEM form. Tests build a CA pool
// from this for mTLS-aware HTTP servers/clients without reaching
// into the package's private testdata.
func CACertPEM() ([]byte, error) {
	return certsFS.ReadFile("testdata/certs/ca.crt")
}

// NodeCertPEM returns the agent-side leaf cert (the one mock-agent
// presents to the CP) in PEM form. Tests that need to construct a
// peer-cert chain or seed a TLS connection state with the canonical
// node.crt use this rather than reaching at testdata via relative
// paths.
func NodeCertPEM() ([]byte, error) {
	return certsFS.ReadFile("testdata/certs/node.crt")
}

// ControlPlaneServerTLSConfig returns a *tls.Config suitable for the
// CP-side TLS listener tests stand up in front of agentmock. The
// config presents controlplane.crt + key, requires + verifies
// client certificates against the same CA pool the mock issues
// node.crt against, and pins TLS 1.2 minimum. Symmetric with the
// mock's serverTLSConfig but the cert / role flip.
func ControlPlaneServerTLSConfig() (*tls.Config, error) {
	pair, err := loadKeyPair("testdata/certs/controlplane.crt", "testdata/certs/controlplane.key")
	if err != nil {
		return nil, fmt.Errorf("agentmock: load controlplane cert: %v", err)
	}
	pool, err := caPool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ControlPlaneCertPEM returns the CP-side leaf certificate (the one
// the CP→agent client presents) in PEM form. Symmetric with
// NodeCertPEM, exposed for tests in packages that need to materialise
// the cert on disk (e.g. internal/api/agentclient unit tests writing
// the cert into a temp directory consumed by file-path config).
func ControlPlaneCertPEM() ([]byte, error) {
	return certsFS.ReadFile("testdata/certs/controlplane.crt")
}

// ControlPlaneKeyPEM returns the CP-side private key matching
// ControlPlaneCertPEM in PEM form. Test-only helper.
func ControlPlaneKeyPEM() ([]byte, error) {
	return certsFS.ReadFile("testdata/certs/controlplane.key")
}

// AgentServerTLSConfig returns the *tls.Config a test server uses to
// impersonate the agent: presents node.crt + node.key and requires +
// verifies client certificates against ca.crt. Mirrors the
// configuration the real mock-agent's TLS listener installs; exposed
// so tests outside this package (e.g. internal/api/agentclient) can
// stand up an httptest.Server that accepts the CP-side client cert
// without reaching at the unexported helpers.
func AgentServerTLSConfig() (*tls.Config, error) {
	return serverTLSConfig()
}

// NodeCertFingerprint returns the SHA-256 fingerprint of node.crt
// (the cert mock-agent presents to the CP) as 32 raw bytes. Tests
// insert this into agent_certs.fingerprint_sha256 so the agentMTLS
// middleware accepts the mock's identity.
func NodeCertFingerprint() ([]byte, error) {
	pemBytes, err := certsFS.ReadFile("testdata/certs/node.crt")
	if err != nil {
		return nil, fmt.Errorf("agentmock: read node.crt: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("agentmock: node.crt is not a CERTIFICATE PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	return sum[:], nil
}
