// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
)

func tlsStateWithCN(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		},
	}
}

func TestRequireCPIdentity(t *testing.T) {
	tests := []struct {
		name       string
		tlsState   *tls.ConnectionState
		wantNext   bool
		wantStatus int
	}{
		{name: "cp identity passes", tlsState: tlsStateWithCN(auth.CPCertCommonName), wantNext: true, wantStatus: http.StatusOK},
		{name: "node identity rejected", tlsState: tlsStateWithCN("node-evil"), wantNext: false, wantStatus: http.StatusForbidden},
		{name: "nil tls rejected", tlsState: nil, wantNext: false, wantStatus: http.StatusForbidden},
		{name: "empty peer certs rejected", tlsState: &tls.ConnectionState{}, wantNext: false, wantStatus: http.StatusForbidden},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			})
			h := middleware.RequireCPIdentity(log)(next)

			req := httptest.NewRequest(http.MethodGet, "/v1/vms", nil)
			req.TLS = tt.tlsState
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if nextCalled != tt.wantNext {
				t.Errorf("nextCalled = %v, want %v", nextCalled, tt.wantNext)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if !tt.wantNext {
				var body struct {
					Error struct {
						Code    string         `json:"code"`
						Details map[string]any `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if got, want := body.Error.Code, "permission_denied"; got != want {
					t.Errorf("error.code = %q, want %q", got, want)
				}
				if got, want := body.Error.Details["reason"], "cp_identity_required"; got != want {
					t.Errorf("error.details.reason = %v, want %q", got, want)
				}
			}
		})
	}
}

// TestRequireCPIdentityRejectionBodiesIdentical guards against an identity
// oracle: the wrong-CN rejection body must be byte-identical to the no-cert
// rejection body so a caller cannot distinguish the two cases.
func TestRequireCPIdentityRejectionBodiesIdentical(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler called, want rejection")
	})
	h := middleware.RequireCPIdentity(log)(next)

	wrongCNReq := httptest.NewRequest(http.MethodGet, "/v1/vms", nil)
	wrongCNReq.TLS = tlsStateWithCN("node-evil")
	wrongCNRec := httptest.NewRecorder()
	h.ServeHTTP(wrongCNRec, wrongCNReq)

	noCertReq := httptest.NewRequest(http.MethodGet, "/v1/vms", nil)
	noCertReq.TLS = nil
	noCertRec := httptest.NewRecorder()
	h.ServeHTTP(noCertRec, noCertReq)

	if wrongCNRec.Code != http.StatusForbidden {
		t.Errorf("wrong-CN status = %d, want %d", wrongCNRec.Code, http.StatusForbidden)
	}
	if noCertRec.Code != http.StatusForbidden {
		t.Errorf("no-cert status = %d, want %d", noCertRec.Code, http.StatusForbidden)
	}
	wrongCNBody := wrongCNRec.Body.Bytes()
	noCertBody := noCertRec.Body.Bytes()
	if !bytes.Equal(wrongCNBody, noCertBody) {
		t.Errorf("rejection bodies differ:\nwrong-CN: %s\nno-cert:  %s", wrongCNBody, noCertBody)
	}
}
