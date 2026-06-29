// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/ssh"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// SSHTokenVerifier is the narrow contract the cert-mint endpoint needs to
// authenticate a CLI bearer (a user JWT or an otx_ API token). *auth.Service
// satisfies it; the interface keeps the handler testable without standing up
// a real Service.
type SSHTokenVerifier interface {
	VerifyAccessToken(raw string) (*auth.User, error)
	VerifyAPIToken(ctx context.Context, raw string) (*auth.User, error)
}

// SSHDeps bundles the dependencies the IssueSSHCert handler needs: the
// CLI-token verifier (grant tokens are resolved through the store, not this
// verifier) and the guest-cert TTL. A zero CertTTL falls back to
// defaultGuestCertTTL.
type SSHDeps struct {
	Verifier SSHTokenVerifier
	CertTTL  time.Duration
}

// defaultGuestCertTTL is the validity of a minted guest SSH user-cert when
// SSHDeps.CertTTL is unset. Deliberately single-digit minutes: the cert is a
// connect-time credential, re-minted per connection.
const defaultGuestCertTTL = 5 * time.Minute

// sshSessionRejectedMsg is the single generic message every anti-enumeration
// rejection on the cert-mint endpoint returns. Unknown VM, unauthorized
// caller, and bad/expired/revoked grant must all answer identically: any
// divergence hands the caller a VM-existence oracle.
const sshSessionRejectedMsg = "ssh certificate request rejected"

// sshCertRequest is the cert-mint request body: the user public key (SSH
// authorized-keys one-line form) to certify and the desired guest login.
// On the grant path the login is ignored unless it contradicts the grant's
// pinned login; on the CLI path it is sanitized to a valid SSH principal.
type sshCertRequest struct {
	PublicKey string `json:"public_key"`
	Login     string `json:"login"`
}

// sshCertResponse carries the minted guest cert (authorized-keys line), the
// login it certifies, and its expiry (RFC 3339).
type sshCertResponse struct {
	Certificate string `json:"certificate"`
	Login       string `json:"login"`
	ExpiresAt   string `json:"expires_at"`
}

// IssueSSHCert implements POST /v1/vms/{id}/ssh-cert: it mints a short-lived
// guest SSH user-cert signed by the cluster SSH user-CA, for the login the
// caller is allowed to use on the named VM.
//
// This route is mounted OUTSIDE the global Authn middleware so it can accept
// an SSH-grant token (not an Authn principal) and structurally guarantee a
// grant token reaches no other route. The handler reads the bearer itself and
// dual-dispatches:
//
//   - An SSH-grant token (auth.IsGrantTokenFormat, checked first because its
//     prefix is a superset of "otx_") resolves through the store; the cert is
//     minted for the grant's pinned login on the named VM. A requested login
//     contradicting the pinned one is rejected (403 ssh_login_not_allowed,
//     not an enumeration oracle: the caller already proved reach).
//   - Any other bearer is verified as a CLI token (JWT or otx_ API token);
//     the caller must hold vm:ssh and own the VM (scope permitting). The cert
//     is minted for the requested login after sanitizeLogin - there is no
//     CP-side allow-list, the guest sshd is the sole login authority.
//
// Every failure on either path (missing/garbage token, unknown VM,
// unauthorized caller, bad/expired/revoked grant) collapses to one uniform
// 401 ssh_session_rejected so the endpoint never leaks VM existence. Only a
// malformed request body / public key (400 validation_failed) and the grant
// login mismatch (403) diverge - neither is VM-dependent.
func (h *Handler) IssueSSHCert(w http.ResponseWriter, r *http.Request) {
	vmName := chi.URLParam(r, "id")

	tok, ok := bearerToken(r)
	if !ok {
		h.rejectSSH(w, r)
		return
	}

	var req sshCertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	userPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "public_key is not a valid SSH public key", nil)
		return
	}

	now := time.Now()
	var login, keyID string
	if auth.IsGrantTokenFormat(tok) {
		login, keyID, ok = h.authorizeGrant(r.Context(), tok, vmName, req.Login, now, w, r)
		if !ok {
			return
		}
	} else {
		login, keyID, ok = h.authorizeCLI(r.Context(), tok, vmName, req.Login, w, r)
		if !ok {
			return
		}
	}

	caRow, err := h.store.ActiveSSHUserCA(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.issueSSHCert load ssh user ca",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "ssh user ca unavailable", nil)
		return
	}
	signer, err := auth.ParseSSHUserCA(caRow.PrivateKeyPEM)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.issueSSHCert parse ssh user ca",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "ssh user ca unavailable", nil)
		return
	}
	cert, err := auth.SignGuestCert(signer, userPub, login, keyID, h.guestCertTTL(), now)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.issueSSHCert sign guest cert",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "sign guest certificate", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, sshCertResponse{
		Certificate: string(ssh.MarshalAuthorizedKey(cert)),
		Login:       login,
		ExpiresAt:   time.Unix(int64(cert.ValidBefore), 0).UTC().Format(time.RFC3339), //nolint:gosec // ValidBefore is a server-set near-future Unix second, well within int64.
	})
}

