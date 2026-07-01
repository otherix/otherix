// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrant_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/ingressgrant"
)

func sampleBundle() ingressgrant.Bundle {
	return ingressgrant.Bundle{
		Version:   ingressgrant.BundleVersion,
		ServerURL: "https://cp.example:8443",
		Trust:     ingressgrant.TrustWebPKI,
		Token:     "otx_ingressgrant_secret",
		VMs: []ingressgrant.BundleVM{
			{VM: "web01", Login: "deploy"},
			{VM: "db01", Login: "postgres"},
		},
	}
}

func TestBundleRoundTripsThroughBlob(t *testing.T) {
	want := sampleBundle()
	blob, err := ingressgrant.EncodeBundle(want)
	if err != nil {
		t.Fatalf("EncodeBundle: %v", err)
	}
	if !strings.HasPrefix(blob, "otx_ingressbundle_") {
		t.Errorf("blob = %q, want otx_ingressbundle_ prefix", blob)
	}
	got, err := ingressgrant.ParseBundle(blob)
	if err != nil {
		t.Fatalf("ParseBundle(blob): %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestBundleParsesRawJSON(t *testing.T) {
	want := sampleBundle()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ingressgrant.ParseBundle(string(raw))
	if err != nil {
		t.Fatalf("ParseBundle(json): %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("json parse mismatch (-want +got):\n%s", diff)
	}
}

func TestParseBundleRejectsUnknownVersion(t *testing.T) {
	b := sampleBundle()
	b.Version = "otherix-ingress-grant/v99"
	raw, _ := json.Marshal(b)
	if _, err := ingressgrant.ParseBundle(string(raw)); err == nil {
		t.Fatal("expected an unsupported-version error")
	}
}

func TestParseBundleRequiresTokenAndServer(t *testing.T) {
	b := sampleBundle()
	b.Token = ""
	raw, _ := json.Marshal(b)
	if _, err := ingressgrant.ParseBundle(string(raw)); err == nil {
		t.Fatal("expected a missing-token error")
	}
}

func TestResolveTrustCABundle(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	b := sampleBundle()
	b.Trust = ingressgrant.TrustCABundle
	b.CACertPEM = base64.StdEncoding.EncodeToString([]byte(pem))

	caPEM, fp, insecure, err := b.ResolveTrust()
	if err != nil {
		t.Fatalf("ResolveTrust: %v", err)
	}
	if string(caPEM) != pem {
		t.Errorf("caPEM = %q, want %q", caPEM, pem)
	}
	if fp != "" || insecure {
		t.Errorf("ca-bundle trust leaked fp=%q insecure=%v", fp, insecure)
	}
}

func TestResolveTrustPin(t *testing.T) {
	b := sampleBundle()
	b.Trust = ingressgrant.TrustPinPrefix + "abc123"
	caPEM, fp, insecure, err := b.ResolveTrust()
	if err != nil {
		t.Fatalf("ResolveTrust: %v", err)
	}
	if fp != "abc123" {
		t.Errorf("fingerprint = %q, want abc123", fp)
	}
	if caPEM != nil || insecure {
		t.Errorf("pin trust leaked caPEM=%v insecure=%v", caPEM, insecure)
	}
}

func TestResolveTrustInsecureAndWebPKI(t *testing.T) {
	b := sampleBundle()
	b.Trust = ingressgrant.TrustInsecure
	if _, _, insecure, err := b.ResolveTrust(); err != nil || !insecure {
		t.Errorf("insecure trust: insecure=%v err=%v", insecure, err)
	}
	b.Trust = ingressgrant.TrustWebPKI
	caPEM, fp, insecure, err := b.ResolveTrust()
	if err != nil || caPEM != nil || fp != "" || insecure {
		t.Errorf("webpki trust should be empty: caPEM=%v fp=%q insecure=%v err=%v", caPEM, fp, insecure, err)
	}
}
