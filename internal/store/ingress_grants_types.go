// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// IngressGrantVM is one VM a grant authorizes: the guest TCP ports the recipient
// may reach on it, plus the guest login the recipient connects as. Ports is the
// closed-by-default reach set (a grant reaches only the ports it lists). The
// login is recorded for the SSH-cert path (the port-22 principal); the guest
// sshd owns login policy, there is no control-plane allow-list per VM.
type IngressGrantVM struct {
	VMName string `json:"vm_name"`
	Ports  []int  `json:"ports"`
	Login  string `json:"login,omitempty"`
}

// IngressGrant is a revocable, per-person access grant an external SSH-only user
// presents at connect time. The token is stored as TokenHash (sha256 of the
// opaque plaintext) and indexed for the connect-time lookup. VMs is a
// server-mutable set edited by AddIngressGrantVM / RemoveIngressGrantVM. Revoked keeps
// the row (and its token index) so audit and a deterministic uniform reject
// survive a revoke.
type IngressGrant struct {
	ID             uuid.UUID        `json:"id"`
	Name           string           `json:"name"`
	CreatedBy      uuid.UUID        `json:"created_by"`
	RecipientLabel string           `json:"recipient_label"`
	TokenHash      []byte           `json:"token_hash"`
	VMs            []IngressGrantVM `json:"vms"`
	ExpiresAt      *time.Time       `json:"expires_at"`
	Revoked        bool             `json:"revoked"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// CreateIngressGrantParams is the input to Store.CreateIngressGrant. The store mints the
// id and stamps created_at/updated_at.
type CreateIngressGrantParams struct {
	Name           string
	CreatedBy      uuid.UUID
	RecipientLabel string
	TokenHash      []byte
	VMs            []IngressGrantVM
	ExpiresAt      *time.Time
}

// ListIngressGrantsParams is the input to Store.ListIngressGrants. Cursor pagination per
// ADR 0019: CursorCreatedAt/CursorID are nil for the first page.
type ListIngressGrantsParams struct {
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}
