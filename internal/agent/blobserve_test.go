// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/migration"
)

// sha256Hex returns the lowercase hex sha256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestServeTokenStore_VerifyScopesToPrimedDigest pins the C1 token gate: a
// primed token authorizes serving exactly its digest and nothing else, an
// unprimed token is rejected, and a dropped token (serve expiry) no longer
// verifies. The per-listener cluster-CA client-cert gate is enforced separately
// in server.go's tls.Config; this covers the token half.
func TestServeTokenStore_VerifyScopesToPrimedDigest(t *testing.T) {
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	s := newServeTokenStore()
	s.prime("tok-1", digestA)

	if got, ok := s.Verify("tok-1", digestA); !ok || got != digestA {
		t.Errorf("Verify(tok-1, A) = (%q, %v), want (%q, true)", got, ok, digestA)
	}
	if _, ok := s.Verify("tok-1", digestB); ok {
		t.Errorf("Verify(tok-1, B) = ok, want token may not serve a different digest")
	}
	if _, ok := s.Verify("unprimed", digestA); ok {
		t.Errorf("Verify(unprimed, A) = ok, want unprimed token rejected")
	}

	s.drop("tok-1")
	if _, ok := s.Verify("tok-1", digestA); ok {
		t.Errorf("Verify(tok-1, A) after drop = ok, want dropped token rejected")
	}
}

// newTestServeManager builds a blobServeManager over a single-port allocator
// and a self-signed node leaf, with a long TTL so the per-serve TTL teardown
// never fires during the test (Close is the path under test). It returns the
// manager and the single port the allocator hands out.
func newTestServeManager(t *testing.T) (*blobServeManager, int) {
	t.Helper()
	store, err := artifactstore.NewForTesting(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "node-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	nodeCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}

	port := freeTCPPort(t)
	m := &blobServeManager{
		store:     store,
		ports:     migration.NewPortAllocator(port, port),
		serveHost: "127.0.0.1",
		nodeCert:  nodeCert,
		clientCAs: &tls.Config{ClientCAs: pool, MinVersion: tls.VersionTLS13},
		ttl:       time.Hour,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		verifier:  newServeTokenStore(),
		active:    map[int]*activeServe{},
	}
	return m, port
}

// freeTCPPort returns a TCP port the OS just confirmed is free, by binding
// 127.0.0.1:0 and immediately releasing it. The tiny window between release and
// reuse is far less collision-prone than a fixed well-known port (which flaked
// in CI when the runner already held it).
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return port
}

// TestBlobServeManager_CloseReleasesAndIsIdempotent verifies Close tears down a
// live serve listener (releasing its port back to the allocator and dropping its
// primed token), and a second Close is a no-op. With a one-port allocator, the
// only proof the port was released is that a fresh Reserve hands the same port
// back.
func TestBlobServeManager_CloseReleasesAndIsIdempotent(t *testing.T) {
	m, port := newTestServeManager(t)

	if _, _, err := m.Serve(strings.Repeat("a", 64), "tok-1", ""); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// Port is reserved (allocator exhausted) and the token is primed.
	if _, err := m.ports.Reserve(); err == nil {
		t.Fatalf("Reserve succeeded while serve active, want exhausted")
	}
	if _, ok := m.verifier.Verify("tok-1", strings.Repeat("a", 64)); !ok {
		t.Fatalf("token not primed during active serve")
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Port re-reservable (released) and token dropped.
	got, err := m.ports.Reserve()
	if err != nil {
		t.Fatalf("Reserve after Close: %v, want port released", err)
	}
	if got != port {
		t.Errorf("Reserve after Close = %d, want %d (the released port)", got, port)
	}
	m.ports.Release(got)
	if _, ok := m.verifier.Verify("tok-1", strings.Repeat("a", 64)); ok {
		t.Errorf("token still primed after Close, want dropped")
	}

	// Second Close is a no-op.
	if err := m.Close(); err != nil {
		t.Errorf("second Close = %v, want nil (idempotent)", err)
	}

	// Serve after Close is rejected (manager closed).
	if _, _, err := m.Serve(strings.Repeat("b", 64), "tok-2", ""); err == nil {
		t.Errorf("Serve after Close = nil error, want rejected")
	}
}

// TestBlobServeManager_StopByTokenReleasesAndIsIdempotent verifies StopByToken
// tears down the serve whose per-op token matches (releasing its port back to
// the allocator and dropping its primed token), and a second StopByToken for the
// same token is a no-op. The token (not the digest) is the teardown key so two
// concurrent serves of the same digest to different consumers stay
// disambiguated.
func TestBlobServeManager_StopByTokenReleasesAndIsIdempotent(t *testing.T) {
	m, port := newTestServeManager(t)

	if _, _, err := m.Serve(strings.Repeat("a", 64), "tok-1", ""); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// Port is reserved (allocator exhausted) and the token is primed.
	if _, err := m.ports.Reserve(); err == nil {
		t.Fatalf("Reserve succeeded while serve active, want exhausted")
	}
	if _, ok := m.verifier.Verify("tok-1", strings.Repeat("a", 64)); !ok {
		t.Fatalf("token not primed during active serve")
	}

	m.StopByToken("tok-1")

	// Port re-reservable (released) and token dropped.
	got, err := m.ports.Reserve()
	if err != nil {
		t.Fatalf("Reserve after StopByToken: %v, want port released", err)
	}
	if got != port {
		t.Errorf("Reserve after StopByToken = %d, want %d (the released port)", got, port)
	}
	m.ports.Release(got)
	if _, ok := m.verifier.Verify("tok-1", strings.Repeat("a", 64)); ok {
		t.Errorf("token still primed after StopByToken, want dropped")
	}

	// Second StopByToken for the same token is a no-op (serve already gone), and
	// it must not release the port a second time.
	m.StopByToken("tok-1")
	if _, err := m.ports.Reserve(); err != nil {
		t.Fatalf("Reserve after second StopByToken: %v, want still one free port", err)
	}

	// StopByToken for an unknown token is a no-op.
	m.StopByToken("never-served")
}

// TestBlobInventoryAdapter_NodeBlobs confirms the heartbeat BlobLister adapter
// maps the artifact store's blob entries to heartbeat.BlobReport and reports
// ok=true (including the empty inventory case).
func TestBlobInventoryAdapter_NodeBlobs(t *testing.T) {
	store, err := artifactstore.NewForTesting(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	a := blobInventoryAdapter{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	reports, ok := a.NodeBlobs()
	if !ok {
		t.Fatalf("NodeBlobs ok = false on empty store, want true")
	}
	if len(reports) != 0 {
		t.Errorf("NodeBlobs len = %d, want 0 on empty store", len(reports))
	}

	digest := mustPutBlob(t, store, []byte("hello blob"))
	reports, ok = a.NodeBlobs()
	if !ok {
		t.Fatalf("NodeBlobs ok = false, want true")
	}
	want := heartbeat.BlobReport{Digest: digest, SizeBytes: int64(len("hello blob"))}
	if len(reports) != 1 || reports[0] != want {
		t.Errorf("NodeBlobs = %+v, want [%+v]", reports, want)
	}
}

// mustPutBlob writes content into store under its sha256 digest and returns the
// digest.
func mustPutBlob(t *testing.T, store *artifactstore.Store, content []byte) string {
	t.Helper()
	digest := sha256Hex(content)
	if err := store.Put(digest, strings.NewReader(string(content))); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	return digest
}
