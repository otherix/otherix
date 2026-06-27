// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users

// createRequest is the body of POST /v1/users. Mirrors UserCreate in
// api/openapi/control-plane.yaml.
type createRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// updateRequest covers both PATCH /v1/users/me and PATCH /v1/users/{id}.
// Pointers distinguish "field omitted" from "field set to zero value";
// only fields whose pointer is non-nil are merged into the existing
// row. Email is intentionally absent — it is immutable in the current
// API.
type updateRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Password    *string `json:"password,omitempty"`
	Role        *string `json:"role,omitempty"`
}

type listResponse struct {
	Data []userView     `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
