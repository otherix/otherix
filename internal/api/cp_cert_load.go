// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// TLSMaterial bundles the in-memory artefacts a CP replica needs to
// build its mTLS posture: the replica's own leaf cert (presented to
// agents on both directions per ExtKeyUsage serverAuth+clientAuth)
// + the cluster CA trust anchor (validates agent client certs on
// the inbound listener AND agent server certs on outbound dials).
//
// Source carries the load-path label so callers' structured logs
// expose which mode produced the material:
//
//   - "operator_files" — Mode A, loaded from cp_cert.cert_file/key_file.
//   - "local_cache"    — Mode B, loaded from /var/lib/otherix/certs/cp-cert.*.
//   - "auto_generate"  — Mode C, freshly minted on boot.
//   - "skipped"        — neither agent_server nor agent_client enabled;
//     material zero-valued, downstream skips wiring.
type TLSMaterial struct {
	Cert      tls.Certificate
	ClusterCA *x509.Certificate
	Source    string
}

// Skipped reports whether the material is a sentinel for a no-
// consumer boot path. Callers that would normally wire agent
// listener / client check this to skip those steps cleanly.
func (m TLSMaterial) Skipped() bool { return m.Source == "skipped" }

// CPCertCAStore is the storage surface the CP-cert loader needs: the active
// cluster CA to chain-check or sign against. *etcdstore.Store satisfies it.
type CPCertCAStore interface {
	ActiveCACert(ctx context.Context) (store.CaCert, error)
}

// LoadOrGenerateCPCert orchestrates the three-mode CP server cert
// lifecycle. Called by cmd/api/main.go after BootstrapClusterCA,
// before agent client / server construction. Returns a fully-loaded
// TLSMaterial for downstream passing to agentclient.New + the inbound
// listener's TLS config.
//
// Mode dispatch order:
//
//  1. Skip condition — neither agent_server.enabled nor
//     agent_client.enabled → return Source="skipped" zero material.
//  2. Mode A (operator files) — cp_cert.cert_file + key_file both set;
//     files must exist (missing-when-configured = fatal).
//  3. Mode B (local cache) — cp_cert.local_cache.enabled = true and
//     existing cache satisfies CertCacheValid invariants.
//  4. Mode C (auto-generate) — fallback default. If cache also
//     enabled, the generated cert is persisted to disk after success
//     (write failure logged WARN — boot succeeds on in-memory cert
//     alone).
func LoadOrGenerateCPCert(ctx context.Context, s CPCertCAStore, cfg config.APIConfig, log *slog.Logger) (TLSMaterial, error) {
	if !cfg.AgentServer.Enabled && !cfg.AgentClient.Enabled {
		log.InfoContext(ctx, "cp_cert.bootstrap.skipped",
			slog.String("reason", "no_consumer"),
			slog.Bool("agent_server_enabled", cfg.AgentServer.Enabled),
			slog.Bool("agent_client_enabled", cfg.AgentClient.Enabled))
		return TLSMaterial{Source: "skipped"}, nil
	}

	cpCfg := cfg.CPCert
	validity := cpCfg.Validity
	if validity <= 0 {
		validity = auth.CPCertValidity
	}

	// Mode A — operator-supplied cert files.
	if cpCfg.CertFile != "" && cpCfg.KeyFile != "" {
		material, err := loadOperatorFiles(cpCfg.CertFile, cpCfg.KeyFile, s, ctx)
		if err != nil {
			return TLSMaterial{}, err
		}
		log.InfoContext(ctx, "cp_cert.mode",
			slog.String("mode", "operator_files"),
			slog.String("cert_file", cpCfg.CertFile))
		return material, nil
	}

	// Load cluster CA from DB (needed by Mode B chain check and Mode C
	// signing). Failure here = boot order bug (BootstrapClusterCA
	// must have run first) OR DB connectivity failure — either way
	// fatal for the listener / agent client wiring.
	caCert, caKey, err := loadClusterCAFromDB(ctx, s)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("load cluster CA: %v", err)
	}

	// Compose expected SAN set (auto-detected baseline + operator
	// additions, deduped).
	autoDNS, autoIPs := auth.AutoDetectSANs(cfg.AgentServer.Listen)
	addDNS, addIPs := auth.ClassifySANs(cpCfg.AdditionalSANs)
	expectedDNS, expectedIPs := auth.MergeUniqueSANs(autoDNS, autoIPs, addDNS, addIPs)

	now := time.Now()

	// Mode B — local cache.
	if cpCfg.LocalCache.Enabled {
		material, reuseErr := tryLoadLocalCache(cpCfg.LocalCache.CertPath, cpCfg.LocalCache.KeyPath, expectedDNS, expectedIPs, caCert, now)
		if reuseErr == nil {
			log.InfoContext(ctx, "cp_cert.mode",
				slog.String("mode", "local_cache"),
				slog.String("cert_path", cpCfg.LocalCache.CertPath),
				slog.String("fingerprint", fingerprintHex(material.Cert)))
			return material, nil
		}
		log.InfoContext(ctx, "cp_cert.cache_invalid_regenerating",
			slog.String("cert_path", cpCfg.LocalCache.CertPath),
			slog.String("reason", reuseErr.Error()))
		// Fall through to Mode C.
	}

	// Mode C — auto-generate.
	certPEM, keyPEM, err := auth.GenerateReplicaCert(caCert, caKey, expectedDNS, expectedIPs, validity, now)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("generate replica cert: %v", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("parse generated cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("parse cert DER: %v", err)
	}

	material := TLSMaterial{
		Cert:      cert,
		ClusterCA: caCert,
		Source:    "auto_generate",
	}
	log.InfoContext(ctx, "cp_cert.mode",
		slog.String("mode", "auto_generate"),
		slog.String("fingerprint", fingerprintHex(cert)),
		slog.Time("not_after", parsed.NotAfter),
		slog.Any("dns_names", expectedDNS),
		slog.String("ip_addresses", ipsToString(expectedIPs)))

	if cpCfg.LocalCache.Enabled {
		if err := auth.WriteCertCacheAtomic(cpCfg.LocalCache.CertPath, cpCfg.LocalCache.KeyPath, certPEM, keyPEM); err != nil {
			// Cache write failure non-fatal; in-memory cert still works.
			log.WarnContext(ctx, "cp_cert.cache_write_failed",
				slog.String("cert_path", cpCfg.LocalCache.CertPath),
				slog.String("err", err.Error()))
		} else {
			log.InfoContext(ctx, "cp_cert.cache_written",
				slog.String("cert_path", cpCfg.LocalCache.CertPath))
		}
	}

	return material, nil
}

