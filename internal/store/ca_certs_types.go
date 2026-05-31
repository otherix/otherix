// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateCACertParams struct {
	ID                uuid.UUID
	CertPem           []byte
	KeyPem            []byte
	FingerprintSha256 []byte
	NotBefore         time.Time
	NotAfter          time.Time
}
