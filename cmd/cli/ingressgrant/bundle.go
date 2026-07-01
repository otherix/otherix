// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrant

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// BundleVersion is the schema version stamped into every emitted bundle. The
// external connector (`otherix-ssh add`, the consumer of this artifact)
// rejects an unrecognised version rather than mis-parsing a future shape.
const BundleVersion = "otherix-ingress-grant/v1"

// bundleBlobPrefix wraps the base64url-encoded bundle JSON into a single
// paste-able token. The operator copies one opaque line and sends it to the
// external person, who feeds it to `otherix-ssh add`. The prefix makes the
// blob self-identifying and lets the connector distinguish it from a raw JSON
// document on stdin.
const bundleBlobPrefix = "otx_ingressbundle_"

// Trust discriminator values. They map one-to-one onto the connector's
// sshconn.Config TLS-trust modes so the external reaches the same Control
// Plane the operator trusts:
//
//   - TrustWebPKI   -> system root store (empty trust).
//   - TrustCABundle -> the CACertPEM field is the sole RootCAs pool
//     (sshconn.Config.CACertPEM); the cluster CA per ADR 0026.
//   - TrustPinPrefix + hex -> leaf-pin by sha256(cert.Raw)
//     (sshconn.Config.CAFingerprint); the value is "pin:<64 hex chars>".
//   - TrustInsecure -> disable verification (sshconn.Config.InsecureSkipTLSVerify).
//
// `ingress-grant create` derives the value from the operator's OWN resolved CLI
// trust, so the bundle carries exactly what the external needs to trust the
// same CP. The pin form is never emitted by create (the operator config
// carries a CA bundle, not a leaf fingerprint) but is a valid value the
// connector accepts.
const (
	TrustWebPKI    = "webpki"
	TrustCABundle  = "ca-bundle"
	TrustInsecure  = "insecure"
	TrustPinPrefix = "pin:"
)

// BundleVM is one granted {vm, login} pair in a bundle.
type BundleVM struct {
	VM    string `json:"vm"`
	Login string `json:"login"`
}

// Bundle is the shareable artifact `ingress-grant create` prints and the external
// connector `otherix-ssh add` consumes. It is a stable, versioned JSON
// document carrying everything the connector needs: the Control Plane URL, a
// TLS-trust discriminator (+ the CA bundle when trust is ca-bundle), the
// one-time grant token, and the granted {vm, login} set. The encoded
// single-line form is `otx_ingressbundle_<base64url(compact JSON)>`.
type Bundle struct {
	Version   string     `json:"version"`
	ServerURL string     `json:"server_url"`
	Trust     string     `json:"trust"`
	CACertPEM string     `json:"ca_cert_pem,omitempty"`
	Token     string     `json:"token"`
	VMs       []BundleVM `json:"vms"`
}

// EncodeBundle renders b as the single-line paste-able blob
// `otx_ingressbundle_<base64url(compact JSON)>`.
func EncodeBundle(b Bundle) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("marshal bundle: %v", err)
	}
	return bundleBlobPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// ParseBundle decodes either the single-line blob form
// (`otx_ingressbundle_<base64url>`) or a raw JSON bundle document, validates the
// version and the required fields, and returns the Bundle. The connector
// `otherix-ssh add` calls this on the operator-supplied artifact.
func ParseBundle(s string) (Bundle, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	if rest, ok := strings.CutPrefix(s, bundleBlobPrefix); ok {
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return Bundle{}, fmt.Errorf("decode bundle blob: %v", err)
		}
		raw = decoded
	} else {
		raw = []byte(s)
	}

	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bundle{}, fmt.Errorf("decode bundle: %v", err)
	}
	if b.Version != BundleVersion {
		return Bundle{}, fmt.Errorf("unsupported bundle version %q (want %q)", b.Version, BundleVersion)
	}
	if b.ServerURL == "" {
		return Bundle{}, errors.New("bundle is missing server_url")
	}
	if b.Token == "" {
		return Bundle{}, errors.New("bundle is missing token")
	}
	return b, nil
}

// ResolveTrust decodes the Bundle's trust discriminator into the three inputs
// the connector's sshconn.Config consumes: the CA-bundle PEM, the leaf-pin
// fingerprint, and the insecure-skip flag. Exactly one (or none, for webpki)
// is set. The connector maps the result onto sshconn.Config.{CACertPEM,
// CAFingerprint, InsecureSkipTLSVerify}.
func (b Bundle) ResolveTrust() (caCertPEM []byte, fingerprint string, insecure bool, err error) {
	switch {
	case b.Trust == TrustWebPKI || b.Trust == "":
		return nil, "", false, nil
	case b.Trust == TrustInsecure:
		return nil, "", true, nil
	case b.Trust == TrustCABundle:
		pem, derr := base64.StdEncoding.DecodeString(b.CACertPEM)
		if derr != nil {
			return nil, "", false, fmt.Errorf("decode bundle ca_cert_pem: %v", derr)
		}
		if len(pem) == 0 {
			return nil, "", false, errors.New("bundle trust is ca-bundle but ca_cert_pem is empty")
		}
		return pem, "", false, nil
	case strings.HasPrefix(b.Trust, TrustPinPrefix):
		fp := strings.TrimSpace(strings.TrimPrefix(b.Trust, TrustPinPrefix))
		if fp == "" {
			return nil, "", false, errors.New("bundle trust is pin: but no fingerprint follows")
		}
		return nil, fp, false, nil
	default:
		return nil, "", false, fmt.Errorf("unrecognised bundle trust %q", b.Trust)
	}
}
