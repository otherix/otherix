// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import "crypto/sha256"

// HashToken returns the sha256 digest of plaintext as bytea. The
// digest is the storage form for every opaque token Otherix mints —
// API tokens (`otx_*`), refresh tokens, и join tokens
// (`otx_join_*`). All three callers store sha256(plaintext) so
// lookup-by-hash is а constant-time string compare on equal digests
// и no per-token salt is needed (the plaintext already carries 256
// bits of entropy from crypto/rand). Centralising the hash here
// keeps the three flows from drifting apart.
func HashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}
