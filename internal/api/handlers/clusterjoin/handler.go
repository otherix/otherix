// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package clusterjoin hosts the anonymous POST /v1/cluster/join endpoint:
// the redemption pathway for a joining control-plane replica. The replica
// presents a kind=cluster join token and receives the cluster CA cert +
// private key, so it can provision its own peer (Raft) mTLS material on
// disk before its etcd member starts.
//
// This is the higher-privilege sibling of /v1/nodes/join: a node join
// returns a leaf cert, a cluster join hands over the CA key. The privilege
// is intrinsic to the token kind (fixed at mint), so the endpoint accepts
// only kind=cluster tokens - a stolen node token can never yield the CA
// key. Anonymous + token-gated + TLS (the joiner pins the CA fingerprint
// out of band, TOFU, like an agent bootstrap).
package clusterjoin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the /v1/cluster/join handler depends on: the
// active-CA lookup (loaded per request) and the cluster-token redemption seam.
// *etcdstore.Store satisfies it.
type Store interface {
	ActiveCACert(ctx context.Context) (store.CaCert, error)
	RedeemClusterJoinToken(ctx context.Context, p store.RedeemClusterJoinTokenParams) (store.RedeemClusterJoinResult, error)
}

// Handler bundles dependencies for the /v1/cluster/join route.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs the Handler.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// joinRequest is the body of POST /v1/cluster/join. The plaintext cluster
// token is the bearer credential.
type joinRequest struct {
	Token string `json:"token"`
}

// joinResponse returns the cluster CA cert + private key. The key is the
// sensitive payload - it lets the joining replica sign its own peer cert and
// (as a CP node) participate fully in the cluster PKI.
type joinResponse struct {
	CACertPEM string `json:"ca_cert_pem"`
	CAKeyPEM  string `json:"ca_key_pem"`
}

// Join implements POST /v1/cluster/join (anonymous). Validates the token,
// redeems it (kind=cluster only), and returns the cluster CA cert + key.
func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || !auth.IsJoinTokenFormat(token) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "token is required and must be a join token", nil)
		return
	}

	caRow, err := h.store.ActiveCACert(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "load active CA", slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster CA not available", nil)
		return
	}

	_, redeemErr := h.store.RedeemClusterJoinToken(r.Context(), store.RedeemClusterJoinTokenParams{
		TokenHash: auth.HashToken(token),
		SourceIP:  extractSourceIP(r),
	})
	if redeemErr != nil {
		h.writeRedeemError(w, r, token, redeemErr)
		return
	}

	h.log.InfoContext(r.Context(), "cluster join redeemed",
		slog.String("token_hash_prefix", tokenHashPrefix(token)),
		slog.String("ca_fingerprint_sha256", hex.EncodeToString(caRow.FingerprintSha256)))

	response.WriteJSON(w, r, http.StatusCreated, joinResponse{
		CACertPEM: string(caRow.CertPem),
		CAKeyPEM:  string(caRow.KeyPem),
	})
}

// writeRedeemError maps a redemption sentinel to its HTTP envelope. Unknown /
// expired / wrong-kind collapse to a single 401 (no info leak); the slog WARN
// field distinguishes them for forensics.
func (h *Handler) writeRedeemError(w http.ResponseWriter, r *http.Request, tokenPlain string, err error) {
	prefix := tokenHashPrefix(tokenPlain)
	switch {
	case errors.Is(err, store.ErrJoinTokenInvalid):
		h.log.WarnContext(r.Context(), "cluster join redemption rejected",
			slog.String("reason", "token_invalid"),
			slog.String("token_hash_prefix", prefix))
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "token not recognized or expired", nil)
	case errors.Is(err, store.ErrJoinTokenExhausted):
		h.log.WarnContext(r.Context(), "cluster join redemption rejected",
			slog.String("reason", "token_exhausted"),
			slog.String("token_hash_prefix", prefix))
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "token max_uses exceeded", nil)
	default:
		h.log.ErrorContext(r.Context(), "cluster join redemption failed",
			slog.String("token_hash_prefix", prefix),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal server error", nil)
	}
}

// tokenHashPrefix returns the first 8 hex chars of sha256(plaintext) as a
// forensics tag - enough to correlate a failure stream without leaking the
// token or its full hash.
func tokenHashPrefix(plaintext string) string {
	if plaintext == "" {
		return "(empty)"
	}
	full := hex.EncodeToString(auth.HashToken(plaintext))
	if len(full) < 8 {
		return full
	}
	return full[:8]
}

// extractSourceIP parses r.RemoteAddr into an optional *netip.Addr for the
// consumption audit row. Proxy headers are not consulted.
func extractSourceIP(r *http.Request) *netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
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
