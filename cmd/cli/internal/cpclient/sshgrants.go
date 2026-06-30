// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// SSHGrantVM is one VM entry in a grant's scope: the VM name plus the
// guest login the grant authorizes on it.
type SSHGrantVM struct {
	VMName string `json:"vm_name"`
	Login  string `json:"login"`
}

// SSHGrant mirrors the grantView projection internal/api/handlers/sshgrants
// produces. Token is the one-time plaintext grant token, present only in the
// POST /v1/ssh-grants create response; it is omitempty so list / get output
// (which never carries a plaintext) does not emit an empty "token" field.
type SSHGrant struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	RecipientLabel string       `json:"recipient_label"`
	CreatedBy      string       `json:"created_by"`
	VMs            []SSHGrantVM `json:"vms"`
	ExpiresAt      *string      `json:"expires_at"`
	Revoked        bool         `json:"revoked"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	Token          string       `json:"token,omitempty"`
}

// SSHGrantList is the cursor-paginated payload of GET /v1/ssh-grants.
type SSHGrantList struct {
	Data []SSHGrant `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateSSHGrantRequest is the body of POST /v1/ssh-grants. TTL is an
// optional Go duration string (e.g. "168h"); empty means the grant never
// expires. RecipientLabel is an optional human label for the external
// person the grant is minted for.
type CreateSSHGrantRequest struct {
	Name           string       `json:"name"`
	RecipientLabel string       `json:"recipient_label,omitempty"`
	VMs            []SSHGrantVM `json:"vms"`
	TTL            string       `json:"ttl,omitempty"`
}

// ListSSHGrantsParams collects the query knobs GET /v1/ssh-grants accepts.
type ListSSHGrantsParams struct {
	Limit  int
	Cursor string
}

// ErrSSHGrantExists is the sentinel CreateSSHGrant returns when the server
// responds 409 conflict on the grant-name uniqueness violation. The CP emits
// the generic `conflict` code, so detection is status-based.
var ErrSSHGrantExists = errors.New("ssh grant name already in use")

// ErrSSHGrantNotFound is returned by the name-resolution path when no grant
// carries the requested name. Callers surface it as a clean "no such grant"
// rather than leaking the list-then-match mechanics.
var ErrSSHGrantNotFound = errors.New("ssh grant not found")

// CreateSSHGrant submits POST /v1/ssh-grants. 201 returns the parsed grant
// including the one-time plaintext Token; 409 collapses to ErrSSHGrantExists;
// any other non-2xx surfaces as *APIError.
func (c *Client) CreateSSHGrant(ctx context.Context, in CreateSSHGrantRequest) (SSHGrant, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ssh-grants", in)
	if err != nil {
		return SSHGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return SSHGrant{}, ErrSSHGrantExists
		}
		return SSHGrant{}, err
	}
	var out SSHGrant
	if err := decodeJSON(body, &out); err != nil {
		return SSHGrant{}, err
	}
	return out, nil
}

// ListSSHGrants fetches one page of GET /v1/ssh-grants. Pagination is the
// caller's responsibility (re-issue with SSHGrantList.Meta.NextCursor).
func (c *Client) ListSSHGrants(ctx context.Context, params ListSSHGrantsParams) (SSHGrantList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	path := "/v1/ssh-grants"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return SSHGrantList{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return SSHGrantList{}, err
	}
	var out SSHGrantList
	if err := decodeJSON(body, &out); err != nil {
		return SSHGrantList{}, err
	}
	return out, nil
}

