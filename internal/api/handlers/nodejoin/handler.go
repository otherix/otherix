// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package nodejoin hosts the anonymous /v1/nodes/join endpoint that
// closes the redemption pathway of the join-token bootstrap
// landing. An agent process submits its CSR + a plaintext
// join token; the CP validates the token, signs the CSR using the
// cluster CA, creates / reuses the node row, and returns the issued
// cert + the active CA cert in a single response ("token bundle"
// pattern).
//
// The endpoint is mounted outside any auth middleware — bootstrap-
// time agents have neither a Bearer JWT nor an mTLS client cert
// (the chicken-and-egg this whole landing exists to solve). Token
// plaintext in the body is the bearer credential; TLS protects
// transport (operator pinned the CA fingerprint out of band via the
// token bundle).
//
// Race safety is achieved via SELECT FOR UPDATE on the join_tokens
// row (see internal/store/queries/join_tokens.sql:GetJoinTokenByHashForUpdate)
// rather than SERIALIZABLE isolation — concurrent redemptions queue
// at the lock and observe each other's consumption rows transactionally.
package nodejoin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the /v1/nodes/join handler depends on:
// the active-CA lookup loaded per request and the atomic join-token
// redemption seam. *etcdstore.Store satisfies it; depending on the
// interface rather than the concrete store narrows the handler's storage
// dependency to the methods it uses and lets tests substitute a fake.
type Store interface {
	ActiveCACert(ctx context.Context) (store.CaCert, error)
	RedeemJoinToken(ctx context.Context, p store.RedeemJoinTokenParams, sign func(node store.Node) (store.IssuedCert, error)) (store.RedeemJoinTokenResult, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles dependencies for the /v1/nodes/join routes.
// Per-request CA load (mirrors /v1/ca handler pattern) means the
// handler holds nothing CA-specific — the store is consulted on each
// request to pick up the active CA row.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs the Handler.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// Join implements POST /v1/nodes/join (anonymous). Validates the
// request body + CSR, orchestrates the atomic redemption transaction,
// and returns the token bundle (signed cert + CA) on success.
//
// All redemption failures emit a WARN slog with a token-hash-prefix
// (first 8 chars of hex sha256) tag so forensics can correlate
// failures without leaking the plaintext token to logs.
func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	if err := req.validate(); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, validationMessage(err), nil)
		return
	}

	csr, err := auth.ValidateCSR([]byte(req.CSRPEM))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	// Per-request CA load mirrors /v1/ca pattern.
	caBundle, err := h.loadCA(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "load active CA", slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster CA not available", nil)
		return
	}

	sourceIP := extractSourceIP(r)
	result, redeemErr := h.redeem(r.Context(), req, csr, caBundle, sourceIP)
	if redeemErr != nil {
		h.writeRedeemError(w, r, req.Token, redeemErr)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, joinResponse{
		NodeID:    result.nodeID,
		CertPEM:   string(result.certPEM),
		CACertPEM: string(caBundle.certPEM),
	})
}

// writeRedeemError maps a redemption sentinel to its HTTP envelope
// and logs the failure with a tokenized hash prefix. Token-unknown /
// expired / exhausted all collapse to identical 401 envelopes (no
// info leak) — the slog WARN field distinguishes them for forensics.
func (h *Handler) writeRedeemError(w http.ResponseWriter, r *http.Request, tokenPlain string, err error) {
	prefix := tokenHashPrefix(tokenPlain)

	switch {
	case errors.Is(err, store.ErrJoinTokenInvalid):
		h.log.WarnContext(r.Context(), "join token redemption rejected",
			slog.String("reason", "token_invalid"),
			slog.String("token_hash_prefix", prefix))
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "token not recognized or expired", nil)
	case errors.Is(err, store.ErrJoinTokenExhausted):
		h.log.WarnContext(r.Context(), "join token redemption rejected",
			slog.String("reason", "token_exhausted"),
			slog.String("token_hash_prefix", prefix))
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "token max_uses exceeded", nil)
	case errors.Is(err, store.ErrJoinNodeNameMismatch):
		h.log.WarnContext(r.Context(), "join token redemption rejected",
			slog.String("reason", "node_name_mismatch"),
			slog.String("token_hash_prefix", prefix))
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"node_name does not match token's pre-binding",
			map[string]any{"field": "node_name"})
	case errors.Is(err, store.ErrJoinNodeNameTaken):
		h.log.WarnContext(r.Context(), "join token redemption rejected",
			slog.String("reason", "node_name_taken"),
			slog.String("token_hash_prefix", prefix))
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"node_name already has an active cert",
			map[string]any{"field": "node_name"})
	default:
		h.log.ErrorContext(r.Context(), "join token redemption failed",
			slog.String("token_hash_prefix", prefix),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal server error", nil)
	}
}

// tokenHashPrefix returns the first 8 hex chars of sha256(plaintext).
// Used as a forensics tag in slog records — sufficient to correlate a
// failure stream without revealing the plaintext (or even the full hash,
// which could enable a fingerprint-style replay against a malicious
// logging sink).
func tokenHashPrefix(plaintext string) string {
	if plaintext == "" {
		return "(empty)"
	}
	hash := auth.HashToken(plaintext)
	full := hex.EncodeToString(hash)
	if len(full) < 8 {
		return full
	}
	return full[:8]
}

// extractSourceIP parses r.RemoteAddr (host:port format) into an
// optional *netip.Addr suitable for the source_ip column. Proxy
// headers (X-Forwarded-For) are NOT consulted in this slice —
// the contract is intentionally simple, deferring trusted-proxy-chain
// handling to a future iteration.
//
// Returns nil when the address cannot be parsed (test harnesses,
// unusual transports). The nullable column accepts NULL.
func extractSourceIP(r *http.Request) *netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Sometimes RemoteAddr is a bare host (test harnesses); try the
		// raw value before giving up.
		host = r.RemoteAddr
	}
	if host == "" {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}
