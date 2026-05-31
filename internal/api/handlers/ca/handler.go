// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ca hosts the anonymous /v1/ca endpoint. It returns the cluster
// CA trust set as a bundle - every CA an agent or peer must currently
// trust, plus which fingerprint is the active signer - so Step 3 agent
// bootstrap can perform TOFU validation and persist the whole trust
// anchor. With no rotation the bundle holds one CA (the signer); a
// future rotation adds the incoming CA alongside the outgoing one until
// it retires. The CA private key never leaves the server - the view
// projection omits the key_pem column from ca_certs entirely.
//
// The endpoint is mounted outside any auth middleware: agents at
// bootstrap time have neither a Bearer JWT nor an mTLS client cert
// (the chicken-and-egg the join-token bootstrap exists to solve). The
// fingerprint distributed via the operator out of band (alongside
// the join token plaintext) is the only authentication channel.
package ca

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the /v1/ca handler depends on: the trust set
// (every currently-trusted CA) plus the active signer for its fingerprint.
// *etcdstore.Store satisfies it; depending on the interface rather than the
// concrete store narrows the handler's storage dependency to the methods it
// uses and lets tests substitute a fake.
type Store interface {
	ActiveCACert(ctx context.Context) (store.CaCert, error)
	ListTrustedCACerts(ctx context.Context) ([]store.CaCert, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies of the /v1/ca routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs the Handler.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// clusterCACertView mirrors components/schemas/ClusterCACert. KeyPem is
// intentionally absent — the projection guarantees the CA private key
// cannot leak via this endpoint.
type clusterCACertView struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
}

// clusterCAView mirrors components/schemas/ClusterCA: the trust-set bundle
// plus the active signer's fingerprint.
type clusterCAView struct {
	CAs                     []clusterCACertView `json:"cas"`
	SignerFingerprintSHA256 string              `json:"signer_fingerprint_sha256"`
}

// Get implements GET /v1/ca (anonymous). Returns 200 with the trust-set
// bundle on the happy path; 500 internal when no active CA exists (the
// boot-time BootstrapClusterCA hook guarantees this in production — a 500
// here surfaces an operational invariant violation).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	signer, err := h.store.ActiveCACert(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.log.WarnContext(ctx, "no active cluster CA row — bootstrap hook not run?",
				slog.String("path", r.URL.Path))
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "cluster CA not provisioned", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster CA lookup failed", nil)
		return
	}

	trusted, err := h.store.ListTrustedCACerts(ctx)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster CA lookup failed", nil)
		return
	}
	if len(trusted) == 0 {
		// The active signer should always be in the trust set; an empty set
		// means it is retired or out of validity, an operational invariant
		// violation the agent cannot recover from.
		h.log.WarnContext(ctx, "active cluster CA is not in the trust set (retired or expired?)",
			slog.String("path", r.URL.Path))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster CA not provisioned", nil)
		return
	}

	cas := make([]clusterCACertView, 0, len(trusted))
	for _, ca := range trusted {
		cas = append(cas, clusterCACertView{
			CertPEM:           string(ca.CertPem),
			FingerprintSHA256: hex.EncodeToString(ca.FingerprintSha256),
			NotBefore:         ca.NotBefore.UTC().Format(time.RFC3339Nano),
			NotAfter:          ca.NotAfter.UTC().Format(time.RFC3339Nano),
		})
	}

	view := clusterCAView{
		CAs:                     cas,
		SignerFingerprintSHA256: hex.EncodeToString(signer.FingerprintSha256),
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}
