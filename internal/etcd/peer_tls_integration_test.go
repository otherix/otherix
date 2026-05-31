// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"crypto"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
)

// TestStartWithPeerMTLS proves a single-node member boots with peer (Raft)
// mTLS always on - the uniform-https-peer posture every topology runs. It
// generates a cluster CA, signs a peer leaf with auth.GeneratePeerCert,
// materializes the trust file, and starts the member with PeerTLSInfo +
// ClientCertAuth pointing at them; a successful write confirms the member
// came up and the peer cert / SAN / trust anchor are mutually consistent.
func TestStartWithPeerMTLS(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	ca, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(ca.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := caKey.(crypto.Signer)
	if !ok {
		t.Fatal("CA key is not a crypto.Signer")
	}

	certPEM, keyPEM, err := auth.GeneratePeerCert(caCert, signer,
		[]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, auth.PeerCertValidity, now)
	if err != nil {
		t.Fatalf("GeneratePeerCert: %v", err)
	}

	certFile := filepath.Join(dir, "peer.crt")
	keyFile := filepath.Join(dir, "peer.key")
	caFile := filepath.Join(dir, "ca.crt")
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)
	writeFile(t, caFile, ca.CertPEM)

	cfg := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n1",
		DataDir:      filepath.Join(dir, "member"),
		PeerURL:      fmt.Sprintf("https://127.0.0.1:%d", freePort(t)),
		ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClusterToken: "otherix-test",
		PeerCertFile: certFile,
		PeerKeyFile:  keyFile,
		PeerCAFile:   caFile,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Start with peer mTLS: %v", err)
	}
	defer r.Stop(10 * time.Second)

	if err := put(ctx, cfg.ClientURL, "/otherix/test/peer-mtls", "ok"); err != nil {
		t.Fatalf("put after peer-mTLS boot: %v", err)
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
