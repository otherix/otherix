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

// blobServeManager is the holder-side half of the cross-node blob pull: on
// Serve it reserves a port from the artifacts range, opens a peer-facing
// mutual-TLS listener (NODE-leaf client auth, NOT the CP-only identity) that
// streams the requested blob by digest, and returns the reachable endpoint plus
// an expiry. One listener per active serve; released on TTL expiry or earlier
// when Close tears the manager down at agent shutdown.
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

	// mu guards active and closed. Every live serve registers an entry in
	// active keyed by its reserved port; whichever of Close or the per-serve
	// TTL teardown runs first removes the entry and releases the port, and the
	// loser sees the entry already gone (no double-close / double-release).
	mu     sync.Mutex
	active map[int]*activeServe
	closed bool
}

// activeServe tracks one live peer serve listener so Close (agent shutdown) and
// the per-serve TTL teardown can each tear it down exactly once. tearDown is
// guarded by the manager mutex via the active map: the entry is removed before
// tearDown runs, so only one caller ever reaches it for a given serve.
type activeServe struct {
	srv   *http.Server
	port  int
	token string
	timer *time.Timer
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
		active:    map[int]*activeServe{},
	}, nil
}

// Serve opens a peer-facing serve listener for digest authorized by token and
// returns the reachable serve_endpoint (https://host:port) plus the RFC 3339
// expiry. It implements the blobs.BlobServer seam. The listener trusts only
// cluster-CA node leaves (RequireAndVerifyClientCert + the cluster CA pool); the
// per-op token gates which blob may be streamed.
func (m *blobServeManager) Serve(digest, token, _ string) (string, string, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", "", fmt.Errorf("blob serve: manager is closed")
	}
	m.mu.Unlock()

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

	// Register the live serve so Close and the TTL teardown share one
	// map-guarded entry; whichever fires first removes it and tears the
	// listener down, the other finds nothing. The timer fires the TTL path.
	as := &activeServe{srv: srv, port: port, token: token}
	as.timer = time.AfterFunc(m.ttl, func() {
		if m.takeServe(port) == nil {
			return // Close (or an earlier teardown) already claimed this serve.
		}
		m.tearDown(as)
		m.log.Info("blob serve listener expired", "port", port, "digest", digest)
	})

	m.mu.Lock()
	if m.closed {
		// Lost the race with Close between the guard above and here: undo the
		// listener we just opened rather than leaking it past shutdown.
		m.mu.Unlock()
		as.timer.Stop()
		_ = srv.Close()
		_ = ln.Close()
		m.ports.Release(port)
		m.verifier.drop(token)
		return "", "", fmt.Errorf("blob serve: manager is closed")
	}
	m.active[port] = as
	m.mu.Unlock()

	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			m.log.Warn("blob serve listener stopped with error", "port", port, "err", err)
		}
	}()

	expiresAt := time.Now().Add(m.ttl).UTC().Format(time.RFC3339)
	endpoint := "https://" + net.JoinHostPort(m.serveHost, strconv.Itoa(port))
	m.log.Info("blob serve listener opened", "endpoint", endpoint, "digest", digest, "expires_at", expiresAt)
	return endpoint, expiresAt, nil
}

// takeServe removes and returns the active serve for port under the mutex, or
// nil if it is already gone. It is the single claim point shared by the TTL
// teardown and Close: the caller that gets a non-nil entry owns tearing it down.
func (m *blobServeManager) takeServe(port int) *activeServe {
	m.mu.Lock()
	defer m.mu.Unlock()
	as, ok := m.active[port]
	if !ok {
		return nil
	}
	delete(m.active, port)
	return as
}

// StopByToken tears down the active serve whose per-op token matches (releasing
// its port and dropping the token), if any. A token with no live serve is a
// no-op. The CP-driven stop-serve endpoint uses this to release the port at pull
// completion instead of waiting out the token TTL. The 10-minute TTL teardown
// remains the fallback for a CP that never calls stop-serve.
//
// The token (not the digest) is the teardown key: two concurrent serves of the
// same digest to different consumers exist as two ports / two tokens, so a
// digest-keyed teardown would be ambiguous. The match-and-claim is atomic
// (takeServeByToken holds the mutex across both), so StopByToken is race-safe
// with the per-serve TTL teardown - both contend for the same single-claim
// active-map entry, so each serve is torn down exactly once, and a stale token
// can never claim a port a fresh serve has since reclaimed under a new token.
func (m *blobServeManager) StopByToken(token string) {
	as := m.takeServeByToken(token)
	if as == nil {
		return
	}
	m.tearDown(as)
}

// takeServeByToken finds and removes the active serve whose per-op token matches
// under a single hold of the mutex, returning it (or nil if none matches). Match
// and claim are atomic so the result is keyed strictly on the token: there is no
// window in which the TTL teardown could free the matched serve's port and a
// fresh Serve re-reserve it for a different token before the claim lands. It is
// the token-keyed sibling of takeServe (which claims by port) and shares the
// same single-claim contract with the TTL teardown and Close.
func (m *blobServeManager) takeServeByToken(token string) *activeServe {
	m.mu.Lock()
	defer m.mu.Unlock()
	for port, as := range m.active {
		if as.token == token {
			delete(m.active, port)
			return as
		}
	}
	return nil
}

// StopServe implements the blobs.BlobStopper seam: it tears down the serve
// identified by its per-op token. It is a thin alias for StopByToken so the
// handler can drive the manager without knowing the token-keyed naming.
func (m *blobServeManager) StopServe(token string) { m.StopByToken(token) }

// tearDown closes one serve's listener, releases its port, drops its primed
// token, and stops its TTL timer. It runs at most once per serve: the caller
// must first have claimed the entry via takeServe (or removed it under the
// mutex in Close), so the TTL path and Close never both reach it.
func (m *blobServeManager) tearDown(as *activeServe) {
	as.timer.Stop()
	_ = as.srv.Close()
	m.ports.Release(as.port)
	m.verifier.drop(as.token)
}

// Close tears down every still-active serve listener: it closes each server,
// releases its reserved port, drops its primed token, and stops its TTL timer.
// It is wired into the agent's graceful shutdown so in-flight peer serve
// listeners are not abandoned. Close is idempotent (a second call is a no-op)
// and race-safe with the per-serve TTL teardown: both claim a serve through the
// shared active map under the mutex, so each serve is torn down exactly once.
func (m *blobServeManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	taken := make([]*activeServe, 0, len(m.active))
	for port, as := range m.active {
		taken = append(taken, as)
		delete(m.active, port)
	}
	m.mu.Unlock()

	for _, as := range taken {
		m.tearDown(as)
	}
	return nil
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
