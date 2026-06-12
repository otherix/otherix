// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// advertisedEndpointMaxLength bounds nodes.advertised_endpoint. 2048 is the
// pragmatic URL length most servers accept; the column itself is
// untyped text.
const advertisedEndpointMaxLength = 2048

// migrationHostMaxLength bounds nodes.migration_host (hostname or IP
// literal of the agent's migration ingress). 253 covers DNS hostnames
// and IPv6 literals comfortably.
const migrationHostMaxLength = 253

// minMigrationPort and maxMigrationPort enforce the SQL-level
// nodes_migration_port_range_valid CHECK constraint at the API edge so
// validation errors surface as 400, not 500.
const (
	minMigrationPort = 1024
	maxMigrationPort = 65535
)

// Create implements POST /v1/nodes. Required permission: node:manage
// (admin only). Pre-registers a node row so an agent can later join via
// mTLS / join-token. The row starts in `pending` status; capabilities
// (cpu_*, memory_*, labels) populate from the heartbeat.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	if err := validateCreate(req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	row, err := h.store.CreateNode(r.Context(), store.CreateNodeParams{
		ID: uuid.New(),
		// Persist the same trimmed value validateCreate vetted - the DNS
		// label guarantee must hold for the stored name, not just the
		// validated copy.
		Name:                    strings.TrimSpace(req.Name),
		Architecture:            store.CPUArch(req.Architecture),
		AdvertisedEndpoint:      req.AdvertisedEndpoint,
		MigrationHost:           req.MigrationHost,
		MigrationPortRangeStart: req.MigrationPortRangeStart,
		MigrationPortRangeEnd:   req.MigrationPortRangeEnd,
		Status:                  store.NodeStatusPending,
	})
	if err != nil {
		if errors.Is(err, store.ErrNodeNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "node name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist node", nil)
		return
	}

	writeNodeResponse(w, r, http.StatusCreated, row, response.WriteJSON)
}

// validateCreate enforces the API-edge invariants. Order is biased
// toward returning the most specific error first.
func validateCreate(req createRequest) error {
	// The name flows into the issued agent cert CN `node-<name>` and SAN
	// `node-<name>.agents.otherix.local`, so it is constrained to a
	// lowercase RFC 1123 DNS label (audit LOW).
	if err := validation.ValidateNodeName(strings.TrimSpace(req.Name)); err != nil {
		return err
	}

	switch req.Architecture {
	case string(store.CpuArchAmd64), string(store.CpuArchArm64):
	default:
		return errors.New("architecture must be one of: amd64, arm64")
	}

	endpoint := strings.TrimSpace(req.AdvertisedEndpoint)
	switch {
	case endpoint == "":
		return errors.New("advertised_endpoint is required")
	case len(endpoint) > advertisedEndpointMaxLength:
		return errors.New("advertised_endpoint is too long")
	}

	host := strings.TrimSpace(req.MigrationHost)
	switch {
	case host == "":
		return errors.New("migration_host is required")
	case len(host) > migrationHostMaxLength:
		return errors.New("migration_host is too long")
	}

	switch {
	case req.MigrationPortRangeStart < minMigrationPort,
		req.MigrationPortRangeStart > maxMigrationPort:
		return errors.New("migration_port_range_start must be between 1024 and 65535")
	case req.MigrationPortRangeEnd < minMigrationPort,
		req.MigrationPortRangeEnd > maxMigrationPort:
		return errors.New("migration_port_range_end must be between 1024 and 65535")
	case req.MigrationPortRangeEnd < req.MigrationPortRangeStart:
		return errors.New("migration_port_range_end must be >= migration_port_range_start")
	}

	return nil
}
