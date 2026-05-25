// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ca hosts the anonymous /v1/ca endpoint. The active cluster
// CA cert and its SHA-256 fingerprint are returned so Step 3 agent
// bootstrap can perform TOFU validation of the CP server cert before
// submitting a CSR. The CA private key never leaves the server — the
// view projection in this package omits the key_pem column from
// ca_certs entirely.
//
// The endpoint is mounted outside any auth middleware: agents at
// bootstrap time have neither a Bearer JWT nor an mTLS client cert
// (the chicken-and-egg the join-token bootstrap exists to solve). The
// fingerprint distributed via the operator out of band (alongside
// the join token plaintext) is the only authentication channel.
package ca

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Handler bundles the dependencies of the /v1/ca routes.
type Handler struct {
	store *store.Store
	log   *slog.Logger
}

// New constructs the Handler.
func New(s *store.Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// clusterCAView mirrors components/schemas/ClusterCA in
// api/openapi/control-plane.yaml. KeyPem is intentionally absent —
// the projection guarantees the CA private key cannot leak via this
// endpoint.
type clusterCAView struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
}

// Get implements GET /v1/ca (anonymous). Returns 200 with the
// projected CA view on the happy path; 500 internal when no active
// CA row exists (the boot-time BootstrapClusterCA hook guarantees
// this row exists in production — a 500 here surfaces an operational
// invariant violation).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	row, err := h.store.Queries().GetActiveCACert(r.Context())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.log.WarnContext(r.Context(), "no active cluster CA row — bootstrap hook not run?",
				slog.String("path", r.URL.Path))
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "cluster CA not provisioned", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster CA lookup failed", nil)
		return
	}

	view := clusterCAView{
		CertPEM:           string(row.CertPem),
		FingerprintSHA256: hex.EncodeToString(row.FingerprintSha256),
		NotBefore:         row.NotBefore.UTC().Format(time.RFC3339Nano),
		NotAfter:          row.NotAfter.UTC().Format(time.RFC3339Nano),
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}