// authorizeGrant resolves the grant token, checks it authorizes vmName, and
// returns the grant's pinned login + the cert key-id. A requested login that
// contradicts the pinned one is 403 ssh_login_not_allowed; every other failure
// is the uniform 401. ok=false means a response was already written.
func (h *Handler) authorizeGrant(ctx context.Context, tok, vmName, requestedLogin string, now time.Time, w http.ResponseWriter, r *http.Request) (login, keyID string, ok bool) {
	grant, err := h.store.SSHGrantByTokenHash(ctx, auth.HashToken(tok))
	if err != nil {
		h.rejectSSH(w, r)
		return "", "", false
	}
	pinned, reachable := auth.GrantPrincipalFromStore(grant).CanReach(vmName, now)
	if !reachable {
		h.rejectSSH(w, r)
		return "", "", false
	}
	if requestedLogin != "" && requestedLogin != pinned {
		response.WriteError(w, r, http.StatusForbidden,
			response.CodeSSHLoginNotAllowed,
			"the requested login is not permitted by this grant", nil)
		return "", "", false
	}
	return pinned, "grant:" + grant.ID.String(), true
}

// authorizeCLI verifies a CLI bearer (JWT or otx_ API token), loads the VM,
// and enforces vm:ssh + ownership. It returns the sanitized requested login +
// the cert key-id. A malformed login is 400 validation_failed; an unknown VM
// or an unauthorized caller is the uniform 401 (no existence leak). ok=false
// means a response was already written.
func (h *Handler) authorizeCLI(ctx context.Context, tok, vmName, requestedLogin string, w http.ResponseWriter, r *http.Request) (login, keyID string, ok bool) {
	if h.sshDeps.Verifier == nil {
		h.rejectSSH(w, r)
		return "", "", false
	}
	user, err := h.verifyCLIToken(ctx, tok)
	if err != nil || user == nil {
		h.rejectSSH(w, r)
		return "", "", false
	}
	vm, err := h.store.VMByName(ctx, vmName)
	if err != nil {
		h.rejectSSH(w, r)
		return "", "", false
	}
	if !auth.Has(user.Role, auth.PermVMSSH) {
		h.rejectSSH(w, r)
		return "", "", false
	}
	if err := auth.CheckOwnership(user, &vm.OwnerID, auth.PermVMSSH); err != nil {
		h.rejectSSH(w, r)
		return "", "", false
	}
	sanitized, valid := sanitizeLogin(requestedLogin)
	if !valid {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"login must match [a-z_][a-z0-9_-]* (max 32 chars)", nil)
		return "", "", false
	}
	return sanitized, "user:" + user.ID.String(), true
}

// verifyCLIToken dispatches the bearer to the API-token or JWT verifier by
// prefix, mirroring the Authn middleware. The grant-token shape was already
// excluded by the caller (IsGrantTokenFormat).
func (h *Handler) verifyCLIToken(ctx context.Context, tok string) (*auth.User, error) {
	if auth.IsAPITokenFormat(tok) {
		return h.sshDeps.Verifier.VerifyAPIToken(ctx, tok)
	}
	return h.sshDeps.Verifier.VerifyAccessToken(tok)
}

// guestCertTTL is SSHDeps.CertTTL, or defaultGuestCertTTL when unset.
func (h *Handler) guestCertTTL() time.Duration {
	if h.sshDeps.CertTTL <= 0 {
		return defaultGuestCertTTL
	}
	return h.sshDeps.CertTTL
}

// bearerToken extracts the token portion of an Authorization: Bearer header.
// The scheme match is case-insensitive per RFC 7235; the token after the
// scheme is returned verbatim. Returns ("", false) when the header is absent,
// malformed, or empty after the scheme.
func bearerToken(r *http.Request) (string, bool) {
	scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	if tok == "" {
		return "", false
	}
	return tok, true
}

// rejectSSH writes the uniform anti-enumeration rejection.
func (h *Handler) rejectSSH(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusUnauthorized,
		response.CodeSSHSessionRejected, sshSessionRejectedMsg, nil)
}

// loginPattern is the valid SSH principal charset the CLI path enforces:
// a lowercase start ([a-z_]) followed by up to 31 [a-z0-9_-] characters
// (32-char cap, the conventional Linux login limit). The guest sshd is the
// sole authority for whether it accepts the login; sanitizeLogin only
// guarantees the value is a safe single principal (no shell metacharacters,
// path separators, or whitespace) before it is baked into a certificate.
var loginPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// sanitizeLogin reports whether login is a valid SSH principal and returns
// it unchanged when so. It rejects (ok=false) empty, over-long, and any
// value carrying characters outside loginPattern.
func sanitizeLogin(login string) (string, bool) {
	if !loginPattern.MatchString(login) {
		return "", false
	}
	return login, true
}
