// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "context"

// ActiveCACert returns the active cluster CA row, or ErrNotFound when no
// active CA has been provisioned. In production the boot-time
// BootstrapClusterCA hook guarantees exactly one active row exists, so
// ErrNotFound here signals an operational invariant violation that
// callers surface as 500.
func (s *Store) ActiveCACert(ctx context.Context) (CaCert, error) {
	row, err := s.queries.GetActiveCACert(ctx)
	if err != nil {
		return CaCert{}, translateNoRows(err)
	}
	return row, nil
}
