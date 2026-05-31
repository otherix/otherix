// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// PeerCertParams drives etcd peer (Raft) mTLS material provisioning. The
// member's advertised PeerURL supplies the SAN host. Two modes, mirroring
// the CP-replica cert lifecycle:
//
//   - Operator override: all three Operator*File paths set - the cert is
//     pre-distributed (the cold multi-node bootstrap path); the files must
//     exist (missing = fatal) and are used as-is.
//   - Auto-generate (default): a fresh peer leaf is signed by the on-disk
//     cluster CA and written to the Gen*Path destinations, with the CA
//     cert materialized as the peer trust file.
type PeerCertParams struct {
	PeerURL string

	OperatorCertFile string
	OperatorKeyFile  string
	OperatorCAFile   string

	GenCertPath string
	GenKeyPath  string
	GenCAPath   string
}

// PeerCertMaterial reports the effective peer mTLS file paths for wiring
// into the etcd member config, plus the load-path label for logging.
type PeerCertMaterial struct {
	CertFile string
	KeyFile  string
	CAFile   string
	Source   string // "operator_files" | "auto_generate"
}

// operatorOverride reports whether all three operator file paths are set.
func (p PeerCertParams) operatorOverride() bool {
	return p.OperatorCertFile != "" && p.OperatorKeyFile != "" && p.OperatorCAFile != ""
}

// ProvisionPeerCert prepares the etcd peer mTLS material before the member
// starts. With an operator override it verifies the configured files exist
// and returns them; otherwise it signs a fresh peer leaf from the on-disk
// cluster CA, writes the cert/key plus the CA trust file, and returns those
// paths.
//
// The peer trust file is the cluster CA cert itself: pre-start the trust
// set is just the on-disk CA (one CA today), so the peer plane trusts
// exactly that anchor. A future rotated-in CA extends this file once
// rotation lands.
func ProvisionPeerCert(ca auth.ClusterCAResult, p PeerCertParams, now time.Time, log *slog.Logger) (PeerCertMaterial, error) {
	if p.operatorOverride() {
		return provisionOperatorPeerCert(p, ca, log)
	}

	if p.GenCertPath == "" || p.GenKeyPath == "" || p.GenCAPath == "" {
		return PeerCertMaterial{}, errors.New("peer cert auto-generate requires all three gen paths")
	}

	caCert, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		return PeerCertMaterial{}, fmt.Errorf("parse cluster CA cert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(ca.KeyPEM)
	if err != nil {
		return PeerCertMaterial{}, fmt.Errorf("parse cluster CA key: %v", err)
	}
	signer, ok := caKey.(crypto.Signer)
	if !ok {
		return PeerCertMaterial{}, errors.New("cluster CA key does not implement crypto.Signer")
	}

	dnsNames, ips := peerSANs(p.PeerURL)
	if len(dnsNames) == 0 && len(ips) == 0 {
		return PeerCertMaterial{}, fmt.Errorf("peer SAN set empty for peer_url %q", p.PeerURL)
	}

	certPEM, keyPEM, err := auth.GeneratePeerCert(caCert, signer, dnsNames, ips, auth.PeerCertValidity, now)
	if err != nil {
		return PeerCertMaterial{}, fmt.Errorf("generate peer cert: %v", err)
	}

	// cert + key via the shared atomic writer (cert 0644, key 0600).
	if err := auth.WriteCertCacheAtomic(p.GenCertPath, p.GenKeyPath, certPEM, keyPEM); err != nil {
		return PeerCertMaterial{}, fmt.Errorf("persist peer cert: %v", err)
	}
	if err := auth.WriteTrustFileAtomic(p.GenCAPath, ca.CertPEM); err != nil {
		return PeerCertMaterial{}, fmt.Errorf("persist peer CA trust file: %v", err)
	}

	log.Info("etcd peer cert: auto-generated",
		slog.String("cert_file", p.GenCertPath),
		slog.Any("dns_names", dnsNames),
		slog.String("ip_addresses", ipsToString(ips)))

	return PeerCertMaterial{
		CertFile: p.GenCertPath,
		KeyFile:  p.GenKeyPath,
		CAFile:   p.GenCAPath,
		Source:   "auto_generate",
	}, nil
}

// provisionOperatorPeerCert handles the operator-override branch: the three
// peer files must exist and the cert must chain to the cluster CA (fail fast on
// a cert issued by a different CA, which etcd would otherwise reject opaquely at
// Raft-handshake time).
func provisionOperatorPeerCert(p PeerCertParams, ca auth.ClusterCAResult, log *slog.Logger) (PeerCertMaterial, error) {
	for label, path := range map[string]string{
		"peer_cert_file": p.OperatorCertFile,
		"peer_key_file":  p.OperatorKeyFile,
		"peer_ca_file":   p.OperatorCAFile,
	} {
		if _, err := os.Stat(path); err != nil {
			return PeerCertMaterial{}, fmt.Errorf("etcd.%s %s: %v (operator peer material configured but file missing)", label, path, err)
		}
	}
	if err := verifyPeerCertChainsToCA(p.OperatorCertFile, ca.CertPEM); err != nil {
		return PeerCertMaterial{}, fmt.Errorf("operator peer cert %s: %v", p.OperatorCertFile, err)
	}
	log.Info("etcd peer cert: operator files", slog.String("cert_file", p.OperatorCertFile))
	return PeerCertMaterial{
		CertFile: p.OperatorCertFile,
		KeyFile:  p.OperatorKeyFile,
		CAFile:   p.OperatorCAFile,
		Source:   "operator_files",
	}, nil
}

// verifyPeerCertChainsToCA confirms the leaf cert at certFile verifies against
// the cluster CA (caCertPEM) for peer-mTLS usage (serverAuth+clientAuth).
func verifyPeerCertChainsToCA(certFile string, caCertPEM []byte) error {
	leafPEM, err := os.ReadFile(certFile) //nolint:gosec // operator-configured peer cert path
	if err != nil {
		return fmt.Errorf("read peer cert: %v", err)
	}
	leaf, _, err := auth.ParseClusterCACert(leafPEM)
	if err != nil {
		return fmt.Errorf("parse peer cert: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(caCertPEM)
	if err != nil {
		return fmt.Errorf("parse cluster CA cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("does not chain to the cluster CA: %v", err)
	}
	return nil
}

// peerSANs derives the SAN set for the peer cert from the member's
// advertised peer URL: the URL host plus the always-present localhost /
// 127.0.0.1 / os.Hostname baseline (AutoDetectSANs).
func peerSANs(peerURL string) (dnsNames []string, ips []net.IP) {
	u, err := url.Parse(peerURL)
	if err != nil || u.Host == "" {
		return auth.AutoDetectSANs("")
	}
	return auth.AutoDetectSANs(u.Host)
}
