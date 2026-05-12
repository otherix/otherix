// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

// listResponse is the payload for GET /v1/tasks. Mirrors
// components/schemas/TaskList in api/openapi/control-plane.yaml.
type listResponse struct {
	Data []taskView     `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
