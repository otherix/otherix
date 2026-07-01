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

func TestCanReachPortMembership(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := GrantPrincipal{
		VMs: map[string]GrantVMScope{"web": {Ports: []int{22, 8080}, Login: "ubuntu"}},
	}
	if login, ok := p.CanReach("web", 22, now); !ok || login != "ubuntu" {
		t.Errorf("CanReach(web,22) = (%q,%v), want (ubuntu,true)", login, ok)
	}
	if _, ok := p.CanReach("web", 5432, now); ok {
		t.Errorf("CanReach(web,5432) = ok, want reject (port not in set)")
	}
	if _, ok := p.CanReach("db", 22, now); ok {
		t.Errorf("CanReach(db,22) = ok, want reject (vm not in set)")
	}
}

func TestGrantPrincipal_CanReach(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	p := GrantPrincipalFromStore(store.IngressGrant{
		ID:        uuid.New(),
		VMs:       []store.IngressGrantVM{{VMName: "web01", Ports: []int{22}, Login: "dev"}},
		ExpiresAt: &exp,
	})
	if login, ok := p.CanReach("web01", 22, now); !ok || login != "dev" {
		t.Errorf("CanReach(web01,22) = (%q,%v), want (dev,true)", login, ok)
	}
	if _, ok := p.CanReach("web99", 22, now); ok {
		t.Errorf("CanReach(web99,22) = true, want false (not in scope)")
	}
	if _, ok := p.CanReach("web01", 22, exp.Add(time.Second)); ok {
		t.Errorf("CanReach after expiry = true, want false")
	}
	p.Revoked = true
	if _, ok := p.CanReach("web01", 22, now); ok {
		t.Errorf("CanReach when revoked = true, want false")
	}
}
