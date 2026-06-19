// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobpeer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// holderCertDNSOnly builds an in-test CA plus a leaf cert valid ONLY for the
// DNS SAN dnsName (no IP SANs). It returns a client whose RootCAs trust the CA
// and a tls.Config carrying the leaf for the test TLS server. This mirrors the
// production failure mode: the holder's node leaf cert carries its node
// identity SAN (node-<name>.agents.otherix.local), NOT the inter-node IP a
// consumer dials it by.
func holderCertDNSOnly(t *testing.T, dnsName string) (*http.Client, *tls.Config) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "blobpeer-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName}, // DNS SAN only - deliberately NO IP SANs.
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: caPool, MinVersion: tls.VersionTLS13},
		},
	}
	srvTLS := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{leafDER},
			PrivateKey:  leafKey,
		}},
		MinVersion: tls.VersionTLS13,
	}
	return client, srvTLS
}

// TestPullServerNamePinning reproduces the cross-node pull TLS bug and
// proves the HolderIdentity ServerName pin fixes it. The test TLS server's leaf
// cert is valid only for the DNS SAN "holder.test"; the consumer dials it by
// 127.0.0.1 (the httptest URL host), which is NOT in the cert.
func TestPullServerNamePinning(t *testing.T) {
	const dnsName = "holder.test"
	content := []byte("identity-pinned-blob")
	d := digestOf(content)

	srcStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("src store: %v", err)
	}
	if err := srcStore.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	const token = "otx_pull_pin"
	client, srvTLS := holderCertDNSOnly(t, dnsName)

	h := NewServeHandler(srcStore, staticTokenVerifierStub{token: token, digest: d})
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	// WITHOUT HolderIdentity: Go verifies the cert against the dialed host
	// (127.0.0.1), which the cert does not cover -> TLS verification error.
	// This reproduces the smoke bug.
	dstStore1, _ := artifactstore.NewForTesting(t.TempDir())
	err = Pull(context.Background(), PullArgs{
		Endpoint: srv.URL, Digest: d, Token: token, Store: dstStore1, TLSClient: client,
	})
	if err == nil {
		t.Fatalf("Pull without HolderIdentity = nil, want TLS verification error (cert is not valid for 127.0.0.1)")
	}
	if dstStore1.Has(d) {
		t.Errorf("blob materialized despite TLS failure")
	}

	// WITH HolderIdentity pinned to the cert's DNS SAN: verification succeeds
	// regardless of the dialed IP.
	dstStore2, _ := artifactstore.NewForTesting(t.TempDir())
	if err := Pull(context.Background(), PullArgs{
		Endpoint: srv.URL, Digest: d, Token: token, Store: dstStore2, TLSClient: client,
		HolderIdentity: dnsName,
	}); err != nil {
		t.Fatalf("Pull with HolderIdentity = %v, want success", err)
	}
	if !dstStore2.Has(d) {
		t.Errorf("dst store missing blob after pinned pull")
	}
}
