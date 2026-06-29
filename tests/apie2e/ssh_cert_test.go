// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// sshCertResp is the decode target for the cert-mint response.
type sshCertResp struct {
	Certificate string `json:"certificate"`
	Login       string `json:"login"`
	ExpiresAt   string `json:"expires_at"`
}

// seedSSHUserCA provisions the cluster SSH user-CA directly via the store so
// the cert-mint endpoint has a signer.
func seedSSHUserCA(t *testing.T, s *etcdstore.Store) {
	t.Helper()
	material, err := auth.GenerateSSHUserCA()
	if err != nil {
		t.Fatalf("GenerateSSHUserCA: %v", err)
	}
	if _, err := s.CreateSSHUserCA(context.Background(), store.CreateSSHUserCAParams{
		PrivateKeyPEM:       material.PrivateKeyPEM,
		PublicKeyAuthorized: material.PublicKeyAuthorized,
	}); err != nil {
		t.Fatalf("CreateSSHUserCA: %v", err)
	}
}

// seedSSHGrant creates a grant scoped to {vmName: login} directly via the
// store and returns the plaintext token to present at the endpoint.
func seedSSHGrant(t *testing.T, s *etcdstore.Store, creator uuid.UUID, vmName, login string) string {
	t.Helper()
	plaintext, hash, err := auth.GenerateGrantToken()
	if err != nil {
		t.Fatalf("GenerateGrantToken: %v", err)
	}
	if _, err := s.CreateSSHGrant(context.Background(), store.CreateSSHGrantParams{
		Name:      "grant-" + uuid.NewString()[:8],
		CreatedBy: creator,
		TokenHash: hash,
		VMs:       []store.SSHGrantVM{{VMName: vmName, Login: login}},
	}); err != nil {
		t.Fatalf("CreateSSHGrant: %v", err)
	}
	return plaintext
}

// genSSHPublicKey returns a fresh ed25519 public key in authorized-keys form.
func genSSHPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(sshPub))
}

// assertSSHCert parses resp as a 200 cert-mint response and asserts the cert
// certifies wantLogin.
func assertSSHCert(t *testing.T, resp *http.Response, wantLogin string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out sshCertResp
	decodeJSON(t, resp, &out)
	if out.Login != wantLogin {
		t.Errorf("login = %q, want %q", out.Login, wantLogin)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(out.Certificate))
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	cert, ok := parsed.(*ssh.Certificate)
	if !ok {
		t.Fatalf("returned key is not an ssh certificate")
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != wantLogin {
		t.Errorf("cert principals = %v, want [%s]", cert.ValidPrincipals, wantLogin)
	}
}

// TestSSHCertCLIToken mints a cert for the VM owner over a CLI access token and
// confirms the requested login is sanitized through to the cert principal.
func TestSSHCertCLIToken(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	vmName, _ := seedOwnedVM(t, h.store, opID)

	resp := h.post(t, "/v1/vms/"+vmName+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "ubuntu",
	}, opTok)
	assertSSHCert(t, resp, "ubuntu")
}

// TestSSHCertCLITokenMalformedLogin rejects a login with shell metacharacters
// at the CP edge (400) before any cert is signed.
func TestSSHCertCLITokenMalformedLogin(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	vmName, _ := seedOwnedVM(t, h.store, opID)

	resp := h.post(t, "/v1/vms/"+vmName+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "dev;rm -rf /",
	}, opTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSSHCertCLITokenForeignVMIsGenericReject: a developer (scope=own)
// requesting a cert for a VM owned by someone else gets the uniform 401, not a
// 403/404 that leaks the VM's existence.
func TestSSHCertCLITokenForeignVMIsGenericReject(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	devTok, _ := loginAs(t, h, auth.RoleDeveloper)
	_, opID := loginAs(t, h, auth.RoleOperator)
	foreignVM, _ := seedOwnedVM(t, h.store, opID)

	resp := h.post(t, "/v1/vms/"+foreignVM+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "ubuntu",
	}, devTok)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (uniform reject)", resp.StatusCode)
	}
}

