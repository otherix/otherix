// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/x509"
	"net"
	"time"
)

// PeerCertValidity is the lifetime of a freshly generated etcd peer
// (Raft) cert. 1 year matches the CP-replica and Step 2 agent leaf certs;
// peer certs are regenerated on each api-server boot from the on-disk
// cluster CA, so the window only bounds an idle-but-running member.
const PeerCertValidity = 365 * 24 * time.Hour

// GeneratePeerCert mints the etcd peer (Raft) mTLS leaf, signed by the
// cluster CA. A single cert serves both directions of the peer handshake
// (serverAuth+clientAuth) because etcd dials and accepts peers with the
// same identity. The SAN set must cover the member's advertised peer host
// so other members validate the connection; pass at least one DNS name or
// IP.
//
// Subject CN is "otherix-cp-peer" - distinct from the CP-replica leaf so
// the two identities are separable in operator-facing TLS debugging. The
// CA verifies chain-of-trust only; etcd does not key authorization on the
// peer CN.
func GeneratePeerCert(caCert *x509.Certificate, caKey crypto.Signer, dnsNames []string, ips []net.IP, validity time.Duration, now time.Time) (certPEM, keyPEM []byte, err error) {
	if validity <= 0 {
		validity = PeerCertValidity
	}
	return signLeafCert(caCert, caKey, "otherix-cp-peer", dnsNames, ips, validity, now)
}
