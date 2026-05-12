// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// stubAgentVerifier is a hand-rolled double for the AgentVerifier
// contract. Tests configure agent / err directly.
type stubAgentVerifier struct {
	agent *auth.Agent
	err   error

	gotFingerprint []byte
}

func (s *stubAgentVerifier) VerifyFingerprint(_ context.Context, fp []byte) (*auth.Agent, error) {
	s.gotFingerprint = append([]byte(nil), fp...)
	return s.agent, s.err
}

// captureAgent is a downstream handler that records the agent placed
// in the request context.
type captureAgent struct{ got *auth.Agent }

func (c *captureAgent) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.got = auth.AgentFromContext(r.Context())
}

func TestAgentMTLS_NoTLS(t *testing.T) {
	v := &stubAgentVerifier{}
	cap := &captureAgent{}
	h := middleware.AgentMTLS(v)(cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := decodeReason(t, rec.Result()); got != "cert_untrusted" {
		t.Errorf("reason = %q, want cert_untrusted", got)
	}
	if cap.got != nil {
		t.Errorf("downstream ran with no TLS")
	}
}

func TestAgentMTLS_NoPeerCertificates(t *testing.T) {
	v := &stubAgentVerifier{}
	cap := &captureAgent{}
	h := middleware.AgentMTLS(v)(cap)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.TLS = &tls.ConnectionState{}
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := decodeReason(t, rec.Result()); got != "cert_untrusted" {
		t.Errorf("reason = %q, want cert_untrusted", got)
	}
}

func TestAgentMTLS_FingerprintUnknown(t *testing.T) {
	v := &stubAgentVerifier{err: auth.ErrCertUnknown}
	cap := &captureAgent{}
	h := middleware.AgentMTLS(v)(cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newMockTLSRequest(t))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := decodeReason(t, rec.Result()); got != "cert_san_unknown" {
		t.Errorf("reason = %q, want cert_san_unknown", got)
	}
	wantFP, err := agentmock.NodeCertFingerprint()
	if err != nil {
		t.Fatalf("NodeCertFingerprint: %v", err)
	}
	if string(v.gotFingerprint) != string(wantFP) {
		t.Errorf("fingerprint passed to verifier mismatches embedded node.crt")
	}
}

func TestAgentMTLS_Revoked(t *testing.T) {
	v := &stubAgentVerifier{err: auth.ErrCertRevoked}
	cap := &captureAgent{}
	h := middleware.AgentMTLS(v)(cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newMockTLSRequest(t))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := decodeReason(t, rec.Result()); got != "cert_revoked" {
		t.Errorf("reason = %q, want cert_revoked", got)
	}
}

func TestAgentMTLS_DBError(t *testing.T) {
	v := &stubAgentVerifier{err: errors.New("db down")}
	cap := &captureAgent{}
	h := middleware.AgentMTLS(v)(cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newMockTLSRequest(t))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if cap.got != nil {
		t.Errorf("downstream ran on db error")
	}
}

func TestAgentMTLS_Success(t *testing.T) {
	want := &auth.Agent{NodeID: uuid.New(), CertFingerprint: []byte("fp")}
	v := &stubAgentVerifier{agent: want}
	cap := &captureAgent{}
	h := middleware.AgentMTLS(v)(cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newMockTLSRequest(t))

	if cap.got == nil {
		t.Fatal("downstream did not run")
	}
	if cap.got.NodeID != want.NodeID {
		t.Errorf("agent.NodeID = %v, want %v", cap.got.NodeID, want.NodeID)
	}

	// Sanity-check: middleware computed the fingerprint over node.crt's
	// raw DER bytes the same way agentmock.NodeCertFingerprint does.
	wantFP, err := agentmock.NodeCertFingerprint()
	if err != nil {
		t.Fatalf("NodeCertFingerprint: %v", err)
	}
	if string(v.gotFingerprint) != string(wantFP) {
		t.Errorf("fingerprint mismatch: middleware computed %x, helper says %x", v.gotFingerprint, wantFP)
	}
}

// newMockTLSRequest builds a request whose TLS state carries the
// embedded mock-agent node.crt as a peer certificate, so the
// fingerprint-extraction path runs against the canonical cert without
// standing up an actual TLS listener.
func newMockTLSRequest(t *testing.T) *http.Request {
	t.Helper()
	pemBytes, err := agentmock.NodeCertPEM()
	if err != nil {
		t.Fatalf("NodeCertPEM: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("could not decode node.crt PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	// Sanity: the test's view of the cert matches the helper's
	// fingerprint, otherwise the middleware would extract a
	// different fingerprint than every other test setup.
	if got := sha256.Sum256(cert.Raw); !equalBytes(got[:], mustFP(t)) {
		t.Fatal("node.crt parsed differently from agentmock.NodeCertFingerprint output")
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.TLS = &tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{cert},
	}
	return req
}

func mustFP(t *testing.T) []byte {
	t.Helper()
	fp, err := agentmock.NodeCertFingerprint()
	if err != nil {
		t.Fatalf("NodeCertFingerprint: %v", err)
	}
	return fp
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func decodeReason(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body response.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Details == nil {
		return ""
	}
	v, _ := body.Error.Details["reason"].(string)
	return v
}
