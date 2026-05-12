// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

// createRequest is the body of POST /v1/nodes. Mirrors NodeCreate in
// api/openapi/control-plane.yaml. Heartbeat-driven fields (cpu_*,
// memory_*, capabilities, labels, …) are intentionally absent — they
// land via the agent heartbeat path, not by admin request.
type createRequest struct {
	Name                    string `json:"name"`
	Architecture            string `json:"architecture"`
	AdvertisedEndpoint      string `json:"advertised_endpoint"`
	MigrationHost           string `json:"migration_host"`
	MigrationPortRangeStart int32  `json:"migration_port_range_start"`
	MigrationPortRangeEnd   int32  `json:"migration_port_range_end"`
}

// listResponse is the payload for GET /v1/nodes. The data slice carries
// either nodeView or nodeSummaryView per the caller's role; a single
// any slice keeps the JSON encoder honest.
type listResponse struct {
	Data []any          `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
