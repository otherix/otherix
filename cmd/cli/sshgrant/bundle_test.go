// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrant_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/sshgrant"
)

func sampleBundle() sshgrant.Bundle {
	return sshgrant.Bundle{
		Version:   sshgrant.BundleVersion,
		ServerURL: "https://cp.example:8443",
		Trust:     sshgrant.TrustWebPKI,
		Token:     "otx_sshgrant_secret",
		VMs: []sshgrant.BundleVM{
			{VM: "web01", Login: "deploy"},
			{VM: "db01", Login: "postgres"},
		},
	}
}

func TestBundleRoundTripsThroughBlob(t *testing.T) {
	want := sampleBundle()
	blob, err := sshgrant.EncodeBundle(want)
	if err != nil {
		t.Fatalf("EncodeBundle: %v", err)
	}
	if !strings.HasPrefix(blob, "otx_sshbundle_") {
		t.Errorf("blob = %q, want otx_sshbundle_ prefix", blob)
	}
	got, err := sshgrant.ParseBundle(blob)
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
	got, err := sshgrant.ParseBundle(string(raw))
	if err != nil {
		t.Fatalf("ParseBundle(json): %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("json parse mismatch (-want +got):\n%s", diff)
	}
}

func TestParseBundleRejectsUnknownVersion(t *testing.T) {
	b := sampleBundle()
	b.Version = "otherix-ssh-grant/v99"
	raw, _ := json.Marshal(b)
	if _, err := sshgrant.ParseBundle(string(raw)); err == nil {
		t.Fatal("expected an unsupported-version error")
	}
}

func TestParseBundleRequiresTokenAndServer(t *testing.T) {
	b := sampleBundle()
	b.Token = ""
	raw, _ := json.Marshal(b)
	if _, err := sshgrant.ParseBundle(string(raw)); err == nil {
		t.Fatal("expected a missing-token error")
	}
}

func TestResolveTrustCABundle(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	b := sampleBundle()
	b.Trust = sshgrant.TrustCABundle
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
	b.Trust = sshgrant.TrustPinPrefix + "abc123"
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
	b.Trust = sshgrant.TrustInsecure
	if _, _, insecure, err := b.ResolveTrust(); err != nil || !insecure {
		t.Errorf("insecure trust: insecure=%v err=%v", insecure, err)
	}
	b.Trust = sshgrant.TrustWebPKI
	caPEM, fp, insecure, err := b.ResolveTrust()
	if err != nil || caPEM != nil || fp != "" || insecure {
		t.Errorf("webpki trust should be empty: caPEM=%v fp=%q insecure=%v err=%v", caPEM, fp, insecure, err)
	}
}
