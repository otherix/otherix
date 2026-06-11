// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
)

// clusterJoinServer stands up a TLS test server that answers POST
// /v1/cluster/join with the given CA cert/key PEM. Its own serving cert is
// self-signed (not the cluster CA), exercising the joiner's TOFU path.
func clusterJoinServer(t *testing.T, caCertPEM, caKeyPEM []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/cluster/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"ca_cert_pem": string(caCertPEM),
			"ca_key_pem":  string(caKeyPEM),
		})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fingerprintHexOf(fp []byte) string { return hex.EncodeToString(fp) }

func TestFetchClusterCASuccess(t *testing.T) {
	ca, err := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	srv := clusterJoinServer(t, ca.CertPEM, ca.KeyPEM)

	got, err := api.FetchClusterCA(context.Background(), api.ClusterJoinFetchParams{
		CPURL:         srv.URL,
		Token:         "otx_join_whatever",
		CAFingerprint: fingerprintHexOf(ca.Fingerprint),
		PeerURL:       "https://127.0.0.1:2380",
		Timeout:       5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("FetchClusterCA: %v", err)
	}
	if !bytes.Equal(got.CA.CertPEM, ca.CertPEM) {
		t.Errorf("fetched cert PEM differs from served CA")
	}
	if !bytes.Equal(got.CA.Fingerprint, ca.Fingerprint) {
		t.Errorf("fetched fingerprint = %x, want %x", got.CA.Fingerprint, ca.Fingerprint)
	}
	if len(got.CA.KeyPEM) == 0 {
		t.Error("fetched key PEM is empty")
	}
}

func TestFetchClusterCAFingerprintMismatch(t *testing.T) {
	ca, _ := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	srv := clusterJoinServer(t, ca.CertPEM, ca.KeyPEM)

	// Flip the first hex character to one guaranteed different from the real
	// fingerprint so the pin always mismatches. A hardcoded "00" prefix
	// collides with the real fingerprint ~1/256 of the time (when the random
	// cert serial yields a fingerprint already starting with 0x00), which
	// made this test flaky.
	wrong := []byte(fingerprintHexOf(ca.Fingerprint))
	if wrong[0] == '0' {
		wrong[0] = '1'
	} else {
		wrong[0] = '0'
	}
	_, err := api.FetchClusterCA(context.Background(), api.ClusterJoinFetchParams{
		CPURL:         srv.URL,
		Token:         "otx_join_whatever",
		CAFingerprint: string(wrong),
		Timeout:       5 * time.Second,
	}, nil)
	if err == nil {
		t.Fatal("expected fingerprint-mismatch error, got nil")
	}
}

func TestFetchClusterCAKeyDoesNotMatchCert(t *testing.T) {
	ca1, _ := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	ca2, _ := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	// Serve ca1's cert with ca2's key: a tampered pair.
	srv := clusterJoinServer(t, ca1.CertPEM, ca2.KeyPEM)

	_, err := api.FetchClusterCA(context.Background(), api.ClusterJoinFetchParams{
		CPURL:         srv.URL,
		Token:         "otx_join_whatever",
		CAFingerprint: fingerprintHexOf(ca1.Fingerprint),
		Timeout:       5 * time.Second,
	}, nil)
	if err == nil {
		t.Fatal("expected key/cert mismatch error, got nil")
	}
}
