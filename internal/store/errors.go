// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by the domain methods on *Store when the
// requested row does not exist. It replaces the driver-level
// pgx.ErrNoRows at the store boundary so callers depend on the store
// package rather than on pgx directly. Match it with errors.Is.
var ErrNotFound = errors.New("store: not found")

// ResourceInUseError reports that a resource cannot be deleted because
// other rows still reference it. Resources maps each blocking kind
// (e.g. "vms", "templates") to its count and is non-empty. The
// producing store method decides whether to include zero-count kinds:
// most include only non-zero entries, but a resource whose API envelope
// always lists a fixed set of kinds (e.g. nodes -> vms +
// active_migrations) may populate every key. Handlers project it onto
// the API blocking_resources envelope. Shared across every resource
// whose delete is gated on dependants so the carrier stays uniform.
type ResourceInUseError struct {
	Resources map[string]int64
}

func (e *ResourceInUseError) Error() string {
	return fmt.Sprintf("resource in use: %v", e.Resources)
}

// translateNoRows maps pgx.ErrNoRows to ErrNotFound and passes any
// other error through unchanged. It is the single point where the
// "missing row" driver error becomes a store-level sentinel.
func translateNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
