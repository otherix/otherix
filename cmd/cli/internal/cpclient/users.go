// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// User mirrors the userView projection internal/api/handlers/users
// produces. Email / DisplayName are omitempty because they are
// optional columns; LastLoginAt is nullable. The CP never surfaces
// password_hash, so it has no field here.
type User struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	Email       string  `json:"email,omitempty"`
	DisplayName string  `json:"display_name,omitempty"`
	Role        string  `json:"role"`
	LastLoginAt *string `json:"last_login_at,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// UserList is the cursor-paginated payload of GET /v1/users.
type UserList struct {
	Data []User `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateUserRequest is the POST /v1/users body. Username, Role and
// Password are required by the server; Email and DisplayName are
// optional and emitted with omitempty so the server applies its
// "unset" semantics when the operator left them blank.
type CreateUserRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
	Password    string `json:"password"`
}

// UpdateUserRequest is the PATCH /v1/users/{id} body. Every field is a
// pointer so UpdateUser can send only the ones the operator set and
// leave the rest untouched server-side; a nil field is omitted from
// the JSON entirely.
type UpdateUserRequest struct {
	Role        *string `json:"role,omitempty"`
	Password    *string `json:"password,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
}

// ListUsersParams collects the query knobs GET /v1/users accepts.
// Username is an optional exact-match filter (the server runs a guard
// lookup keyed on it); Limit / Cursor drive cursor pagination.
type ListUsersParams struct {
	Username string
	Limit    int
	Cursor   string
}

// CreateUser submits POST /v1/users and returns the parsed User view.
// Non-2xx surfaces as *APIError (e.g. 409 conflict on a duplicate
// username, 400 validation_failed on a malformed username/password).
// Admin-only (`user:manage`).
func (c *Client) CreateUser(ctx context.Context, req CreateUserRequest) (User, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/users", req)
	if err != nil {
		return User{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return User{}, err
	}
	var out User
	if err := decodeJSON(body, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

// ListUsers fetches GET /v1/users and returns the parsed page. When
// Username is set it is sent as ?username= for the server-side exact
// match. Pagination is the caller's responsibility (re-issue with
// UserList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListUsers(ctx context.Context, params ListUsersParams) (UserList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Username != "" {
		q.Set("username", params.Username)
	}

	path := "/v1/users"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return UserList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return UserList{}, err
	}
	var out UserList
	if err := decodeJSON(body, &out); err != nil {
		return UserList{}, err
	}
	return out, nil
}

// GetUserByUsername resolves a username to its full User row via the
// ListUsers username filter. The CP returns 200 + an empty data array
// on a miss (mirroring the email-lookup guard, NOT a 404), so an empty
// page is translated explicitly to ErrNotFound rather than surfacing a
// phantom zero-value User. Usernames are unique, so a hit yields exactly
// one row.
func (c *Client) GetUserByUsername(ctx context.Context, username string) (User, error) {
	page, err := c.ListUsers(ctx, ListUsersParams{Username: username})
	if err != nil {
		return User{}, err
	}
	if len(page.Data) == 0 {
		return User{}, fmt.Errorf("%w: user %q", ErrNotFound, username)
	}
	return page.Data[0], nil
}

// UpdateUser submits PATCH /v1/users/{id} with only the non-nil fields
// of req (the pointer/omitempty shape on UpdateUserRequest takes care
// of the "send only what changed" contract). Returns the updated User
// view. Admin-only.
func (c *Client) UpdateUser(ctx context.Context, id string, req UpdateUserRequest) (User, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPatch, "/v1/users/"+id, req)
	if err != nil {
		return User{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return User{}, err
	}
	var out User
	if err := decodeJSON(body, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

// DeleteUser submits DELETE /v1/users/{id}. 204 returns nil; any
// non-2xx surfaces as *APIError (e.g. 409 conflict when the user still
// owns resources). Admin-only.
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/users/"+id, nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	return err
}

// GetMe fetches GET /v1/users/me and returns the authenticated
// caller's own User view. Available to every authenticated role.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/users/me", nil)
	if err != nil {
		return User{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return User{}, err
	}
	var out User
	if err := decodeJSON(body, &out); err != nil {
		return User{}, err
	}
	return out, nil
}
