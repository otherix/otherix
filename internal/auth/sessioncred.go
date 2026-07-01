// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Ingress-credential wire format: "otx_ingress_" + base64url(header) + "." +
// base64url(claims) + "." + base64url(signature). The token is a self-contained
// bearer (no x509), so it hits none of the mesh mTLS cert gates: the control
// plane signs it with the ingress-session CA private half and a gateway verifies
// it offline with the public half shipped over heartbeat. The longer prefix
// keeps ingress credentials distinct from ordinary API tokens ("otx_") so the
// bearer dispatch routes a credential only to the ingress path. The prefix is
// intentionally a superset of "otx_": callers that test for ingress shape must
// check IsIngressCredFormat before IsAPITokenFormat.
const (
	ingressCredPrefix = "otx_ingress_" //nolint:gosec // G101: public routing prefix, not a credential; the secret is the signature.

	// defaultSessionCredTTL is the validity stamped onto a credential whose
	// claims leave ExpiresAt unset. It mirrors the guest SSH user-cert TTL:
	// short enough that a leaked bearer is near-useless, long enough to cover
	// a connect handshake without a clock-skew miss.
	defaultSessionCredTTL = 5 * time.Minute

	// sessionCredAlg is the only signature algorithm an ingress credential
	// carries: ECDSA over P-384 with SHA-384, matching the session CA key.
	sessionCredAlg = "ES384"
	sessionCredTyp = "otx-ingress" //nolint:gosec // G101: fixed token-type tag, not a credential; the secret is the signature.
)

// ErrSessionCredExpired reports that an ingress session credential verified
// cleanly but its ExpiresAt is at or before the verification instant.
var ErrSessionCredExpired = errors.New("ingress session credential expired")

// ErrSessionCredInvalid reports that an ingress session credential is
// malformed, was not signed by the trusted session CA, or has been tampered
// with. It never distinguishes those cases, so a verifier leaks nothing about
// why a forged token failed.
var ErrSessionCredInvalid = errors.New("ingress session credential invalid")

// SessionCredClaims is the offline-verifiable assertion the control plane mints
// to authorize one ingress session to one VM NIC. VMID, NICMAC, and GuestIP
// bind the credential to a single guest interface; the gateway re-checks the MAC
// binding against observed runtime state. Port names the guest port the session
// targets. LeaseEpoch is meaningful only on the owning-agent bridge path (which
// holds the lease table) and is carried opaquely for the gateway. ExpiresAt
// bounds the credential's validity window.
type SessionCredClaims struct {
	VMID       uuid.UUID
	NICMAC     string
	GuestIP    netip.Addr
	Port       int
	LeaseEpoch int64
	ExpiresAt  time.Time
}

