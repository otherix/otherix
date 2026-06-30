// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHUserCAMaterial is a freshly generated cluster SSH user-CA: the
// PEM-encoded private key (persisted in etcd) and the authorized-keys
// one-line public form (handed to guests as TrustedUserCAKeys).
type SSHUserCAMaterial struct {
	PrivateKeyPEM       []byte
	PublicKeyAuthorized []byte
}

// GenerateSSHUserCA produces a fresh ECDSA P-384 SSH user certificate
// authority. The cluster signs short-lived guest user-certs with it; the
// public half is provisioned into every SSH-ingress VM as TrustedUserCAKeys.
func GenerateSSHUserCA() (SSHUserCAMaterial, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return SSHUserCAMaterial{}, fmt.Errorf("generate ssh user ca key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return SSHUserCAMaterial{}, fmt.Errorf("marshal ssh user ca key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sshPub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		return SSHUserCAMaterial{}, fmt.Errorf("ssh public key: %v", err)
	}
	return SSHUserCAMaterial{
		PrivateKeyPEM:       keyPEM,
		PublicKeyAuthorized: ssh.MarshalAuthorizedKey(sshPub),
	}, nil
}

// ParseSSHUserCA loads a PEM private key produced by GenerateSSHUserCA
// into an ssh.Signer used to sign guest certs.
func ParseSSHUserCA(privateKeyPEM []byte) (ssh.Signer, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("ssh user ca: no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ssh user ca: parse key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("ssh user ca: signer: %v", err)
	}
	return signer, nil
}

// SignGuestCert issues a short-lived SSH user certificate for login on a
// CA-trusting guest. The login is the sole valid principal; keyID carries
// the Otherix principal / grant id for guest-side audit. The caller has
// already gated reach (vm:ssh / grant scope) and sanitized/pinned the login;
// the guest sshd is the authority for whether it accepts that login.
func SignGuestCert(caSigner ssh.Signer, userPubKey ssh.PublicKey, login, keyID string, validity time.Duration, now time.Time) (*ssh.Certificate, error) {
	serial := make([]byte, 8)
	if _, err := rand.Read(serial); err != nil {
		return nil, fmt.Errorf("sign guest cert: serial: %v", err)
	}
	cert := &ssh.Certificate{
		Key:             userPubKey,
		Serial:          uint64(serial[0]) | uint64(serial[1])<<8, // any unique-ish value
		CertType:        ssh.UserCert,
		KeyId:           keyID,
		ValidPrincipals: []string{login},
		ValidAfter:      uint64(now.Add(-30 * time.Second).Unix()), //nolint:gosec // G115: a post-1970 Unix timestamp is always positive and well within uint64 range.
		ValidBefore:     uint64(now.Add(validity).Unix()),          //nolint:gosec // G115: a post-1970 Unix timestamp is always positive and well within uint64 range.
		Permissions: ssh.Permissions{
			Extensions: map[string]string{
				"permit-pty":     "",
				"permit-user-rc": "",
			},
		},
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		return nil, fmt.Errorf("sign guest cert: %v", err)
	}
	return cert, nil
}
