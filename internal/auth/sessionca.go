// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// SessionCAMaterial is a freshly generated ingress-session certificate
// authority: the PEM-encoded private key (persisted in etcd, signs short-lived
// ingress session credentials) and the PEM-encoded public half (distributed to
// gateways so they verify those credentials offline). It is a distinct key from
// the cluster CA, so a leaked session credential never widens mesh trust.
type SessionCAMaterial struct {
	PrivateKeyPEM []byte
	PublicKeyPEM  []byte
}

// GenerateSessionCA produces a fresh ECDSA P-384 ingress-session certificate
// authority. The control plane signs short-lived session credentials with the
// private half; the public half is shipped to gateways via heartbeat so they
// verify those credentials without contacting the control plane.
func GenerateSessionCA() (SessionCAMaterial, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return SessionCAMaterial{}, fmt.Errorf("generate session ca key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return SessionCAMaterial{}, fmt.Errorf("marshal session ca key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return SessionCAMaterial{}, fmt.Errorf("marshal session ca public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return SessionCAMaterial{
		PrivateKeyPEM: keyPEM,
		PublicKeyPEM:  pubPEM,
	}, nil
}

// ParseSessionCASigner loads a PEM private key produced by GenerateSessionCA
// into a crypto.Signer the control plane uses to sign session credentials.
func ParseSessionCASigner(privPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(privPEM)
	if block == nil {
		return nil, fmt.Errorf("session ca: no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("session ca: parse key: %v", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("session ca: key of type %T is not a crypto.Signer", key)
	}
	return signer, nil
}

// ParseSessionCAPublic loads the PEM public half produced by GenerateSessionCA
// into a crypto.PublicKey gateways use to verify session credentials offline.
func ParseSessionCAPublic(pubPEM []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return nil, fmt.Errorf("session ca public: no PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("session ca public: parse key: %v", err)
	}
	return pub, nil
}
