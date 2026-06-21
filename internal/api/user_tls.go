// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"crypto/tls"
	"errors"
)

// buildUserServerTLSConfig returns the server-side TLS config for the
// user-facing /v1 listener: it presents the replica's CP leaf cert and
// pins TLS 1.3, but does NOT verify client certs - users authenticate
// with Bearer tokens, not mTLS (that is the agent listener's job). The
// caller guarantees material carries a non-empty leaf.
func buildUserServerTLSConfig(material TLSMaterial) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{material.Cert},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.NoClientCert,
	}
}

// userListenerTLSConfig decides the user listener's transport from the
// config flag and the available cert material:
//
//   - disabled            -> (nil, nil): the listener stays plaintext.
//   - enabled, no material -> (nil, error): fatal at boot. server.tls
//     is on but LoadOrGenerateCPCert produced nothing, which means the
//     cp_cert block / cluster CA are misconfigured.
//   - enabled, material    -> (config, nil): server-side TLS.
func userListenerTLSConfig(enabled bool, material TLSMaterial) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}
	if material.Skipped() || len(material.Cert.Certificate) == 0 {
		return nil, errors.New("server.tls.enabled=true but no CP cert material was produced - configure cp_cert.cert_file/key_file or ensure the cluster CA is available for auto-generation")
	}
	return buildUserServerTLSConfig(material), nil
}
