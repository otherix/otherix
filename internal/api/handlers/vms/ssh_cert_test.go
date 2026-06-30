// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// sshCertStoreStub backs the IssueSSHCert tests. It serves one VM (web01,
// owned by vmOwner), one grant (resolved by token hash), and one active
// SSH user-CA. Any unconfigured method panics, proving the handler bailed
// before reaching it.
type sshCertStoreStub struct {
	Store
	vm       store.VM
	vmErr    error
	grant    store.SSHGrant
	grantErr error
	ca       store.SSHUserCA
	caErr    error
}

func (s *sshCertStoreStub) VMByName(context.Context, string) (store.VM, error) {
	return s.vm, s.vmErr
}

func (s *sshCertStoreStub) SSHGrantByTokenHash(context.Context, []byte) (store.SSHGrant, error) {
	return s.grant, s.grantErr
}

func (s *sshCertStoreStub) ActiveSSHUserCA(context.Context) (store.SSHUserCA, error) {
	return s.ca, s.caErr
}

// sshCertVerifierStub stands in for the CLI-token verifier. It returns the
// configured user for any token (the dual-auth grant branch never reaches it).
type sshCertVerifierStub struct {
	user *auth.User
	err  error
}

func (v sshCertVerifierStub) VerifyAccessToken(string) (*auth.User, error) {
	return v.user, v.err
}

func (v sshCertVerifierStub) VerifyAPIToken(context.Context, string) (*auth.User, error) {
	return v.user, v.err
}

// withChiURLParam attaches a chi route context so chi.URLParam(r, key)
// resolves to val inside the handler under test.
func withChiURLParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// newSSHCertTestHandler builds a Handler whose store serves a web01 VM
// owned by vmOwner, a grant scoped to {web01: dev}, and a freshly
// generated active SSH user-CA. It returns the handler and the grant's
// plaintext token. cliUser, when non-nil, is the principal the CLI-token
// verifier resolves to.
func newSSHCertTestHandler(t *testing.T, vmOwner uuid.UUID, cliUser *auth.User) (*Handler, string) {
	t.Helper()
	caMaterial, err := auth.GenerateSSHUserCA()
	if err != nil {
		t.Fatalf("GenerateSSHUserCA: %v", err)
	}
	plaintext, hash, err := auth.GenerateGrantToken()
	if err != nil {
		t.Fatalf("GenerateGrantToken: %v", err)
	}
	st := &sshCertStoreStub{
		vm: store.VM{ID: uuid.New(), Name: "web01", OwnerID: vmOwner},
		grant: store.SSHGrant{
			ID:        uuid.New(),
			TokenHash: hash,
			VMs:       []store.SSHGrantVM{{VMName: "web01", Login: "dev"}},
		},
		ca: store.SSHUserCA{ID: uuid.New(), PrivateKeyPEM: caMaterial.PrivateKeyPEM},
	}
	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{}, ConsoleDeps{},
		SSHDeps{Verifier: sshCertVerifierStub{user: cliUser}, CertTTL: 5 * time.Minute})
	return h, plaintext
}

func newTestSSHPublicKey(t *testing.T) string {
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

func sshCertRequestHTTP(t *testing.T, token string, body sshCertRequest) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/web01/ssh-cert", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+token)
	req = withChiURLParam(req, "id", "web01")
	return req, httptest.NewRecorder()
}

// TestIssueSSHCert_GrantTokenMintsCertForAllowedLogin: a grant scoped to
// web01/login=dev mints a guest cert for that login.
func TestIssueSSHCert_GrantTokenMintsCertForAllowedLogin(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)

	req, rec := sshCertRequestHTTP(t, grantToken, sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "dev",
	})
	h.IssueSSHCert(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp sshCertResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Login != "dev" {
		t.Errorf("login = %q, want dev", resp.Login)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(resp.Certificate))
	if err != nil {
		t.Fatalf("parse returned cert: %v", err)
	}
	cert, ok := parsed.(*ssh.Certificate)
	if !ok {
		t.Fatalf("returned key is not an ssh certificate")
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "dev" {
		t.Errorf("cert principals = %v, want [dev]", cert.ValidPrincipals)
	}
}

// TestIssueSSHCert_GrantTokenEmptyLoginUsesPinned: omitting login on the
// grant path mints the grant's pinned login.
func TestIssueSSHCert_GrantTokenEmptyLoginUsesPinned(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)

	req, rec := sshCertRequestHTTP(t, grantToken, sshCertRequest{PublicKey: newTestSSHPublicKey(t)})
	h.IssueSSHCert(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp sshCertResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Login != "dev" {
		t.Errorf("login = %q, want dev (pinned)", resp.Login)
	}
}

// TestIssueSSHCert_GrantTokenRejectsDisallowedLogin: a requested login that
// differs from the grant's pinned login is 403 ssh_login_not_allowed (the
// caller already proved reach, so this is not an enumeration oracle).
func TestIssueSSHCert_GrantTokenRejectsDisallowedLogin(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)

	req, rec := sshCertRequestHTTP(t, grantToken, sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "root",
	})
	h.IssueSSHCert(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (login root not the grant login)", rec.Code)
	}
	var body response.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error.Code != response.CodeSSHLoginNotAllowed {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeSSHLoginNotAllowed)
	}
}

// TestIssueSSHCert_GrantTokenRevokedIsGenericReject: a revoked grant (CanReach
// false) collapses to the uniform 401 ssh_session_rejected.
func TestIssueSSHCert_GrantTokenRevokedIsGenericReject(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)
	h.store.(*sshCertStoreStub).grant.Revoked = true

	req, rec := sshCertRequestHTTP(t, grantToken, sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "dev",
	})
	h.IssueSSHCert(rec, req)
	assertGenericSSHReject(t, rec)
}

