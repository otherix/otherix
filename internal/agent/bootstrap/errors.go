// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"errors"
	"fmt"
)

// ErrCSRRejected wraps a non-2xx response from /v1/nodes/join. Callers
// can use errors.Is(err, ErrCSRRejected) to dispatch on the CSR
// rejection case (operator-facing message includes the server's
// HTTP status and error envelope code).
var ErrCSRRejected = errors.New("bootstrap: CSR submission rejected by CP")

// FingerprintMismatchError is returned when the CA fetched from
// /v1/ca does not match the operator-pinned fingerprint, OR when the
// CA returned alongside the issued cert in /v1/nodes/join does not
// match the same pin (defense-in-depth re-verification).
//
// Both Expected and Computed are lowercase hex strings — operators can
// paste them back-to-back into a terminal to see exactly what diverged.
type FingerprintMismatchError struct {
	Expected string
	Computed string
}

// Error reports the divergence in an operator-actionable form. The
// "possible MITM or operator typo" framing keeps the message
// honest about both failure modes (a typo is the more common cause
// in practice, but the security-critical case must remain visible).
func (e *FingerprintMismatchError) Error() string {
	return fmt.Sprintf(
		"bootstrap: CA fingerprint mismatch — possible MITM or operator typo (expected %s, got %s)",
		e.Expected, e.Computed,
	)
}
