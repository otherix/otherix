// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestGrantToken_FormatAndHashRoundTrip(t *testing.T) {
	pt, hash, err := GenerateIngressGrantToken()
	if err != nil {
		t.Fatalf("GenerateIngressGrantToken() error = %v", err)
	}
	if !strings.HasPrefix(pt, "otx_ingressgrant_") {
		t.Errorf("token = %q, want otx_ingressgrant_ prefix", pt)
	}
	if !IsIngressGrantFormat(pt) {
		t.Errorf("IsIngressGrantFormat(%q) = false, want true", pt)
	}
	if IsIngressGrantFormat("otx_deadbeef") {
		t.Errorf("IsIngressGrantFormat(api token) = true, want false")
	}
	if string(HashToken(pt)) != string(hash) {
		t.Errorf("HashToken(plaintext) != returned hash")
	}
}

func TestGrantPrincipal_CanReach(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	p := GrantPrincipalFromStore(store.IngressGrant{
		ID:        uuid.New(),
		VMs:       []store.IngressGrantVM{{VMName: "web01", Login: "dev"}},
		ExpiresAt: &exp,
	})
	if login, ok := p.CanReach("web01", now); !ok || login != "dev" {
		t.Errorf("CanReach(web01) = (%q,%v), want (dev,true)", login, ok)
	}
	if _, ok := p.CanReach("web99", now); ok {
		t.Errorf("CanReach(web99) = true, want false (not in scope)")
	}
	if _, ok := p.CanReach("web01", exp.Add(time.Second)); ok {
		t.Errorf("CanReach after expiry = true, want false")
	}
	p.Revoked = true
	if _, ok := p.CanReach("web01", now); ok {
		t.Errorf("CanReach when revoked = true, want false")
	}
}
