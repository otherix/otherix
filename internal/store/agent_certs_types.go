// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateAgentCertParams struct {
	ID                uuid.UUID
	NodeID            uuid.UUID
	Serial            []byte
	FingerprintSha256 []byte
	SubjectDn         string
	NotBefore         time.Time
	NotAfter          time.Time
}

type LookupAgentCertByFingerprintRow struct {
	NodeID    uuid.UUID
	RevokedAt *time.Time
}
