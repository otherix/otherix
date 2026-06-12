// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
)

// clusterJoinServer stands up a TLS test server that advertises signCA on
// GET /v1/ca (the prefetch pin anchor), presents a serving cert signed by
// signCA (so the verified token POST passes), and answers POST
// /v1/cluster/join with the given CA cert/key PEM.
func clusterJoinServer(t *testing.T, signCA auth.ClusterCAResult, caCertPEM, caKeyPEM []byte) *httptest.Server {
	t.Helper()
	signCert, _, err := auth.ParseClusterCACert(signCA.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	signKeyAny, err := auth.ParseClusterCAKey(signCA.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := signKeyAny.(crypto.Signer)
	if !ok {
		t.Fatalf("CA key %T is not a crypto.Signer", signKeyAny)
	}
	leafCertPEM, leafKeyPEM, err := auth.GenerateReplicaCert(
		signCert, signer, nil, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	leaf, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ca", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cas": []map[string]string{{
				"cert_pem":           string(signCA.CertPEM),
				"fingerprint_sha256": hex.EncodeToString(signCA.Fingerprint),
			}},
			"signer_fingerprint_sha256": hex.EncodeToString(signCA.Fingerprint),
		})
	})
	mux.HandleFunc("/v1/cluster/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"ca_cert_pem": string(caCertPEM),
			"ca_key_pem":  string(caKeyPEM),
		})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func fingerprintHexOf(fp []byte) string { return hex.EncodeToString(fp) }

func TestFetchClusterCASuccess(t *testing.T) {
	ca, err := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	srv := clusterJoinServer(t, ca, ca.CertPEM, ca.KeyPEM)

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
	srv := clusterJoinServer(t, ca, ca.CertPEM, ca.KeyPEM)

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

// TestFetchClusterCA_TokenOnlySentOverVerifiedTLS proves the H4 contract: the
// cluster token is POSTed to /v1/cluster/join only after the target replica's
// serving cert verifies against the CA pinned by the /v1/ca prefetch. The MITM
// subtest advertises the REAL CA on /v1/ca (so the fingerprint pin passes) but
// presents a TLS cert signed by a DIFFERENT CA - the POST must abort before
// the handler is reached.
func TestFetchClusterCA_TokenOnlySentOverVerifiedTLS(t *testing.T) {
	ca, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	fpHex := hex.EncodeToString(ca.Fingerprint)

	// makeServer serves /v1/ca advertising the REAL cluster CA and
	// /v1/cluster/join returning its cert+key; the server's own TLS leaf is
	// signed by tlsSignCA (== ca in the verified case, a MITM CA otherwise).
	makeServer := func(t *testing.T, tlsSignCA auth.ClusterCAResult) (*httptest.Server, *bool) {
		t.Helper()
		signCert, _, err := auth.ParseClusterCACert(tlsSignCA.CertPEM)
		if err != nil {
			t.Fatalf("ParseClusterCACert: %v", err)
		}
		signKeyAny, err := auth.ParseClusterCAKey(tlsSignCA.KeyPEM)
		if err != nil {
			t.Fatalf("ParseClusterCAKey: %v", err)
		}
		signer, ok := signKeyAny.(crypto.Signer)
		if !ok {
			t.Fatalf("CA key %T is not a crypto.Signer", signKeyAny)
		}
		leafCertPEM, leafKeyPEM, err := auth.GenerateReplicaCert(
			signCert, signer, nil, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour, time.Now())
		if err != nil {
			t.Fatalf("GenerateReplicaCert: %v", err)
		}
		leaf, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
		if err != nil {
			t.Fatalf("X509KeyPair: %v", err)
		}

		reached := false
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/ca", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cas": []map[string]string{{
					"cert_pem":           string(ca.CertPEM),
					"fingerprint_sha256": fpHex,
				}},
				"signer_fingerprint_sha256": fpHex,
			})
		})
		mux.HandleFunc("/v1/cluster/join", func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ca_cert_pem": string(ca.CertPEM),
				"ca_key_pem":  string(ca.KeyPEM),
				"members":     []any{},
			})
		})
		srv := httptest.NewUnstartedServer(mux)
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv, &reached
	}

	t.Run("verified server reaches cluster-join", func(t *testing.T) {
		srv, reached := makeServer(t, ca)
		p := api.ClusterJoinFetchParams{
			CPURL:         srv.URL,
			Token:         "otx_join_whatever",
			CAFingerprint: fpHex,
			PeerURL:       "https://127.0.0.1:2380",
			Timeout:       10 * time.Second,
		}
		if _, err := api.FetchClusterCA(context.Background(), p, nil); err != nil {
			t.Fatalf("FetchClusterCA against verified server: %v", err)
		}
		if !*reached {
			t.Error("cluster-join handler not reached over verified TLS")
		}
	})

	t.Run("mitm tls cert aborts before cluster-join", func(t *testing.T) {
		mitm, err := auth.GenerateClusterCA(time.Now().Add(time.Second))
		if err != nil {
			t.Fatalf("GenerateClusterCA mitm: %v", err)
		}
		srv, reached := makeServer(t, mitm)
		p := api.ClusterJoinFetchParams{
			CPURL:         srv.URL,
			Token:         "otx_join_whatever",
			CAFingerprint: fpHex,
			PeerURL:       "https://127.0.0.1:2380",
			Timeout:       10 * time.Second,
		}
		if _, err := api.FetchClusterCA(context.Background(), p, nil); err == nil {
			t.Fatal("FetchClusterCA against MITM = nil error, want TLS verification failure")
		}
		if *reached {
			t.Error("cluster-join handler reached over UNVERIFIED TLS - cluster token leaked")
		}
	})
}

func TestFetchClusterCAKeyDoesNotMatchCert(t *testing.T) {
	ca1, _ := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	ca2, _ := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	// Serve ca1's cert with ca2's key: a tampered pair. TLS is signed by ca1
	// so the verified POST goes through; the key/cert pairing check must fail.
	srv := clusterJoinServer(t, ca1, ca1.CertPEM, ca2.KeyPEM)

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
