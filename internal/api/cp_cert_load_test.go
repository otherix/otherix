// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
)

// TestLoadOrGenerateCPCert_SkipWhenNoConsumer verifies the skip lock:
// when neither agent_server nor agent_client is enabled, the
// orchestrator returns a Source="skipped" zero material and does NOT
// touch the database. This test passes a nil store to prove the no-
// DB code path executes only the skip branch.
func TestLoadOrGenerateCPCert_SkipWhenNoConsumer(t *testing.T) {
	t.Parallel()
	cfg := config.APIConfig{
		Server:      config.ServerConfig{TLS: config.ServerTLSConfig{Enabled: false}},
		AgentServer: config.AgentServerConfig{Enabled: false},
		AgentClient: config.AgentClientConfig{Enabled: false},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	material, err := api.LoadOrGenerateCPCert(context.Background(), nil, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}
	if !material.Skipped() {
		t.Errorf("material.Skipped() = false, want true (no consumer)")
	}
	if material.Source != "skipped" {
		t.Errorf("Source = %q, want %q", material.Source, "skipped")
	}
	if len(material.Cert.Certificate) != 0 {
		t.Error("Cert should be zero-valued when skipped")
	}
	if material.ClusterCA != nil {
		t.Error("ClusterCA should be nil when skipped")
	}
}

// TestLoadOrGenerateCPCert_UserTLSForcesMaterial verifies user-facing
// TLS is a cert consumer: with agent server/client off but
// server.tls.enabled on, the orchestrator must NOT skip - it produces
// real material (auto-generate against the cluster CA).
func TestLoadOrGenerateCPCert_UserTLSForcesMaterial(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	s := &fakeCABootstrapStore{active: rowFromCA(ca)}
	cfg := config.APIConfig{
		Server:      config.ServerConfig{Listen: "127.0.0.1:8080", TLS: config.ServerTLSConfig{Enabled: true}},
		AgentServer: config.AgentServerConfig{Enabled: false},
		AgentClient: config.AgentClientConfig{Enabled: false},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	material, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}
	if material.Skipped() {
		t.Fatal("material.Skipped() = true, want false (user TLS is a consumer)")
	}
	if len(material.Cert.Certificate) == 0 {
		t.Error("Cert is empty, want a generated leaf")
	}
	if material.ClusterCA == nil {
		t.Error("ClusterCA is nil, want the cluster trust anchor")
	}
}

// writeCertFiles writes cert + key PEM blobs into dir and returns the
// two file paths, modelling the operator-supplied cp_cert.cert_file /
// key_file pair Mode A reads.
func writeCertFiles(t *testing.T, dir string, certPEM, keyPEM []byte) (certFile, keyFile string) {
	t.Helper()
	certFile = filepath.Join(dir, "cp-cert.crt")
	keyFile = filepath.Join(dir, "cp-cert.key")
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatalf("write cert file: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return certFile, keyFile
}

// replicaCertFromCA mints a CA-signed leaf carrying the pinned CP CN -
// the cert a well-behaved operator would supply for Mode A.
func replicaCertFromCA(t *testing.T, ca auth.ClusterCAResult) (certPEM, keyPEM []byte) {
	t.Helper()
	caCert, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	key, err := auth.ParseClusterCAKey(ca.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		t.Fatalf("CA key %T does not implement crypto.Signer", key)
	}
	certPEM, keyPEM, err = auth.GenerateReplicaCert(caCert, signer, []string{"localhost"}, nil, 365*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	return certPEM, keyPEM
}

// selfSignedCertWithCN mints a self-signed ECDSA P-256 leaf with the
// given Subject CommonName - the shape of a wrong operator cert (e.g.
// a node cert pointed at cp_cert.cert_file by mistake). It need not
// chain to the cluster CA: Mode A only loads the keypair and checks
// the CN.
func selfSignedCertWithCN(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8 key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestLoadOrGenerateCPCert_OperatorCertCNAccepted drives Mode A through
// the exported orchestrator with an operator cert carrying the pinned
// CP CommonName and expects clean material with Source="operator_files".
func TestLoadOrGenerateCPCert_OperatorCertCNAccepted(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	s := &fakeCABootstrapStore{active: rowFromCA(ca)}
	certPEM, keyPEM := replicaCertFromCA(t, ca)
	certFile, keyFile := writeCertFiles(t, t.TempDir(), certPEM, keyPEM)

	cfg := config.APIConfig{
		AgentServer: config.AgentServerConfig{Enabled: true},
		CPCert:      config.CPCertConfig{CertFile: certFile, KeyFile: keyFile},
	}

	material, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, discardLogger())
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert(good operator cert): %v", err)
	}
	if material.Source != "operator_files" {
		t.Errorf("Source = %q, want %q", material.Source, "operator_files")
	}
	if material.ClusterCA == nil {
		t.Error("ClusterCA = nil, want the active cluster CA loaded for the ClientCAs pool")
	}
}

// TestLoadOrGenerateCPCert_OperatorCertWrongCNRejected pins the Mode A
// boot-time guard: an operator cert whose CN is not the pinned
// control-plane identity must fail boot with an error naming the wrong
// CN and the offending config key, instead of booting fine and having
// every CP->agent call 403 at the agent's RequireCPIdentity gate.
func TestLoadOrGenerateCPCert_OperatorCertWrongCNRejected(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	s := &fakeCABootstrapStore{active: rowFromCA(ca)}
	certPEM, keyPEM := selfSignedCertWithCN(t, "node-evil")
	certFile, keyFile := writeCertFiles(t, t.TempDir(), certPEM, keyPEM)

	cfg := config.APIConfig{
		AgentServer: config.AgentServerConfig{Enabled: true},
		CPCert:      config.CPCertConfig{CertFile: certFile, KeyFile: keyFile},
	}

	material, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, discardLogger())
	if err == nil {
		t.Fatalf("LoadOrGenerateCPCert(wrong-CN operator cert) = nil error, want CN rejection (got Source=%q)", material.Source)
	}
	if !strings.Contains(err.Error(), "node-evil") {
		t.Errorf("error %q does not name the wrong CN %q", err.Error(), "node-evil")
	}
	if !strings.Contains(err.Error(), "cp_cert.cert_file") {
		t.Errorf("error %q does not name the offending config key %q", err.Error(), "cp_cert.cert_file")
	}
	if material.Source != "" || len(material.Cert.Certificate) != 0 || material.ClusterCA != nil {
		t.Errorf("material = %+v, want zero value on CN rejection", material)
	}
}
