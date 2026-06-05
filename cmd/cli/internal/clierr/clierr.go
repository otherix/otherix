// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package clierr maps cpclient errors to the CLI's stable
// "<category>: <detail>" prefix contract that operator scripts grep on.
package clierr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Classify maps an error from cpclient to the operator-facing
// "<category>: <detail>" form. APIError surfaces its own code-bearing
// message; transport failures get a stable category prefix
// (request_timeout, connection_refused, tls_handshake_failed) so shell
// scripts can branch on them; anything else falls back to
// request_failed. Returns an error so callers can `return Classify(err)`.
func Classify(err error) error {
	var apiErr *cpclient.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%s", apiErr.Error())
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("request_timeout: %s", msg)
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("connection_refused: %s", msg)
	case strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:"):
		return fmt.Errorf("tls_handshake_failed: %s", msg)
	default:
		return fmt.Errorf("request_failed: %s", msg)
	}
}
