// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// isUnique returns true if err is a PostgreSQL unique-violation (SQLSTATE 23505).
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
