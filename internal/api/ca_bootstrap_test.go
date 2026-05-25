// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/otherix/otherix/internal/api"
)

// freshStoreNoCA returns a fresh Store and clears the ca_certs
// table so BootstrapClusterCA exercises the generate path. Tests
// that simply need an active CA can use the e2eHarness directly
// (which calls BootstrapClusterCA by construction).
func freshStoreNoCA(t *testing.T) *e2eHarness {
	t.Helper()
	h := newE2E(t)
	if _, err := h.store.Pool().Exec(context.Background(),
		"delete from ca_certs"); err != nil {
		t.Fatalf("reset ca_certs: %v", err)
	}
	return h
}

// TestBootstrapClusterCA_GeneratesOnFreshDB verifies a fresh ca_certs
// table results in a single active row after one bootstrap call.
func TestBootstrapClusterCA_GeneratesOnFreshDB(t *testing.T) {
	h := freshStoreNoCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := api.BootstrapClusterCA(context.Background(), h.store, log); err != nil {
		t.Fatalf("BootstrapClusterCA: %v", err)
	}

	row, err := h.store.Queries().GetActiveCACert(context.Background())
	if err != nil {
		t.Fatalf("GetActiveCACert: %v", err)
	}
	if !row.Active {
		t.Error("Active = false on freshly-generated row")
	}
	if len(row.CertPem) == 0 {
		t.Error("CertPem empty")
	}
	if len(row.KeyPem) == 0 {
		t.Error("KeyPem empty")
	}
	if len(row.FingerprintSha256) != 32 {
		t.Errorf("Fingerprint length = %d, want 32", len(row.FingerprintSha256))
	}
}

// TestBootstrapClusterCA_IdempotentOnExisting verifies a repeat call
// against a DB that already has an active CA leaves the row
// untouched and does not produce an error.
func TestBootstrapClusterCA_IdempotentOnExisting(t *testing.T) {
	h := freshStoreNoCA(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := api.BootstrapClusterCA(context.Background(), h.store, log); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	first, err := h.store.Queries().GetActiveCACert(context.Background())
	if err != nil {
		t.Fatalf("first GetActiveCACert: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := api.BootstrapClusterCA(context.Background(), h.store, log); err != nil {
			t.Fatalf("bootstrap call %d: %v", i+2, err)
		}
	}

	second, err := h.store.Queries().GetActiveCACert(context.Background())
	if err != nil {
		t.Fatalf("second GetActiveCACert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("row replaced: first.ID=%s, second.ID=%s — bootstrap should be idempotent",
			first.ID, second.ID)
	}
	if string(first.FingerprintSha256) != string(second.FingerprintSha256) {
		t.Error("fingerprint changed on idempotent bootstrap")
	}
}
