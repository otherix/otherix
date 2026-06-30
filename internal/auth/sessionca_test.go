// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"testing"
)

func TestGenerateSessionCA_ParseableP384(t *testing.T) {
	mat, err := GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA() error = %v", err)
	}
	signer, err := ParseSessionCASigner(mat.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("ParseSessionCASigner() error = %v", err)
	}
	priv, ok := signer.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("signer concrete type = %T, want *ecdsa.PrivateKey", signer)
	}
	if priv.Curve != elliptic.P384() {
		t.Errorf("curve = %s, want P-384", priv.Curve.Params().Name)
	}
	pub, err := ParseSessionCAPublic(mat.PublicKeyPEM)
	if err != nil {
		t.Fatalf("ParseSessionCAPublic() error = %v", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public concrete type = %T, want *ecdsa.PublicKey", pub)
	}
	if !ecPub.Equal(&priv.PublicKey) {
		t.Errorf("parsed public half does not match the signer's public key")
	}
}

func TestGenerateSessionCA_DistinctKeys(t *testing.T) {
	a, err := GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA() first error = %v", err)
	}
	b, err := GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA() second error = %v", err)
	}
	if bytes.Equal(a.PrivateKeyPEM, b.PrivateKeyPEM) {
		t.Errorf("two GenerateSessionCA calls produced identical private keys")
	}
	if bytes.Equal(a.PublicKeyPEM, b.PublicKeyPEM) {
		t.Errorf("two GenerateSessionCA calls produced identical public keys")
	}
	// The session CA must be a distinct key from the cluster SSH user-CA so a
	// leaked session credential never widens trust granted by another authority.
	ssh, err := GenerateSSHUserCA()
	if err != nil {
		t.Fatalf("GenerateSSHUserCA() error = %v", err)
	}
	if bytes.Equal(a.PrivateKeyPEM, ssh.PrivateKeyPEM) {
		t.Errorf("session CA private key equals SSH user-CA private key; must be distinct")
	}
}
