// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrClusterCAAbsent reports that no cluster CA exists on disk and the
// caller did not permit generation. A joining replica hits this until it
// has fetched the CA from the running cluster and persisted it: the peer
// (Raft) transport needs a CA-signed cert before its member starts, so a
// join node must obtain the shared CA rather than mint a divergent one.
var ErrClusterCAAbsent = errors.New("cluster CA absent on disk")

// LoadOrGenerateClusterCAOnDisk provisions the cluster CA as a filesystem
// artifact, consumed by the peer-mTLS plane before etcd starts. It loads
// the CA (cert + key) from certPath/keyPath when both files exist, or -
// when both are absent and allowGenerate is true - generates a fresh
// ECDSA P-384 CA and persists it atomically (cert 0644, key 0600).
//
// The returned bool reports whether the CA was freshly generated (true)
// or loaded from existing files (false), for operator-facing logging.
//
// Errors:
//   - both files absent and allowGenerate is false: ErrClusterCAAbsent.
//     A bootstrap / single node passes allowGenerate=true; a join node
//     passes false so a missing CA fails fast instead of forking a new
//     trust root.
//   - exactly one of cert / key present: a partial state error - the CA
//     is corrupt; the operator must remove both files to re-provision.
func LoadOrGenerateClusterCAOnDisk(certPath, keyPath string, allowGenerate bool, now time.Time) (ClusterCAResult, bool, error) {
	certExists, err := fileExists(certPath)
	if err != nil {
		return ClusterCAResult{}, false, fmt.Errorf("stat cluster CA cert %s: %v", certPath, err)
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return ClusterCAResult{}, false, fmt.Errorf("stat cluster CA key %s: %v", keyPath, err)
	}

	switch {
	case certExists && keyExists:
		res, loadErr := loadClusterCAFromDisk(certPath, keyPath)
		if loadErr != nil {
			return ClusterCAResult{}, false, loadErr
		}
		return res, false, nil
	case certExists != keyExists:
		return ClusterCAResult{}, false, fmt.Errorf(
			"cluster CA on disk is in a partial state (cert present=%v, key present=%v); remove both to re-provision",
			certExists, keyExists)
	}

	// Neither file present.
	if !allowGenerate {
		return ClusterCAResult{}, false, ErrClusterCAAbsent
	}
	res, err := GenerateClusterCA(now)
	if err != nil {
		return ClusterCAResult{}, false, fmt.Errorf("generate cluster CA: %v", err)
	}
	if err := WriteCertCacheAtomic(certPath, keyPath, res.CertPEM, res.KeyPEM); err != nil {
		return ClusterCAResult{}, false, fmt.Errorf("persist cluster CA: %v", err)
	}
	return res, true, nil
}

// loadClusterCAFromDisk reads and validates an existing on-disk CA,
// reconstructing the ClusterCAResult (including the recomputed
// fingerprint and the cert's own validity window) so the load path
// returns the same shape as the generate path.
func loadClusterCAFromDisk(certPath, keyPath string) (ClusterCAResult, error) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // operator-configured CA path; this function exists to read it
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("read cluster CA cert %s: %v", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // operator-configured CA path; this function exists to read it
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("read cluster CA key %s: %v", keyPath, err)
	}
	cert, der, err := ParseClusterCACert(certPEM)
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("parse on-disk cluster CA cert: %v", err)
	}
	key, err := ParseClusterCAKey(keyPEM)
	if err != nil {
		return ClusterCAResult{}, fmt.Errorf("parse on-disk cluster CA key: %v", err)
	}
	// The cert and key are parsed independently, so confirm they actually pair -
	// a mismatched on-disk pair (wrong key copied, partial restore) would
	// otherwise load cleanly and sign peer/leaf certs that do not chain to the
	// published CA cert, surfacing far downstream as opaque TLS errors.
	if err := keyMatchesCert(key, cert.PublicKey); err != nil {
		return ClusterCAResult{}, fmt.Errorf("on-disk cluster CA: %v", err)
	}
	fp := sha256.Sum256(der)
	return ClusterCAResult{
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		Fingerprint: fp[:],
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
	}, nil
}

// KeyMatchesCert reports an error unless the private key's public part equals
// certPub - confirming a parsed key and cert actually pair. key must implement
// crypto.Signer (every key ParseClusterCAKey returns does).
func KeyMatchesCert(key any, certPub crypto.PublicKey) error {
	return keyMatchesCert(key, certPub)
}

func keyMatchesCert(key any, certPub crypto.PublicKey) error {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return errors.New("key does not implement crypto.Signer")
	}
	type equalable interface {
		Equal(crypto.PublicKey) bool
	}
	pub, ok := signer.Public().(equalable)
	if !ok {
		return errors.New("key public part is not comparable")
	}
	if !pub.Equal(certPub) {
		return errors.New("key does not match cert public key")
	}
	return nil
}

// fileExists reports whether path exists, distinguishing a genuine
// absence (false, nil) from a stat error worth surfacing (false, err).
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
