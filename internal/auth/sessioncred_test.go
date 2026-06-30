// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
)

// sessionCredCmpOpts compare the claim fields whose types carry unexported
// state (netip.Addr) or monotonic-clock state (time.Time) by value equality
// rather than struct identity, so a round-tripped credential diffs clean.
var sessionCredCmpOpts = []cmp.Option{
	cmp.Comparer(func(a, b netip.Addr) bool { return a == b }),
	cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) }),
}

func newSessionCA(t *testing.T) (SessionCAMaterial, crypto.Signer, crypto.PublicKey) {
	t.Helper()
	mat, err := GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA() error = %v", err)
	}
	signer, err := ParseSessionCASigner(mat.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("ParseSessionCASigner() error = %v", err)
	}
	pub, err := ParseSessionCAPublic(mat.PublicKeyPEM)
	if err != nil {
		t.Fatalf("ParseSessionCAPublic() error = %v", err)
	}
	return mat, signer, pub
}

func sampleClaims(t *testing.T) SessionCredClaims {
	t.Helper()
	return SessionCredClaims{
		VMID:       uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		NICMAC:     "52:54:00:ab:cd:ef",
		GuestIP:    netip.MustParseAddr("10.42.0.7"),
		Port:       22,
		LeaseEpoch: 7,
	}
}

func TestSignSessionCredRoundTrip(t *testing.T) {
	_, signer, pub := newSessionCA(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := sampleClaims(t)

	tok, err := SignSessionCred(signer, in, now)
	if err != nil {
		t.Fatalf("SignSessionCred() error = %v", err)
	}
	if !strings.HasPrefix(tok, "otx_ingress_") {
		t.Fatalf("SignSessionCred() token = %q, want otx_ingress_ prefix", tok)
	}

	got, err := VerifySessionCred(pub, tok, now)
	if err != nil {
		t.Fatalf("VerifySessionCred() error = %v", err)
	}

	want := in
	want.ExpiresAt = now.Add(defaultSessionCredTTL)
	if diff := cmp.Diff(want, got, sessionCredCmpOpts...); diff != "" {
		t.Errorf("VerifySessionCred() claims mismatch (-want +got):\n%s", diff)
	}
}

func TestVerifySessionCredHonorsExplicitExpiry(t *testing.T) {
	_, signer, pub := newSessionCA(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := sampleClaims(t)
	in.ExpiresAt = now.Add(90 * time.Second)

	tok, err := SignSessionCred(signer, in, now)
	if err != nil {
		t.Fatalf("SignSessionCred() error = %v", err)
	}
	got, err := VerifySessionCred(pub, tok, now)
	if err != nil {
		t.Fatalf("VerifySessionCred() error = %v", err)
	}
	if !got.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("VerifySessionCred() ExpiresAt = %v, want %v", got.ExpiresAt, in.ExpiresAt)
	}
}

func TestVerifySessionCredTampered(t *testing.T) {
	_, signer, pub := newSessionCA(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tok, err := SignSessionCred(signer, sampleClaims(t), now)
	if err != nil {
		t.Fatalf("SignSessionCred() error = %v", err)
	}

	tampered := flipByte(tok, len(tok)-5)
	if tampered == tok {
		t.Fatalf("flipByte() did not change the token")
	}
	if _, err := VerifySessionCred(pub, tampered, now); !errors.Is(err, ErrSessionCredInvalid) {
		t.Errorf("VerifySessionCred(tampered) error = %v, want ErrSessionCredInvalid", err)
	}
}

func TestVerifySessionCredExpired(t *testing.T) {
	_, signer, pub := newSessionCA(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tok, err := SignSessionCred(signer, sampleClaims(t), now)
	if err != nil {
		t.Fatalf("SignSessionCred() error = %v", err)
	}

	later := now.Add(defaultSessionCredTTL + time.Minute)
	if _, err := VerifySessionCred(pub, tok, later); !errors.Is(err, ErrSessionCredExpired) {
		t.Errorf("VerifySessionCred(expired) error = %v, want ErrSessionCredExpired", err)
	}
}

func TestVerifySessionCredForeignKey(t *testing.T) {
	_, signer, _ := newSessionCA(t)
	_, _, foreignPub := newSessionCA(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tok, err := SignSessionCred(signer, sampleClaims(t), now)
	if err != nil {
		t.Fatalf("SignSessionCred() error = %v", err)
	}
	if _, err := VerifySessionCred(foreignPub, tok, now); !errors.Is(err, ErrSessionCredInvalid) {
		t.Errorf("VerifySessionCred(foreign key) error = %v, want ErrSessionCredInvalid", err)
	}
}

// TestIngressCredPrefixCollision pins the routing fact that an ingress
// credential also satisfies IsAPITokenFormat (otx_ingress_ is a superset of
// otx_), so any bearer-shape dispatcher MUST test IsIngressCredFormat first.
func TestIngressCredPrefixCollision(t *testing.T) {
	_, signer, _ := newSessionCA(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tok, err := SignSessionCred(signer, sampleClaims(t), now)
	if err != nil {
		t.Fatalf("SignSessionCred() error = %v", err)
	}
	if !IsIngressCredFormat(tok) {
		t.Errorf("IsIngressCredFormat(%q) = false, want true", tok)
	}
	if !IsAPITokenFormat(tok) {
		t.Errorf("IsAPITokenFormat(%q) = false, want true (otx_ingress_ collides with otx_)", tok)
	}
}

func flipByte(s string, i int) string {
	b := []byte(s)
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}
