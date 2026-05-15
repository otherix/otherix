// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"errors"
	"fmt"
)

// ErrCSRRejected wraps а non-2xx response от /v1/nodes/join. Callers
// can use errors.Is(err, ErrCSRRejected) к dispatch на the CSR
// rejection case (operator-facing message includes the server's
// HTTP status и error envelope code).
var ErrCSRRejected = errors.New("bootstrap: CSR submission rejected by CP")

// FingerprintMismatchError is returned when the CA fetched от
// /v1/ca does not match the operator-pinned fingerprint, OR when the
// CA returned alongside the issued cert в /v1/nodes/join does не
// match the same pin (defense-in-depth re-verification).
//
// Both Expected и Computed are lowercase hex strings — operators can
// paste them back-to-back into а terminal к see exactly what diverged.
type FingerprintMismatchError struct {
	Expected string
	Computed string
}

// Error reports the divergence в an operator-actionable form. The
// "possible MITM или operator typo" framing keeps the message
// honest about both failure modes (а typo is the more common cause
// в practice, but the security-critical case must remain visible).
func (e *FingerprintMismatchError) Error() string {
	return fmt.Sprintf(
		"bootstrap: CA fingerprint mismatch — possible MITM или operator typo (expected %s, got %s)",
		e.Expected, e.Computed,
	)
}
