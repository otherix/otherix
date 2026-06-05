// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package resolver

import (
	"errors"
	"fmt"

	"github.com/otherix/otherix/internal/store"
)

// ErrorCode classifies a resolution failure. Callers map these to wire
// envelopes (typically 404 not_found for `not_found`, 500 internal for
// `internal`).
type ErrorCode string

const (
	// CodeNotFound — neither the UUID branch nor the name branch
	// returned a row. This is the catch-all "no such resource"
	// signal; cross-user / visibility leaks are handled by the
	// handler layer after the row loads.
	CodeNotFound ErrorCode = "not_found"

	// CodeInternal — the lookup hit an unexpected SQL error
	// (connection drop, malformed schema, etc). Handlers translate
	// this to 500 with a generic envelope; the underlying error is
	// kept on the chain for the logger.
	CodeInternal ErrorCode = "internal"

	// CodeAmbiguous — a name resolution succeeded against multiple
	// rows. Only emitted by Pool under the multi-instance model: the
	// same pool name may exist on more than one node, so requesting a
	// single instance by name alone is ambiguous when the cluster has
	// more than one matching row. Callers that need the per-node
	// instance must address it by UUID; callers that want the
	// aggregated view should use PoolByName instead.
	CodeAmbiguous ErrorCode = "ambiguous"

	// CodeUUIDInName — a UUID literal was supplied to a parameter that
	// accepts resource names only. Emitted by the strict resolver
	// helpers (Node, Pool, VM) before any DB roundtrip. The Pool
	// resolver retains UUID acceptance for the multi-instance carve-out
	// and never returns this code.
	CodeUUIDInName ErrorCode = "uuid_in_name"
)

// IsUUIDInName reports whether err is a resolver Error with
// Code=CodeUUIDInName. Handlers branch on this so the rejection path
// produces the canonical `validation_failed` envelope with the
// structured details payload from response.WriteUUIDNotAllowedError.
func IsUUIDInName(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodeUUIDInName
}

// Error is the structured failure value every Resolve function
// returns. Kind names the resource class, Identifier echoes the raw
// caller input (for the wire envelope's `details`), and Code lets the
// handler branch on not-found vs. internal without unwrapping. The
// underlying store error (if any) is held in cause so callers can
// errors.Is(err, store.ErrNotFound) when convenient.
type Error struct {
	Kind       Kind
	Identifier string
	Code       ErrorCode
	cause      error
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("resolver: %s %q: %s", e.Kind, e.Identifier, e.Code)
}

// Unwrap exposes the underlying SQL error for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.cause }

// IsNotFound reports whether err is a resolver Error with
// Code=CodeNotFound. Use this rather than testing for store.ErrNotFound
// directly — the resolver may have promoted a different state to
// not-found (e.g. soft-deleted rows look like missing rows here).
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodeNotFound
}

// wrapLookupErr converts an error returned by a store lookup method to
// a resolver Error. store.ErrNotFound promotes to CodeNotFound;
// anything else stays as CodeInternal with the cause preserved for
// logging.
func wrapLookupErr(kind Kind, identifier string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return &Error{Kind: kind, Identifier: identifier, Code: CodeNotFound, cause: err}
	}
	return &Error{Kind: kind, Identifier: identifier, Code: CodeInternal, cause: err}
}
