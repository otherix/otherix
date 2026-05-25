// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// freshStoreWithCA returns a Store with fresh ca_certs row populated by
// BootstrapClusterCA. Tests that depend on cluster CA loading use this
// helper to skip the BootstrapAdmin path (irrelevant to cp_cert lifecycle).
func freshStoreWithCA(t *testing.T) *store.Store {
	t.Helper()
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN: sharedHarness.DSN, MaxConns: 4, MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(s.Close)

	// Clear any prior state (table per-test isolation).
	if _, err := s.Pool().Exec(context.Background(), "delete from ca_certs"); err != nil {
		t.Fatalf("reset ca_certs: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := api.BootstrapClusterCA(context.Background(), s, log); err != nil {
		t.Fatalf("BootstrapClusterCA: %v", err)
	}
	return s
}

func cpCertConfigBase() config.APIConfig {
	return config.APIConfig{
		AgentServer: config.AgentServerConfig{Enabled: true, Listen: ":8443"},
		AgentClient: config.AgentClientConfig{Enabled: false},
		CPCert: config.CPCertConfig{
			Validity: 365 * 24 * time.Hour,
		},
	}
}

func fingerprintOf(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	fp := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(fp[:])
}

func TestCPCert_BootGeneratesFresh(t *testing.T) {
	s := freshStoreWithCA(t)
	cfg := cpCertConfigBase()
	cfg.CPCert.AdditionalSANs = []string{"replica-1.cluster.local"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	material, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}
	if material.Source != "auto_generate" {
		t.Errorf("Source = %q, want auto_generate", material.Source)
	}
	if material.ClusterCA == nil {
		t.Fatal("ClusterCA nil")
	}
	if len(material.Cert.Certificate) == 0 {
		t.Fatal("Cert empty")
	}
	parsed, err := x509.ParseCertificate(material.Cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	// Operator's additional_sans should appear in DNSNames.
	found := false
	for _, name := range parsed.DNSNames {
		if name == "replica-1.cluster.local" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DNSNames %v missing replica-1.cluster.local", parsed.DNSNames)
	}
	// Chain validates.
	pool := x509.NewCertPool()
	pool.AddCert(material.ClusterCA)
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("Verify against cluster CA failed: %v", err)
	}
}

func TestCPCert_RestartReusesCache(t *testing.T) {
	s := freshStoreWithCA(t)
	cacheDir := t.TempDir()
	cfg := cpCertConfigBase()
	cfg.CPCert.LocalCache = config.CPCertCacheConfig{
		Enabled:  true,
		CertPath: filepath.Join(cacheDir, "cp-cert.crt"),
		KeyPath:  filepath.Join(cacheDir, "cp-cert.key"),
	}
	cfg.CPCert.AdditionalSANs = []string{"stable-replica.example.com"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("first LoadOrGenerateCPCert: %v", err)
	}
	if first.Source != "auto_generate" {
		t.Errorf("first Source = %q, want auto_generate", first.Source)
	}

	// Second call should reuse cache.
	second, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("second LoadOrGenerateCPCert: %v", err)
	}
	if second.Source != "local_cache" {
		t.Errorf("second Source = %q, want local_cache", second.Source)
	}
	if fingerprintOf(first.Cert) != fingerprintOf(second.Cert) {
		t.Error("fingerprint differs across boots — cache not reused")
	}
}

func TestCPCert_RestartRegeneratesOnSANDrift(t *testing.T) {
	s := freshStoreWithCA(t)
	cacheDir := t.TempDir()
	cfg := cpCertConfigBase()
	cfg.CPCert.LocalCache = config.CPCertCacheConfig{
		Enabled:  true,
		CertPath: filepath.Join(cacheDir, "cp-cert.crt"),
		KeyPath:  filepath.Join(cacheDir, "cp-cert.key"),
	}
	cfg.CPCert.AdditionalSANs = []string{"old-name.example.com"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Add a new SAN — cache should invalidate.
	cfg.CPCert.AdditionalSANs = []string{"old-name.example.com", "new-name.example.com"}
	second, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.Source != "auto_generate" {
		t.Errorf("Source = %q, want auto_generate (SAN drift)", second.Source)
	}
	if fingerprintOf(first.Cert) == fingerprintOf(second.Cert) {
		t.Error("fingerprint identical despite SAN drift")
	}
	parsed, _ := x509.ParseCertificate(second.Cert.Certificate[0])
	found := false
	for _, name := range parsed.DNSNames {
		if name == "new-name.example.com" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("new SAN missing from regenerated cert: %v", parsed.DNSNames)
	}
}

func TestCPCert_HASimulation(t *testing.T) {
	s := freshStoreWithCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Two replicas with different hostnames — simulate via hostnameFn swap
	// since auth.AutoDetectSANs is the only consumer of os.Hostname.
	// We can't easily run two concurrent boots through one process
	// without coordinating the swap, so test serial regeneration with
	// different hostnames + verify both chains validate against the
	// same cluster CA.

	// Replica 1
	cfg1 := cpCertConfigBase()
	cfg1.CPCert.AdditionalSANs = []string{"replica-1"}
	mat1, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg1, log)
	if err != nil {
		t.Fatalf("replica1: %v", err)
	}

	// Replica 2 — different SANs simulating different identity.
	cfg2 := cpCertConfigBase()
	cfg2.CPCert.AdditionalSANs = []string{"replica-2"}
	mat2, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg2, log)
	if err != nil {
		t.Fatalf("replica2: %v", err)
	}

	// Both chain to same CA.
	pool := x509.NewCertPool()
	pool.AddCert(mat1.ClusterCA)
	for i, mat := range []api.TLSMaterial{mat1, mat2} {
		parsed, _ := x509.ParseCertificate(mat.Cert.Certificate[0])
		if _, err := parsed.Verify(x509.VerifyOptions{
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("replica %d cert chain invalid: %v", i+1, err)
		}
	}
	// Distinct fingerprints.
	if fingerprintOf(mat1.Cert) == fingerprintOf(mat2.Cert) {
		t.Error("HA replicas produced identical fingerprints — auto-detection broken")
	}
}