// TestSSHCertGrantTokenPinnedLogin: a grant scoped to {vm: ci} mints a cert for
// the pinned login, ignoring an omitted client login.
func TestSSHCertGrantTokenPinnedLogin(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	_, opID := loginAs(t, h, auth.RoleOperator)
	vmName, _ := seedOwnedVM(t, h.store, opID)
	grantTok := seedSSHGrant(t, h.store, opID, vmName, "ci")

	resp := h.post(t, "/v1/vms/"+vmName+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
	}, grantTok)
	assertSSHCert(t, resp, "ci")
}

// TestSSHCertGrantTokenLoginMismatch: a grant caller requesting a login other
// than the grant's pinned login is 403 ssh_login_not_allowed (the caller
// already proved reach, so this is not an enumeration oracle).
func TestSSHCertGrantTokenLoginMismatch(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	_, opID := loginAs(t, h, auth.RoleOperator)
	vmName, _ := seedOwnedVM(t, h.store, opID)
	grantTok := seedSSHGrant(t, h.store, opID, vmName, "ci")

	resp := h.post(t, "/v1/vms/"+vmName+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "root",
	}, grantTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestSSHCertGrantTokenOutOfSetIsGenericReject: a grant that does not cover the
// requested VM collapses to the uniform 401.
func TestSSHCertGrantTokenOutOfSetIsGenericReject(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	_, opID := loginAs(t, h, auth.RoleOperator)
	grantedVM, _ := seedOwnedVM(t, h.store, opID)
	otherVM, _ := seedOwnedVM(t, h.store, opID)
	grantTok := seedSSHGrant(t, h.store, opID, grantedVM, "ci")

	resp := h.post(t, "/v1/vms/"+otherVM+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "ci",
	}, grantTok)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (uniform reject)", resp.StatusCode)
	}
}

// TestSSHCertGrantTokenRevokedIsGenericReject: a revoked grant collapses to the
// uniform 401 (the row survives revoke so the reject is deterministic).
func TestSSHCertGrantTokenRevokedIsGenericReject(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	_, opID := loginAs(t, h, auth.RoleOperator)
	vmName, _ := seedOwnedVM(t, h.store, opID)
	grantTok := seedSSHGrant(t, h.store, opID, vmName, "ci")

	// Revoke it via the store (the grant id is resolved through the token hash).
	g, err := h.store.SSHGrantByTokenHash(context.Background(), auth.HashToken(grantTok))
	if err != nil {
		t.Fatalf("SSHGrantByTokenHash: %v", err)
	}
	if err := h.store.RevokeSSHGrant(context.Background(), g.ID); err != nil {
		t.Fatalf("RevokeSSHGrant: %v", err)
	}

	resp := h.post(t, "/v1/vms/"+vmName+"/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "ci",
	}, grantTok)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (uniform reject)", resp.StatusCode)
	}
}

// TestSSHCertUnknownVMIsGenericReject: an unknown VM name collapses to the
// uniform 401 - no existence oracle for an authenticated CLI caller.
func TestSSHCertUnknownVMIsGenericReject(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	opTok, _ := loginAs(t, h, auth.RoleOperator)

	resp := h.post(t, "/v1/vms/ghost/ssh-cert", map[string]string{
		"public_key": genSSHPublicKey(t),
		"login":      "ubuntu",
	}, opTok)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (uniform reject)", resp.StatusCode)
	}
}

// TestSSHCertBadPublicKeyIs400: a public_key that does not parse is 400
// validation_failed.
func TestSSHCertBadPublicKeyIs400(t *testing.T) {
	h := newE2E(t)
	seedSSHUserCA(t, h.store)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	vmName, _ := seedOwnedVM(t, h.store, opID)

	resp := h.post(t, "/v1/vms/"+vmName+"/ssh-cert", map[string]string{
		"public_key": "garbage",
		"login":      "ubuntu",
	}, opTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
