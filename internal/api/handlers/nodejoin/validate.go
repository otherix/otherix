// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

import (
	"errors"
	"strings"

	"github.com/otherix/otherix/internal/store"
)

// Validation sentinels surfaced as 400 validation_failed envelopes by
// the handler. Each carries a distinct details key for operator
// debugging.
var (
	errMissingToken              = errors.New("token is required")
	errMissingCSR                = errors.New("csr_pem is required")
	errMissingNodeName           = errors.New("node_name is required")
	errNodeNameTooLong           = errors.New("node_name must be at most 253 characters")
	errMissingArchitecture       = errors.New("architecture is required")
	errInvalidArchitecture       = errors.New("architecture must be one of: amd64, arm64")
	errMissingAdvertisedEndpoint = errors.New("advertised_endpoint is required")
	errAdvertisedEndpointTooLong = errors.New("advertised_endpoint must be at most 2048 characters")
	errMissingMigrationHost      = errors.New("migration_host is required")
	errMigrationHostTooLong      = errors.New("migration_host must be at most 253 characters")
	errMigrationPortInvalid      = errors.New("migration_port_range must lie in [1024, 65535] with end >= start")
)

// Bounds mirror the existing nodes.create handler — same column
// shape, same edge constraints.
const (
	nodeNameMaxLength           = 253
	advertisedEndpointMaxLength = 2048
	migrationHostMaxLength      = 253
	minMigrationPort            = 1024
	maxMigrationPort            = 65535
)

// validate normalises and validates the request body. Returns the
// canonicalised values + a sentinel error suitable for direct status
// mapping. Whitespace-only fields collapse to empty string and are
// treated as missing.
func (req joinRequest) validate() error {
	if strings.TrimSpace(req.Token) == "" {
		return errMissingToken
	}
	if strings.TrimSpace(req.CSRPEM) == "" {
		return errMissingCSR
	}

	name := strings.TrimSpace(req.NodeName)
	switch {
	case name == "":
		return errMissingNodeName
	case len(name) > nodeNameMaxLength:
		return errNodeNameTooLong
	}

	arch := strings.TrimSpace(req.Architecture)
	switch {
	case arch == "":
		return errMissingArchitecture
	case arch != string(store.CpuArchAmd64) && arch != string(store.CpuArchArm64):
		return errInvalidArchitecture
	}

	endpoint := strings.TrimSpace(req.AdvertisedEndpoint)
	switch {
	case endpoint == "":
		return errMissingAdvertisedEndpoint
	case len(endpoint) > advertisedEndpointMaxLength:
		return errAdvertisedEndpointTooLong
	}

	host := strings.TrimSpace(req.MigrationHost)
	switch {
	case host == "":
		return errMissingMigrationHost
	case len(host) > migrationHostMaxLength:
		return errMigrationHostTooLong
	}

	switch {
	case req.MigrationPortRangeStart < minMigrationPort,
		req.MigrationPortRangeStart > maxMigrationPort,
		req.MigrationPortRangeEnd < minMigrationPort,
		req.MigrationPortRangeEnd > maxMigrationPort,
		req.MigrationPortRangeEnd < req.MigrationPortRangeStart:
		return errMigrationPortInvalid
	}

	return nil
}

// validationMessage maps a sentinel to its operator-facing message
// for inclusion in the 400 envelope. Returns the error's own .Error()
// when the value isn't a recognised sentinel (defensive fallback).
func validationMessage(err error) string {
	return err.Error()
}
