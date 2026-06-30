// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// SSHGrantVM is one VM a grant authorizes, with the guest login the recipient
// connects as. The login is recorded for convenience only - the guest sshd owns
// login policy; there is no control-plane allow-list per VM.
type SSHGrantVM struct {
	VMName string `json:"vm_name"`
	Login  string `json:"login"`
}

// SSHGrant is a revocable, per-person access grant an external SSH-only user
// presents at connect time. The token is stored as TokenHash (sha256 of the
// opaque plaintext) and indexed for the connect-time lookup. VMs is a
// server-mutable set edited by AddSSHGrantVM / RemoveSSHGrantVM. Revoked keeps
// the row (and its token index) so audit and a deterministic uniform reject
// survive a revoke.
type SSHGrant struct {
	ID             uuid.UUID    `json:"id"`
	Name           string       `json:"name"`
	CreatedBy      uuid.UUID    `json:"created_by"`
	RecipientLabel string       `json:"recipient_label"`
	TokenHash      []byte       `json:"token_hash"`
	VMs            []SSHGrantVM `json:"vms"`
	ExpiresAt      *time.Time   `json:"expires_at"`
	Revoked        bool         `json:"revoked"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// CreateSSHGrantParams is the input to Store.CreateSSHGrant. The store mints the
// id and stamps created_at/updated_at.
type CreateSSHGrantParams struct {
	Name           string
	CreatedBy      uuid.UUID
	RecipientLabel string
	TokenHash      []byte
	VMs            []SSHGrantVM
	ExpiresAt      *time.Time
}

// ListSSHGrantsParams is the input to Store.ListSSHGrants. Cursor pagination per
// ADR 0019: CursorCreatedAt/CursorID are nil for the first page.
type ListSSHGrantsParams struct {
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}