// loadOperatorFiles implements Mode A. Both paths must exist
// (configured intent + missing files = fatal, not silent fallback).
// The leaf's Subject CommonName is pinned to auth.CPCertCommonName -
// agents reject any other CN at their RequireCPIdentity gate, so a
// mismatched operator cert must fail boot, not 403 on every call.
// Cluster CA still loaded from DB so the inbound listener's ClientCAs
// pool has the correct trust anchor for agent client cert validation.
func loadOperatorFiles(certFile, keyFile string, s CPCertCAStore, ctx context.Context) (TLSMaterial, error) {
	if _, err := os.Stat(certFile); err != nil {
		return TLSMaterial{}, fmt.Errorf("cp_cert.cert_file %s: %v (Mode A configured but file missing)", certFile, err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		return TLSMaterial{}, fmt.Errorf("cp_cert.key_file %s: %v (Mode A configured but file missing)", keyFile, err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("load operator cert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		return TLSMaterial{}, fmt.Errorf("cp_cert.cert_file %s: no certificate in file", certFile)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("cp_cert.cert_file %s: parse leaf: %v", certFile, err)
	}
	if leaf.Subject.CommonName != auth.CPCertCommonName {
		return TLSMaterial{}, fmt.Errorf("cp_cert.cert_file %s: leaf CommonName %q must be %q (agents pin the control-plane identity to this CN; a mismatched cert boots but every agent rejects it with 403)", certFile, leaf.Subject.CommonName, auth.CPCertCommonName)
	}
	caCert, _, err := loadClusterCAFromDB(ctx, s)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("load cluster CA (Mode A still needs CA for ClientCAs pool): %v", err)
	}
	return TLSMaterial{Cert: cert, ClusterCA: caCert, Source: "operator_files"}, nil
}

// tryLoadLocalCache implements Mode B. Reads the cached cert + key,
// validates the cert against (expected SANs, current CA, not-near-
// expiry). On any failure returns the descriptive error so the
// orchestrator logs the reason and falls through to Mode C.
func tryLoadLocalCache(certPath, keyPath string, expectedDNS []string, expectedIPs []net.IP, currentCA *x509.Certificate, now time.Time) (TLSMaterial, error) {
	certBytes, err := os.ReadFile(certPath) //nolint:gosec // operator-configured cache path; this function exists exactly to open it
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("read cache cert: %v", err)
	}
	keyBytes, err := os.ReadFile(keyPath) //nolint:gosec // operator-configured cache path; this function exists exactly to open it
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("read cache key: %v", err)
	}
	cert, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("parse cache material: %v", err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("parse cache cert: %v", err)
	}
	if err := auth.CertCacheValid(parsed, expectedDNS, expectedIPs, currentCA, now); err != nil {
		return TLSMaterial{}, err
	}
	return TLSMaterial{Cert: cert, ClusterCA: currentCA, Source: "local_cache"}, nil
}

// loadClusterCAFromDB fetches the active cluster CA row + parses
// cert + key into typed forms. The signer interface is required by
// Mode C's auth.GenerateReplicaCert; Mode A and Mode B chain-check
// only need caCert.
func loadClusterCAFromDB(ctx context.Context, s CPCertCAStore) (*x509.Certificate, crypto.Signer, error) {
	row, err := s.ActiveCACert(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("query active CA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(row.CertPem)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %v", err)
	}
	key, err := auth.ParseClusterCAKey(row.KeyPem)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %v", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("ca key does not implement crypto.Signer")
	}
	return caCert, signer, nil
}

// fingerprintHex returns the hex-encoded SHA256 of the leaf cert's
// DER bytes — operator-facing identifier for structured logs +
// cross-referencing with openssl s_client output.
func fingerprintHex(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	fp := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(fp[:])
}

// ipsToString renders a []net.IP as a space-separated string for
// structured log fields. Less verbose than slog.Any on a large slice,
// and keeps the line operator-readable.
func ipsToString(ips []net.IP) string {
	if len(ips) == 0 {
		return ""
	}
	out := ips[0].String()
	for _, ip := range ips[1:] {
		out += " " + ip.String()
	}
	return out
}
