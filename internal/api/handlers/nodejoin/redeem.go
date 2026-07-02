// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// caBundle is the in-memory projection of the active ca_certs row used
// by redemption. Pre-parsed cert + key to avoid re-parsing inside the
// signing callback.
type caBundle struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
}

// loadCA fetches the active cluster CA row, parses both cert and key,
// and returns a caBundle ready for SignCSR. Per-request load mirrors
// the /v1/ca handler pattern — the bootstrap path is infrequent enough
// that the parse cost is acceptable, and avoiding cached state
// simplifies CA rotation flows landing later.
func (h *Handler) loadCA(ctx context.Context) (*caBundle, error) {
	row, err := h.store.ActiveCACert(ctx)
	if err != nil {
		return nil, fmt.Errorf("active CA lookup: %v", err)
	}
	cert, _, err := auth.ParseClusterCACert(row.CertPem)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %v", err)
	}
	rawKey, err := auth.ParseClusterCAKey(row.KeyPem)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %v", err)
	}
	signer, ok := rawKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA key type %T does not implement crypto.Signer", rawKey)
	}
	return &caBundle{cert: cert, key: signer, certPEM: row.CertPem}, nil
}

// redeemResult is the in-memory shape of a successful redemption,
// passed back to the handler for response construction. The full cert
// PEM lives here (and in the HTTP response); the database keeps only
// metadata.
type redeemResult struct {
	nodeID  string
	certPEM []byte
}

// redeem orchestrates the atomic Step 2 redemption. The caller has
// already validated request shape + CSR contents; this method delegates
// the transaction to store.RedeemJoinToken and supplies a sign callback
// that performs the actual CSR signing (keeping x509 / crypto out of the
// store). The signed cert PEM and fingerprint are captured from the
// callback for the HTTP response and the post-commit audit log.
func (h *Handler) redeem(ctx context.Context, req joinRequest, csr *x509.CertificateRequest, ca *caBundle, sourceIP *netip.Addr) (redeemResult, error) {
	var certPEM []byte
	var fingerprint [32]byte

	res, err := h.store.RedeemJoinToken(ctx, store.RedeemJoinTokenParams{
		TokenHash:                 auth.HashToken(req.Token),
		NodeName:                  req.NodeName,
		Architecture:              store.CPUArch(req.Architecture),
		AdvertisedEndpoint:        req.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: req.IngressAdvertisedEndpoint,
		MigrationHost:             req.MigrationHost,
		MigrationPortRangeStart:   req.MigrationPortRangeStart,
		MigrationPortRangeEnd:     req.MigrationPortRangeEnd,
		SourceIP:                  sourceIP,
	}, func(node store.Node) (store.IssuedCert, error) {
		// Every node, gateway or hypervisor, gets one node-<name> leaf. A gateway
		// node also advertises an ingress endpoint whose host may differ from the
		// control host, so both hosts land in the leaf SAN.
		pem, parsed, err := auth.SignCSR(csr, req.NodeName, node.AdvertisedEndpoint, ca.cert, ca.key, time.Now(), node.IngressAdvertisedEndpoint)
		if err != nil {
			return store.IssuedCert{}, fmt.Errorf("sign csr: %v", err)
		}
		certPEM = pem
		fingerprint = sha256.Sum256(parsed.Raw)
		return store.IssuedCert{
			Serial:            parsed.SerialNumber.Bytes(),
			FingerprintSha256: fingerprint[:],
			SubjectDN:         parsed.Subject.String(),
			NotBefore:         parsed.NotBefore,
			NotAfter:          parsed.NotAfter,
		}, nil
	})
	if err != nil {
		return redeemResult{}, err
	}

	h.log.InfoContext(ctx, "join token redeemed",
		slog.String("token_id", res.TokenID.String()),
		slog.String("node_id", res.NodeID.String()),
		slog.String("node_name", req.NodeName),
		slog.String("cert_fingerprint", fmt.Sprintf("%x", fingerprint)))

	return redeemResult{nodeID: res.NodeID.String(), certPEM: certPEM}, nil
}