// sessionCredHeader is the token's first segment: a fixed algorithm and type
// tag, decoded only to reject a credential minted under an unexpected scheme.
type sessionCredHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// sessionCredWire is the JSON shape of the claims segment. The snake_case tags
// are the stable on-wire contract; the exported SessionCredClaims is the Go-side
// view.
type sessionCredWire struct {
	VMID       uuid.UUID  `json:"vm_id"`
	NICMAC     string     `json:"nic_mac"`
	GuestIP    netip.Addr `json:"guest_ip"`
	Port       int        `json:"port"`
	LeaseEpoch int64      `json:"lease_epoch"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

// SignSessionCred mints a compact signed ingress credential for c, signed by the
// session CA signer over SHA-384. When c.ExpiresAt is the zero time it is
// stamped to now plus defaultSessionCredTTL; a non-zero ExpiresAt is honored
// verbatim so a caller may shorten the window. The returned token carries the
// "otx_ingress_" prefix and is safe to hand to a client as an opaque bearer.
func SignSessionCred(caSigner crypto.Signer, c SessionCredClaims, now time.Time) (token string, err error) {
	if caSigner == nil {
		return "", fmt.Errorf("sign session cred: nil signer")
	}
	exp := c.ExpiresAt
	if exp.IsZero() {
		exp = now.Add(defaultSessionCredTTL)
	}
	hdrJSON, err := json.Marshal(sessionCredHeader{Alg: sessionCredAlg, Typ: sessionCredTyp})
	if err != nil {
		return "", fmt.Errorf("sign session cred: marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(sessionCredWire{
		VMID:       c.VMID,
		NICMAC:     c.NICMAC,
		GuestIP:    c.GuestIP,
		Port:       c.Port,
		LeaseEpoch: c.LeaseEpoch,
		ExpiresAt:  exp.UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("sign session cred: marshal claims: %v", err)
	}
	signingInput := encodeSeg(hdrJSON) + "." + encodeSeg(claimsJSON)
	digest := sha512.Sum384([]byte(signingInput))
	sig, err := caSigner.Sign(rand.Reader, digest[:], crypto.SHA384)
	if err != nil {
		return "", fmt.Errorf("sign session cred: sign: %v", err)
	}
	return ingressCredPrefix + signingInput + "." + encodeSeg(sig), nil
}

// VerifySessionCred parses and verifies token against the session CA public half
// caPub, returning the carried claims. It returns ErrSessionCredInvalid for any
// shape, signature, or trust failure and ErrSessionCredExpired when the
// signature is valid but ExpiresAt is at or before now. The signature is checked
// before expiry so a tampered credential never masquerades as merely expired.
func VerifySessionCred(caPub crypto.PublicKey, token string, now time.Time) (SessionCredClaims, error) {
	pub, ok := caPub.(*ecdsa.PublicKey)
	if !ok {
		return SessionCredClaims{}, fmt.Errorf("%w: public key of type %T is not ECDSA", ErrSessionCredInvalid, caPub)
	}
	if !IsIngressCredFormat(token) {
		return SessionCredClaims{}, fmt.Errorf("%w: missing prefix", ErrSessionCredInvalid)
	}
	parts := strings.Split(strings.TrimPrefix(token, ingressCredPrefix), ".")
	if len(parts) != 3 {
		return SessionCredClaims{}, fmt.Errorf("%w: malformed token", ErrSessionCredInvalid)
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return SessionCredClaims{}, fmt.Errorf("%w: decode header: %v", ErrSessionCredInvalid, err)
	}
	var hdr sessionCredHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return SessionCredClaims{}, fmt.Errorf("%w: parse header: %v", ErrSessionCredInvalid, err)
	}
	if hdr.Alg != sessionCredAlg || hdr.Typ != sessionCredTyp {
		return SessionCredClaims{}, fmt.Errorf("%w: unexpected header alg %q typ %q", ErrSessionCredInvalid, hdr.Alg, hdr.Typ)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return SessionCredClaims{}, fmt.Errorf("%w: decode signature: %v", ErrSessionCredInvalid, err)
	}
	digest := sha512.Sum384([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return SessionCredClaims{}, fmt.Errorf("%w: signature mismatch", ErrSessionCredInvalid)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return SessionCredClaims{}, fmt.Errorf("%w: decode claims: %v", ErrSessionCredInvalid, err)
	}
	var w sessionCredWire
	if err := json.Unmarshal(claimsJSON, &w); err != nil {
		return SessionCredClaims{}, fmt.Errorf("%w: parse claims: %v", ErrSessionCredInvalid, err)
	}
	if w.ExpiresAt.IsZero() {
		return SessionCredClaims{}, fmt.Errorf("%w: missing expiry", ErrSessionCredInvalid)
	}
	if !now.Before(w.ExpiresAt) {
		return SessionCredClaims{}, fmt.Errorf("%w: at %s", ErrSessionCredExpired, w.ExpiresAt.Format(time.RFC3339))
	}
	return SessionCredClaims(w), nil
}

// IsIngressCredFormat reports whether s carries the ingress-credential prefix.
// The bearer dispatch uses this to route an ingress credential to the gateway
// path. It checks shape only, not validity. Because "otx_ingress_" is a superset
// of the API-token prefix "otx_", callers MUST check IsIngressCredFormat before
// IsAPITokenFormat, mirroring the SSH-grant dispatch discipline.
func IsIngressCredFormat(s string) bool {
	return strings.HasPrefix(s, ingressCredPrefix)
}

// encodeSeg base64url-encodes one token segment without padding.
func encodeSeg(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
