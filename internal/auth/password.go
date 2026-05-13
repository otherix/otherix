// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters tuned to the OWASP password-storage cheat sheet
// (m=19 MiB, t=2, p=1, keyLen=32). Salt is 16 bytes from crypto/rand.
//
// argonTime / argonMemory / argonThreads are package vars (not const)
// so an integration-test build under `-tags=test_fast_argon` can swap
// them for cheap parameters via init() in argon_fast.go. The PHC-format
// string returned by HashPassword encodes whatever values were active
// at hash time, and VerifyPassword reads them back from the string —
// so the override is invisible to round-trip semantics. Production
// builds (no build tag) always use the OWASP defaults below.
//
// argonKeyLen and argonSaltLen are output sizes, not cost parameters,
// and never change.
var (
	argonTime    uint32 = 2
	argonMemory  uint32 = 19 * 1024
	argonThreads uint8  = 1
)

const (
	argonKeyLen  uint32 = 32
	argonSaltLen int    = 16
)

// HashPassword returns plaintext hashed in PHC argon2id format:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<hash-b64>
//
// An empty plaintext is rejected; this prevents accidentally creating
// users with no usable password via the same code path as legitimate
// password changes.
func HashPassword(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("empty password")
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %v", err)
	}

	hash := argon2.IDKey([]byte(plaintext), salt,
		argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether plaintext matches stored, an
// argon2id PHC hash. A return of (false, nil) is the normal mismatch
// case; a non-nil error indicates a malformed stored hash and never
// constitutes "match". Comparison is constant-time over the digest
// portion.
func VerifyPassword(stored, plaintext string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("malformed argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("parse argon2 version: %v", err)
	}
	if version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version: %d", version)
	}

	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("parse argon2 params: %v", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode salt: %v", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("decode hash: %v", err)
	}

	// Bound the digest length before passing it to argon2.IDKey: a
	// legitimate argon2id digest is at most 64 bytes, so anything over
	// 1 KiB is malformed and rejected. The bound also lets gosec see
	// that the int → uint32 conversion below is safe.
	if len(want) > 1024 {
		return false, fmt.Errorf("argon2id digest too long: %d bytes", len(want))
	}
	got := argon2.IDKey([]byte(plaintext), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // G115: bounded above
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
