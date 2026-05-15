// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// defaultRequestTimeout caps а single HTTP round-trip during bootstrap.
// 30 seconds absorbs а cold TLS handshake plus the CP's redemption
// transaction, with margin for slow networks. Override per-config via
// BootstrapConfig.RequestTimeout when needed.
const defaultRequestTimeout = 30 * time.Second

// Result is the in-memory bundle produced by а successful Bootstrap
// run. KeyPEM, CertPEM, и CACertPEM are PEM-encoded blobs ready для
// direct persistence; NodeID is the CP-assigned UUID, written к the
// node-id sidecar on Persist.
type Result struct {
	NodeID    string
	KeyPEM    []byte
	CertPEM   []byte
	CACertPEM []byte
}

// Bootstrap executes the join protocol against the CP described by
// cfg и returns the issued cert material. Caller is responsible для
// persisting the result via Persist.
//
// Steps (each logged at INFO):
//
//  1. Resolve token plaintext (literal field или TokenPath file).
//  2. Fetch /v1/ca с InsecureSkipVerify, verify the returned cert's
//     sha256(cert.Raw) matches cfg.CAFingerprint (TOFU pin).
//  3. Generate ECDSA P-384 keypair и build CSR.
//  4. POST /v1/nodes/join. **The token is consumed at the CP side
//     once HTTP returns 201, even if the agent never observes the
//     response** — retry requires а fresh token.
//  5. Re-verify the returned CA matches the pinned fingerprint,
//     then verify the leaf cert chains к the same CA.
//
// Failure semantics: каждый step short-circuits с а descriptive
// error. The FingerprintMismatchError sentinel surfaces the most
// security-critical case; ErrCSRRejected wraps CP rejections so
// callers can distinguish CSR submission failures от network errors.
func Bootstrap(ctx context.Context, cfg *config.BootstrapConfig, log *slog.Logger) (*Result, error) {
	if cfg == nil {
		return nil, errors.New("bootstrap: config is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	token, err := resolveToken(cfg)
	if err != nil {
		return nil, err
	}
	hashPrefix := tokenHashPrefix(token)

	expectedFingerprint, err := NormalizeFingerprint(cfg.CAFingerprint)
	if err != nil {
		return nil, fmt.Errorf("invalid ca_fingerprint: %v", err)
	}

	log.InfoContext(ctx, "bootstrap: fetching cluster CA",
		slog.String("cp_url", cfg.CPURL),
		slog.String("expected_fingerprint", expectedFingerprint))

	caPEM, caCert, err := fetchAndVerifyCA(ctx, cfg.CPURL, expectedFingerprint, timeout)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "bootstrap: CA fingerprint verified",
		slog.String("ca_subject", caCert.Subject.String()),
		slog.String("ca_fingerprint", expectedFingerprint))

	log.InfoContext(ctx, "bootstrap: generating keypair",
		slog.String("algorithm", "ECDSA-P384"))
	keyPEM, csrPEM, _, err := generateKeypairAndCSR(cfg.NodeName)
	if err != nil {
		return nil, err
	}

	log.InfoContext(ctx, "bootstrap: submitting CSR — token will be consumed",
		slog.String("token_hash_prefix", hashPrefix),
		slog.String("node_name", cfg.NodeName))

	resp, err := submitCSR(ctx, cfg, token, string(csrPEM), timeout)
	if err != nil {
		log.WarnContext(ctx, "bootstrap: CSR submission failed — token may have been consumed",
			slog.String("token_hash_prefix", hashPrefix),
			slog.String("error", err.Error()))
		return nil, err
	}

	if err := verifyResponseChain(resp.CertPEM, resp.CACertPEM, expectedFingerprint); err != nil {
		log.ErrorContext(ctx, "bootstrap: response chain verification failed — refusing к persist potentially compromised cert material",
			slog.String("token_hash_prefix", hashPrefix),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("verify response chain: %v", err)
	}

	certFP := sha256.Sum256([]byte(resp.CertPEM))
	log.InfoContext(ctx, "bootstrap: cert issued и chain verified",
		slog.String("node_id", resp.NodeID),
		slog.String("cert_fingerprint_pem", hex.EncodeToString(certFP[:])))

	// Ensure the returned CA matches what we initially fetched (sanity
	// check — the response's ca_cert_pem could differ from /v1/ca if
	// the CP rotated mid-bootstrap, though that flow is not supported
	// в this slice). We compare the raw PEM bytes rather than parsing
	// twice — а post-validation byte mismatch indicates the response
	// CA differs от what we pinned; verifyResponseChain has already
	// confirmed the fingerprint, so this branch is mostly defensive.
	if !strings.EqualFold(strings.TrimSpace(string(caPEM)), strings.TrimSpace(resp.CACertPEM)) {
		log.WarnContext(ctx, "bootstrap: ca_cert_pem от /v1/nodes/join differs от /v1/ca cert PEM bytes (fingerprint matches — proceeding)",
			slog.String("node_id", resp.NodeID))
	}

	return &Result{
		NodeID:    resp.NodeID,
		KeyPEM:    keyPEM,
		CertPEM:   []byte(resp.CertPEM),
		CACertPEM: []byte(resp.CACertPEM),
	}, nil
}

// resolveToken returns the plaintext join token, reading от
// cfg.TokenPath если cfg.Token is empty. File contents are
// whitespace-trimmed; an empty result after trimming is treated as
// missing.
//
// The token file is NOT deleted после а successful read — operators
// own the cleanup decision (filesystem audit trail benefit или
// security risk depending on deployment).
func resolveToken(cfg *config.BootstrapConfig) (string, error) {
	if cfg.Token != "" {
		return cfg.Token, nil
	}
	if cfg.TokenPath == "" {
		return "", errors.New("bootstrap: token или token_path is required")
	}
	content, err := os.ReadFile(cfg.TokenPath)
	if err != nil {
		return "", fmt.Errorf("read token_path %s: %v", cfg.TokenPath, err)
	}
	token := strings.TrimSpace(string(content))
	if token == "" {
		return "", fmt.Errorf("token_path %s is empty after trimming whitespace", cfg.TokenPath)
	}
	return token, nil
}

// tokenHashPrefix returns the first 8 hex chars of sha256(plaintext).
// Mirrors the CP-side forensics tag в internal/api/handlers/nodejoin —
// both ends agree on the same identifier so log correlation across
// agent + CP is trivial.
func tokenHashPrefix(plaintext string) string {
	if plaintext == "" {
		return "(empty)"
	}
	sum := sha256.Sum256([]byte(plaintext))
	full := hex.EncodeToString(sum[:])
	if len(full) < 8 {
		return full
	}
	return full[:8]
}