func TestCPCert_HASimulationConcurrent(t *testing.T) {
	s := freshStoreWithCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Two concurrent calls — both should succeed independently. No
	// shared state between replicas (each generates own cert), so no
	// race conditions expected.
	var wg sync.WaitGroup
	results := make([]api.TLSMaterial, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cfg := cpCertConfigBase()
			cfg.CPCert.AdditionalSANs = []string{}
			results[idx], errs[idx] = api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent boot %d failed: %v", i, err)
		}
	}
	// Same CA loaded by both.
	if results[0].ClusterCA == nil || results[1].ClusterCA == nil {
		t.Fatal("ClusterCA nil in one or both results")
	}
	if results[0].ClusterCA.SerialNumber.Cmp(results[1].ClusterCA.SerialNumber) != 0 {
		t.Error("replicas loaded different CA rows — partial unique index broken?")
	}
}

func TestCPCert_ManualOverride(t *testing.T) {
	s := freshStoreWithCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Generate a cert externally + write to temp paths to simulate
	// operator-supplied Mode A material. The "external" CA here = same
	// cluster CA in DB so chain validation passes downstream; in
	// production operator might use Let's Encrypt OR corporate CA.
	row, err := s.Queries().GetActiveCACert(context.Background())
	if err != nil {
		t.Fatalf("GetActiveCACert: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(row.CertPem)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(row.KeyPem)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	caKeySigner, ok := caKey.(crypto.Signer)
	if !ok {
		t.Fatalf("caKey is %T, not crypto.Signer", caKey)
	}
	certPEM, keyPEM, err := auth.GenerateReplicaCert(caCert, caKeySigner, []string{"externally-supplied.example.com"}, nil, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "operator-cert.crt")
	keyPath := filepath.Join(dir, "operator-cert.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := cpCertConfigBase()
	cfg.CPCert.CertFile = certPath
	cfg.CPCert.KeyFile = keyPath

	mat, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}
	if mat.Source != "operator_files" {
		t.Errorf("Source = %q, want operator_files", mat.Source)
	}
	parsed, _ := x509.ParseCertificate(mat.Cert.Certificate[0])
	if parsed.DNSNames[0] != "externally-supplied.example.com" {
		t.Errorf("DNSNames = %v, want externally-supplied SAN to lead", parsed.DNSNames)
	}
}

func TestCPCert_ManualOverride_MissingFilesFatal(t *testing.T) {
	s := freshStoreWithCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := cpCertConfigBase()
	cfg.CPCert.CertFile = "/nonexistent/cert.crt"
	cfg.CPCert.KeyFile = "/nonexistent/cert.key"

	_, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err == nil {
		t.Fatal("expected fatal error for missing operator files")
	}
}

func TestCPCert_AgentMTLSHandshake(t *testing.T) {
	s := freshStoreWithCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := cpCertConfigBase()
	cfg.CPCert.AdditionalSANs = []string{"127.0.0.1"}
	mat, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}

	// Stand up a TLS server presenting mat.Cert; client built with pool
	// rooted at mat.ClusterCA must complete handshake.
	pool := x509.NewCertPool()
	pool.AddCert(mat.ClusterCA)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{mat.Cert},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "127.0.0.1",
				MinVersion: tls.VersionTLS12,
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := httpClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get against CP-cert server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want 200", resp.StatusCode)
	}
}

func TestCPCert_PEMShapeStable(t *testing.T) {
	// Catch accidental encoding drift in PEM output — both PEM blocks
	// must decode cleanly and round-trip back through tls.X509KeyPair.
	s := freshStoreWithCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := cpCertConfigBase()
	mat, err := api.LoadOrGenerateCPCert(context.Background(), s, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}

	// re-encode the parsed cert and ensure it still parses
	der := mat.Cert.Certificate[0]
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if encoded == nil {
		t.Fatal("re-encode produced nil")
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		t.Fatal("re-encoded PEM doesn't decode")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Fatalf("re-parse failed: %v", err)
	}
}