// TestIssueSSHCert_GrantTokenOutOfSetIsGenericReject: a grant that does not
// cover the requested VM collapses to the uniform 401.
func TestIssueSSHCert_GrantTokenOutOfSetIsGenericReject(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)
	h.store.(*sshCertStoreStub).grant.VMs = []store.SSHGrantVM{{VMName: "other", Login: "dev"}}

	req, rec := sshCertRequestHTTP(t, grantToken, sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "dev",
	})
	h.IssueSSHCert(rec, req)
	assertGenericSSHReject(t, rec)
}

// TestIssueSSHCert_CLITokenMintsSanitizedLogin: an owner with vm:ssh mints a
// cert for the sanitized requested login.
func TestIssueSSHCert_CLITokenMintsSanitizedLogin(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	user := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeAPIToken}
	h, _ := newSSHCertTestHandler(t, owner, user)

	req, rec := sshCertRequestHTTP(t, "otx_clitokenplaceholder", sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "ubuntu",
	})
	h.IssueSSHCert(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp sshCertResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Login != "ubuntu" {
		t.Errorf("login = %q, want ubuntu", resp.Login)
	}
}

// TestIssueSSHCert_CLITokenForeignVMIsGenericReject: a CLI caller who does
// not own the VM gets the uniform 401, not a 403/404 that leaks existence.
func TestIssueSSHCert_CLITokenForeignVMIsGenericReject(t *testing.T) {
	t.Parallel()
	// developer (scope=own) requesting a VM owned by someone else.
	user := &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper, Type: auth.TypeJWT}
	h, _ := newSSHCertTestHandler(t, uuid.New(), user)

	req, rec := sshCertRequestHTTP(t, "eyJ.jwt.placeholder", sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "ubuntu",
	})
	h.IssueSSHCert(rec, req)
	assertGenericSSHReject(t, rec)
}

// TestIssueSSHCert_UnknownVMIsGenericReject: an unknown VM name collapses to
// the uniform 401 - no existence oracle.
func TestIssueSSHCert_UnknownVMIsGenericReject(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	user := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeAPIToken}
	h, _ := newSSHCertTestHandler(t, owner, user)
	h.store.(*sshCertStoreStub).vmErr = store.ErrNotFound

	req, rec := sshCertRequestHTTP(t, "otx_clitokenplaceholder", sshCertRequest{
		PublicKey: newTestSSHPublicKey(t),
		Login:     "ubuntu",
	})
	h.IssueSSHCert(rec, req)
	assertGenericSSHReject(t, rec)
}

// TestIssueSSHCert_BadPublicKeyIs400: a request whose public_key does not
// parse is 400 validation_failed.
func TestIssueSSHCert_BadPublicKeyIs400(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)

	req, rec := sshCertRequestHTTP(t, grantToken, sshCertRequest{
		PublicKey: "not-a-key",
		Login:     "dev",
	})
	h.IssueSSHCert(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var body response.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error.Code != response.CodeValidationFailed {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeValidationFailed)
	}
}

// TestIssueSSHCert_MissingBearerIsGenericReject: no Authorization header
// collapses to the uniform 401.
func TestIssueSSHCert_MissingBearerIsGenericReject(t *testing.T) {
	t.Parallel()
	h, _ := newSSHCertTestHandler(t, uuid.New(), nil)

	raw, _ := json.Marshal(sshCertRequest{PublicKey: newTestSSHPublicKey(t), Login: "dev"})
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/web01/ssh-cert", strings.NewReader(string(raw)))
	req = withChiURLParam(req, "id", "web01")
	rec := httptest.NewRecorder()
	h.IssueSSHCert(rec, req)
	assertGenericSSHReject(t, rec)
}

// TestIssueSSHCert_OverCapBodyIs400: a request body larger than
// sshCertMaxRequestBytes is rejected as 400 validation_failed by the
// handler's own MaxBytesReader - it must not 500, panic, or buffer the
// whole oversized body. Guards the pre-auth DoS hardening on this
// outside-Authn endpoint.
func TestIssueSSHCert_OverCapBodyIs400(t *testing.T) {
	t.Parallel()
	h, grantToken := newSSHCertTestHandler(t, uuid.New(), nil)

	// A well-formed JSON object whose public_key value alone exceeds the
	// 64 KiB cap, so the MaxBytesReader trips mid-decode.
	oversized := `{"public_key":"` + strings.Repeat("A", int(sshCertMaxRequestBytes)+1024) + `","login":"dev"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/web01/ssh-cert", strings.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer "+grantToken)
	req = withChiURLParam(req, "id", "web01")
	rec := httptest.NewRecorder()

	h.IssueSSHCert(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var body response.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error.Code != response.CodeValidationFailed {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeValidationFailed)
	}
}

func assertGenericSSHReject(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body response.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error.Code != response.CodeSSHSessionRejected {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeSSHSessionRejected)
	}
}

func TestSanitizeLogin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"dev", "dev", true},
		{"ubuntu", "ubuntu", true},
		{"root", "root", true}, // charset-valid; the guest sshd decides
		{"_svc", "_svc", true},
		{"a-b_c0", "a-b_c0", true},
		{"", "", false},
		{"dev;rm -rf", "", false},
		{"../x", "", false},
		{"Dev", "", false},                   // leading uppercase rejected
		{"0day", "", false},                  // leading digit rejected
		{"foo bar", "", false},               // space rejected
		{"foo$bar", "", false},               // shell metachar rejected
		{strings.Repeat("a", 33), "", false}, // over length cap
	}
	for _, c := range cases {
		got, ok := sanitizeLogin(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("sanitizeLogin(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
