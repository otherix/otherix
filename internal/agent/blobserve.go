// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/blobpeer"
	"github.com/otherix/otherix/internal/agent/migration"
)

// serveTokenTTL bounds the lifetime of a peer serve listener: the CP-minted
// per-op token is single-use and short-lived, and the consumer pull runs
// promptly after the CP receives the serve_endpoint. The listener is torn down
// when the TTL elapses (or earlier on Close), freeing the reserved port and
// dropping the primed token so a replay finds nothing to serve.
const serveTokenTTL = 10 * time.Minute

// blobServeManager is the holder-side half of the slice-C1 blob pull: on
// Serve it reserves a port from the artifacts range, opens a peer-facing
// mutual-TLS listener (NODE-leaf client auth, NOT the CP-only identity) that
// streams the requested blob by digest, and returns the reachable endpoint plus
// an expiry. One listener per active serve; released on TTL.
//
// SECURITY: the listener's tls.Config sets ClientAuth =
// RequireAndVerifyClientCert with the cluster CA pool, so a peer without a valid
// cluster-CA node leaf can never reach blobpeer.ServeHandler (whose CN check is
// permissive when no client cert is present). This listener is the enforcement
// boundary for that handler.
type blobServeManager struct {
	store     *artifactstore.Store
	ports     *migration.PortAllocator
	serveHost string
	nodeCert  tls.Certificate
	clientCAs *tls.Config // carries ClientCAs (the cluster CA pool) from loadTLS
	ttl       time.Duration
	log       *slog.Logger

	verifier *serveTokenStore
}

// newBlobServeManager builds a serve manager. baseTLS is the *tls.Config the
// main mTLS server uses (from loadTLS): its ClientCAs (cluster CA pool) and
// Certificates (node leaf) are reused so the blob listener trusts exactly the
// same cluster identities. serveHost is the reachable host (migration/advertised
// host) the returned serve_endpoint advertises.
func newBlobServeManager(store *artifactstore.Store, ports *migration.PortAllocator, serveHost string, baseTLS *tls.Config, log *slog.Logger) (*blobServeManager, error) {
	if len(baseTLS.Certificates) == 0 {
		return nil, fmt.Errorf("blob serve: base tls config carries no node leaf certificate")
	}
	if baseTLS.ClientCAs == nil {
		return nil, fmt.Errorf("blob serve: base tls config carries no cluster CA pool")
	}
	return &blobServeManager{
		store:     store,
		ports:     ports,
		serveHost: serveHost,
		nodeCert:  baseTLS.Certificates[0],
		clientCAs: baseTLS,
		ttl:       serveTokenTTL,
		log:       log,
		verifier:  newServeTokenStore(),
	}, nil
}

// Serve opens a peer-facing serve listener for digest authorized by token and
// returns the reachable serve_endpoint (https://host:port) plus the RFC 3339
// expiry. It implements the blobs.BlobServer seam. The listener trusts only
// cluster-CA node leaves (RequireAndVerifyClientCert + the cluster CA pool); the
// per-op token gates which blob may be streamed.
func (m *blobServeManager) Serve(digest, token, _ string) (string, string, error) {
	port, err := m.ports.Reserve()
	if err != nil {
		return "", "", fmt.Errorf("blob serve: reserve port: %w", err)
	}

	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.ports.Release(port)
		return "", "", fmt.Errorf("blob serve: listen %s: %v", addr, err)
	}

	m.verifier.prime(token, digest)

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.nodeCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    m.clientCAs.ClientCAs,
	}
	srv := &http.Server{
		Handler:           blobpeer.NewServeHandler(m.store, m.verifier),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			m.log.Warn("blob serve listener stopped with error", "port", port, "err", err)
		}
	}()

	// Schedule teardown on TTL: close the server, release the port, drop the
	// primed token so a replay after expiry finds nothing.
	go func() {
		timer := time.NewTimer(m.ttl)
		defer timer.Stop()
		<-timer.C
		_ = srv.Close()
		m.ports.Release(port)
		m.verifier.drop(token)
		m.log.Info("blob serve listener expired", "port", port, "digest", digest)
	}()

	expiresAt := time.Now().Add(m.ttl).UTC().Format(time.RFC3339)
	endpoint := "https://" + net.JoinHostPort(m.serveHost, strconv.Itoa(port))
	m.log.Info("blob serve listener opened", "endpoint", endpoint, "digest", digest, "expires_at", expiresAt)
	return endpoint, expiresAt, nil
}

// serveTokenStore is the in-agent TokenVerifier primed by Serve: it maps a
// CP-handed per-op token to the digest it authorizes serving. It implements
// blobpeer.TokenVerifier. Minimal for C1: a token authorizes exactly its primed
// digest; the per-listener cluster-CA client-cert gate (Serve's tls.Config) is
// the node-identity boundary.
type serveTokenStore struct {
	mu     sync.Mutex
	tokens map[string]string // token -> digest
}

func newServeTokenStore() *serveTokenStore {
	return &serveTokenStore{tokens: map[string]string{}}
}

// prime records that token authorizes serving digest.
func (s *serveTokenStore) prime(token, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = digest
}

// drop removes a token (called on serve expiry).
func (s *serveTokenStore) drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// Verify reports whether token authorizes serving requestedDigest. It returns
// the digest to stream (the primed digest) and ok=true only when the token is
// primed AND matches the requested digest - a token may never be used to pull a
// different blob than it was minted for.
func (s *serveTokenStore) Verify(token, requestedDigest string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest, ok := s.tokens[token]
	if !ok || digest != requestedDigest {
		return "", false
	}
	return digest, true
}
