// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

import (
	"errors"
	"net/url"
	"strings"

	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Validation sentinels surfaced as 400 validation_failed envelopes by
// the handler. Each carries a distinct details key for operator
// debugging.
var (
	errMissingToken                 = errors.New("token is required")
	errMissingCSR                   = errors.New("csr_pem is required")
	errMissingNodeName              = errors.New("node_name is required")
	errMissingArchitecture          = errors.New("architecture is required")
	errInvalidArchitecture          = errors.New("architecture must be one of: amd64, arm64")
	errMissingAdvertisedEndpoint    = errors.New("advertised_endpoint is required")
	errAdvertisedEndpointTooLong    = errors.New("advertised_endpoint must be at most 2048 characters")
	errAdvertisedEndpointInvalidURL = errors.New("advertised_endpoint must be an https URL with a host")
	errMissingMigrationHost         = errors.New("migration_host is required")
	errMigrationHostTooLong         = errors.New("migration_host must be at most 253 characters")
	errMigrationPortInvalid         = errors.New("migration_port_range must lie in [1024, 65535] with end >= start")
)

// Bounds mirror the existing nodes.create handler — same column
// shape, same edge constraints.
const (
	advertisedEndpointMaxLength = 2048
	migrationHostMaxLength      = 253
	minMigrationPort            = 1024
	maxMigrationPort            = 65535
)

// validate normalises and validates the request body. Returns a
// sentinel error suitable for direct status mapping. Whitespace-only
// fields collapse to empty string and are treated as missing. The
// pointer receiver lets validate normalise req.NodeName in place: the
// trimmed, charset-validated name is what redeem() then feeds into both
// the node row and SignCSR (cert CN `node-<name>` + SAN), so the value
// that is VALIDATED is the value that is SIGNED - no padded-vs-vetted
// drift (e.g. " node-1 " validating as node-1 but signed untrimmed).
func (req *joinRequest) validate() error {
	if strings.TrimSpace(req.Token) == "" {
		return errMissingToken
	}
	if strings.TrimSpace(req.CSRPEM) == "" {
		return errMissingCSR
	}

	name := strings.TrimSpace(req.NodeName)
	if name == "" {
		return errMissingNodeName
	}
	// The redeemed name becomes the server-authoritative cert CN
	// `node-<name>` + SAN, so it must be a lowercase RFC 1123 DNS label
	// (audit LOW). Persist the trimmed value back so the signed name
	// equals the validated name.
	if err := validation.ValidateNodeName(name); err != nil {
		return err
	}
	req.NodeName = name

	arch := strings.TrimSpace(req.Architecture)
	switch {
	case arch == "":
		return errMissingArchitecture
	case arch != string(store.CpuArchAmd64) && arch != string(store.CpuArchArm64):
		return errInvalidArchitecture
	}

	if err := validateAdvertisedEndpoint(req.AdvertisedEndpoint); err != nil {
		return err
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

// validateAdvertisedEndpoint enforces that advertised_endpoint is present,
// within bounds, and an https URL with a host. The endpoint feeds the agent
// serving-cert SAN at sign time, so a malformed value is rejected up front.
// Whitespace-only collapses to empty and is treated as missing.
func validateAdvertisedEndpoint(raw string) error {
	endpoint := strings.TrimSpace(raw)
	switch {
	case endpoint == "":
		return errMissingAdvertisedEndpoint
	case len(endpoint) > advertisedEndpointMaxLength:
		return errAdvertisedEndpointTooLong
	}
	if u, err := url.Parse(endpoint); err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errAdvertisedEndpointInvalidURL
	}
	return nil
}

// validationMessage maps a sentinel to its operator-facing message
// for inclusion in the 400 envelope. Returns the error's own .Error()
// when the value isn't a recognised sentinel (defensive fallback).
func validationMessage(err error) string {
	return err.Error()
}