// GetSSHGrant fetches a single grant. The CP GET-by-id route accepts only a
// UUID literal, so a non-UUID identifier is resolved to a UUID client-side via
// resolveGrantName (list-then-match on name). The raw response body returned
// alongside the decoded value is the by-UUID GET's body, so `ssh-grant get
// --output json` echoes the server's projection verbatim; decode-only callers
// pass `_`.
func (c *Client) GetSSHGrant(ctx context.Context, identifier string) (SSHGrant, json.RawMessage, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return SSHGrant{}, nil, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/ssh-grants/"+id, nil)
	if err != nil {
		return SSHGrant{}, nil, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return SSHGrant{}, nil, err
	}
	var out SSHGrant
	if err := decodeJSON(body, &out); err != nil {
		return SSHGrant{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// AddSSHGrantVM submits POST /v1/ssh-grants/{id}/vms, adding (or replacing the
// login of) one VM in the grant's scope. The identifier is a UUID or a grant
// name resolved client-side. Returns the updated grant.
func (c *Client) AddSSHGrantVM(ctx context.Context, identifier string, vm SSHGrantVM) (SSHGrant, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return SSHGrant{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ssh-grants/"+id+"/vms", vm)
	if err != nil {
		return SSHGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return SSHGrant{}, err
	}
	var out SSHGrant
	if err := decodeJSON(body, &out); err != nil {
		return SSHGrant{}, err
	}
	return out, nil
}

// RemoveSSHGrantVM submits DELETE /v1/ssh-grants/{id}/vms/{vm_name}, shrinking
// the grant's scope. Removing a vm_name not in the grant is a server-side
// no-op. The identifier is a UUID or a grant name resolved client-side.
// Returns the updated grant.
func (c *Client) RemoveSSHGrantVM(ctx context.Context, identifier, vmName string) (SSHGrant, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return SSHGrant{}, err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/ssh-grants/"+id+"/vms/"+url.PathEscape(vmName), nil)
	if err != nil {
		return SSHGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return SSHGrant{}, err
	}
	var out SSHGrant
	if err := decodeJSON(body, &out); err != nil {
		return SSHGrant{}, err
	}
	return out, nil
}

// RevokeSSHGrant submits POST /v1/ssh-grants/{id}/revoke. The identifier is a
// UUID or a grant name resolved client-side. The server-side revoke is
// idempotent. Returns the updated (revoked) grant.
func (c *Client) RevokeSSHGrant(ctx context.Context, identifier string) (SSHGrant, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return SSHGrant{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ssh-grants/"+id+"/revoke", nil)
	if err != nil {
		return SSHGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return SSHGrant{}, err
	}
	var out SSHGrant
	if err := decodeJSON(body, &out); err != nil {
		return SSHGrant{}, err
	}
	return out, nil
}

// DeleteSSHGrant submits DELETE /v1/ssh-grants/{id}, removing the grant and
// freeing its name for reuse (unlike RevokeSSHGrant, which keeps the row). The
// identifier is a UUID or a grant name resolved client-side. A 204 returns nil;
// any non-2xx surfaces as *APIError (e.g. 404 when the grant is already gone).
func (c *Client) DeleteSSHGrant(ctx context.Context, identifier string) error {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/ssh-grants/"+id, nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(req)
	return err
}

// resolveGrantIdentifier returns identifier unchanged when it is already a
// UUID literal, otherwise treats it as a grant name and resolves it to a UUID
// via resolveGrantName. The CP per-id routes accept only UUIDs, so this is the
// single name-resolution seam shared by Get / AddVM / RemoveVM / Revoke.
func (c *Client) resolveGrantIdentifier(ctx context.Context, identifier string) (string, error) {
	if uuid.Validate(identifier) == nil {
		return identifier, nil
	}
	return c.resolveGrantName(ctx, identifier)
}

// resolveGrantName pages GET /v1/ssh-grants looking for a name match and
// returns its UUID. Matching is case-insensitive to mirror the store's
// lowercased name guard. Grant names are globally unique, so at most one row
// matches. The loop also stops on a non-advancing cursor so a misbehaving
// server cannot spin it forever. Returns ErrSSHGrantNotFound when no row
// matches across all pages.
func (c *Client) resolveGrantName(ctx context.Context, name string) (string, error) {
	cursor := ""
	for {
		page, err := c.ListSSHGrants(ctx, ListSSHGrantsParams{Limit: 200, Cursor: cursor})
		if err != nil {
			return "", err
		}
		for _, g := range page.Data {
			if strings.EqualFold(g.Name, name) {
				return g.ID, nil
			}
		}
		next := page.Meta.NextCursor
		if next == nil || *next == "" || *next == cursor {
			return "", fmt.Errorf("%w: %q", ErrSSHGrantNotFound, name)
		}
		cursor = *next
	}
}
