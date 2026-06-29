// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/ed25519"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSignGuestCert_PrincipalAndValidity(t *testing.T) {
	ca, err := GenerateSSHUserCA()
	if err != nil {
		t.Fatalf("GenerateSSHUserCA() error = %v", err)
	}
	signer, err := ParseSSHUserCA(ca.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("ParseSSHUserCA() error = %v", err)
	}
	// A client keypair to be certified.
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	cert, err := SignGuestCert(signer, sshPub, "dev", "grant:abc", 2*time.Minute, now)
	if err != nil {
		t.Fatalf("SignGuestCert() error = %v", err)
	}
	if got, want := cert.CertType, uint32(ssh.UserCert); got != want {
		t.Errorf("CertType = %d, want %d", got, want)
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "dev" {
		t.Errorf("ValidPrincipals = %v, want [dev]", cert.ValidPrincipals)
	}
	if cert.KeyId != "grant:abc" {
		t.Errorf("KeyId = %q, want grant:abc", cert.KeyId)
	}
	// Verify the cert chains to the CA and the login is permitted.
	checker := &ssh.CertChecker{
		// Pin the clock to the same instant the cert was signed against so the
		// validity window check is deterministic rather than wall-clock dependent.
		Clock: func() time.Time { return now },
		IsUserAuthority: func(k ssh.PublicKey) bool {
			return string(k.Marshal()) == string(signer.PublicKey().Marshal())
		},
	}
	if err := checker.CheckCert("dev", cert); err != nil {
		t.Errorf("CheckCert(dev) error = %v, want nil", err)
	}
	if err := checker.CheckCert("root", cert); err == nil {
		t.Errorf("CheckCert(root) error = nil, want non-nil (login not a principal)")
	}
}
