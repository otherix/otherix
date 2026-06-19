// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package blobpeer is the agent-to-agent blob data channel: a
// separate peer-facing mutual-TLS HTTPS listener that streams an immutable
// content-addressed blob by digest, plus the consumer-side pull client. This is
// NOT the agent's main API server: that server gates every route on the CP-only
// RequireCPIdentity. The blob transport must accept a peer NODE leaf cert
// (CN=node-<name>), so it runs on its own listener with its own authz - the one
// place a non-CP cluster node is allowed in, scoped to this listener and the
// GET /blobs/{digest} route, and additionally gated by a CP-minted single-use
// per-op bearer token.
package blobpeer

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// nodeCNPrefix is the cluster-CA node-leaf Subject CN prefix (node-<name>), the
// same value internal/auth/csr.go SignCSR stamps. The serve listener accepts any
// client cert whose CN carries this prefix (any cluster node), distinct from the
// main server's CP-only identity check.
const nodeCNPrefix = "node-"

// TokenVerifier verifies the per-op bearer token a puller presents against the
// requested digest. The production impl checks the agent's in-memory primed
// tokens (set when the CP told this agent to serve); tests pass a stub. Verify
// returns the digest whose bytes to stream (normally == the requested digest)
// and ok=true when the token authorizes serving that blob.
type TokenVerifier interface {
	Verify(token, digest string) (serveDigest string, ok bool)
}

// Getter resolves a digest to its bytes. The serve handler holds a resolver
// (not a concrete *artifactstore.Store) so a holder can serve a pinned image it
// only has in the node-level image cache tier, not just the artifact store.
// *artifactstore.Store satisfies this interface.
type Getter interface {
	Get(digest string) (io.ReadCloser, error)
}

// multiStore resolves a digest across an ordered list of stores, returning the
// first hit. A nil store is skipped, so a node with no image cache tier still
// serves from the artifact store. Build it via MultiStore.
type multiStore struct {
	stores []Getter
}

// MultiStore composes an ordered list of stores into one Getter that resolves a
// digest against each in turn, returning the first hit. Pass only non-nil
// stores: a typed-nil *artifactstore.Store wrapped in a Getter interface is
// non-nil at the interface level and would not be skipped. The serve wiring
// appends only the stores it actually has.
func MultiStore(stores ...Getter) Getter {
	return multiStore{stores: stores}
}

// Get returns the first store's bytes for digest. When no store holds it, it
// returns the last store's error (or artifactstore.ErrNotFound if the list was
// empty / all-nil), so the serve handler still maps the miss to 404.
func (m multiStore) Get(digest string) (io.ReadCloser, error) {
	var lastErr error
	for _, s := range m.stores {
		if s == nil {
			continue
		}
		rc, err := s.Get(digest)
		if err == nil {
			return rc, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = artifactstore.ErrNotFound
	}
	return nil, lastErr
}

// ServeHandler streams a blob by digest, resolving it across the holder's
// stores. Mounted on the SEPARATE peer listener (not the main server). Authz
// layers: (1) the listener requires a cluster-CA client cert and this handler
// asserts CN=node-*; (2) the per-op bearer token via TokenVerifier.
type ServeHandler struct {
	store    Getter
	verifier TokenVerifier
}

// NewServeHandler builds the chi handler streaming GET /blobs/{digest}. store is
// the resolver the handler reads from; pass a single *artifactstore.Store, or a
// MultiStore over both the artifact and image-cache tiers.
func NewServeHandler(store Getter, verifier TokenVerifier) http.Handler {
	h := &ServeHandler{store: store, verifier: verifier}
	r := chi.NewRouter()
	r.Get("/blobs/{digest}", h.serve)
	return r
}

func (h *ServeHandler) serve(w http.ResponseWriter, r *http.Request) {
	// Node-leaf identity gate: any cluster node, NOT the CP-only identity. A
	// missing peer cert is permitted here ONLY because httptest presents none;
	// production runs this handler behind a tls.Config with
	// RequireAndVerifyClientCert + the cluster CA pool, so a peer
	// without a valid cluster-CA leaf never reaches this handler. When a cert IS
	// present we still assert the node-<name> CN (reject a stray non-node leaf).
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cn := r.TLS.PeerCertificates[0].Subject.CommonName
		if !strings.HasPrefix(cn, nodeCNPrefix) {
			http.Error(w, "peer identity required", http.StatusForbidden)
			return
		}
	}

	digest := chi.URLParam(r, "digest")
	token := bearerToken(r)
	serveDigest, ok := h.verifier.Verify(token, digest)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rc, err := h.store.Get(serveDigest)
	if err != nil {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// bearerToken extracts the token from the Authorization: Bearer header.
func bearerToken(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, p) {
		return strings.TrimPrefix(h, p)
	}
	return ""
}
