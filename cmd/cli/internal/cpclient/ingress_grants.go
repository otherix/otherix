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

// IngressGrantVM is one VM entry in a grant's scope: the VM name, the set of
// guest TCP ports the grant authorizes, and the guest login the grant
// authorizes on it.
type IngressGrantVM struct {
	VMName string `json:"vm_name"`
	Ports  []int  `json:"ports"`
	Login  string `json:"login"`
}

// IngressGrant mirrors the grantView projection internal/api/handlers/ingressgrants
// produces. Token is the one-time plaintext grant token, present only in the
// POST /v1/ingress-grants create response; it is omitempty so list / get output
// (which never carries a plaintext) does not emit an empty "token" field.
type IngressGrant struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	RecipientLabel string           `json:"recipient_label"`
	CreatedBy      string           `json:"created_by"`
	VMs            []IngressGrantVM `json:"vms"`
	ExpiresAt      *string          `json:"expires_at"`
	Revoked        bool             `json:"revoked"`
	CreatedAt      string           `json:"created_at"`
	UpdatedAt      string           `json:"updated_at"`
	Token          string           `json:"token,omitempty"`
}

// IngressGrantList is the cursor-paginated payload of GET /v1/ingress-grants.
type IngressGrantList struct {
	Data []IngressGrant `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateIngressGrantRequest is the body of POST /v1/ingress-grants. TTL is an
// optional Go duration string (e.g. "168h"); empty means the grant never
// expires. RecipientLabel is an optional human label for the external
// person the grant is minted for.
type CreateIngressGrantRequest struct {
	Name           string           `json:"name"`
	RecipientLabel string           `json:"recipient_label,omitempty"`
	VMs            []IngressGrantVM `json:"vms"`
	TTL            string           `json:"ttl,omitempty"`
	SourceIP       string           `json:"source_ip,omitempty"`
}

// ListIngressGrantsParams collects the query knobs GET /v1/ingress-grants accepts.
type ListIngressGrantsParams struct {
	Limit  int
	Cursor string
}

// ErrIngressGrantExists is the sentinel CreateIngressGrant returns when the server
// responds 409 conflict on the grant-name uniqueness violation. The CP emits
// the generic `conflict` code, so detection is status-based.
var ErrIngressGrantExists = errors.New("ingress grant name already in use")

// ErrIngressGrantNotFound is returned by the name-resolution path when no grant
// carries the requested name. Callers surface it as a clean "no such grant"
// rather than leaking the list-then-match mechanics.
var ErrIngressGrantNotFound = errors.New("ingress grant not found")

// CreateIngressGrant submits POST /v1/ingress-grants. 201 returns the parsed grant
// including the one-time plaintext Token; 409 collapses to ErrIngressGrantExists;
// any other non-2xx surfaces as *APIError.
func (c *Client) CreateIngressGrant(ctx context.Context, in CreateIngressGrantRequest) (IngressGrant, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ingress-grants", in)
	if err != nil {
		return IngressGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return IngressGrant{}, ErrIngressGrantExists
		}
		return IngressGrant{}, err
	}
	var out IngressGrant
	if err := decodeJSON(body, &out); err != nil {
		return IngressGrant{}, err
	}
	return out, nil
}

// ListIngressGrants fetches one page of GET /v1/ingress-grants. Pagination is the
// caller's responsibility (re-issue with IngressGrantList.Meta.NextCursor).
func (c *Client) ListIngressGrants(ctx context.Context, params ListIngressGrantsParams) (IngressGrantList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	path := "/v1/ingress-grants"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return IngressGrantList{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return IngressGrantList{}, err
	}
	var out IngressGrantList
	if err := decodeJSON(body, &out); err != nil {
		return IngressGrantList{}, err
	}
	return out, nil
}

// GetIngressGrant fetches a single grant. The CP GET-by-id route accepts only a
// UUID literal, so a non-UUID identifier is resolved to a UUID client-side via
// resolveGrantName (list-then-match on name). The raw response body returned
// alongside the decoded value is the by-UUID GET's body, so `ingress-grant get
// --output json` echoes the server's projection verbatim; decode-only callers
// pass `_`.
func (c *Client) GetIngressGrant(ctx context.Context, identifier string) (IngressGrant, json.RawMessage, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return IngressGrant{}, nil, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/ingress-grants/"+id, nil)
	if err != nil {
		return IngressGrant{}, nil, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return IngressGrant{}, nil, err
	}
	var out IngressGrant
	if err := decodeJSON(body, &out); err != nil {
		return IngressGrant{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// AddIngressGrantVM submits POST /v1/ingress-grants/{id}/vms, adding (or replacing the
// login of) one VM in the grant's scope. The identifier is a UUID or a grant
// name resolved client-side. Returns the updated grant.
func (c *Client) AddIngressGrantVM(ctx context.Context, identifier string, vm IngressGrantVM) (IngressGrant, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return IngressGrant{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ingress-grants/"+id+"/vms", vm)
	if err != nil {
		return IngressGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return IngressGrant{}, err
	}
	var out IngressGrant
	if err := decodeJSON(body, &out); err != nil {
		return IngressGrant{}, err
	}
	return out, nil
}

// RemoveIngressGrantVM submits DELETE /v1/ingress-grants/{id}/vms/{vm_name}, shrinking
// the grant's scope. Removing a vm_name not in the grant is a server-side
// no-op. The identifier is a UUID or a grant name resolved client-side.
// Returns the updated grant.
func (c *Client) RemoveIngressGrantVM(ctx context.Context, identifier, vmName string) (IngressGrant, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return IngressGrant{}, err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/ingress-grants/"+id+"/vms/"+url.PathEscape(vmName), nil)
	if err != nil {
		return IngressGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return IngressGrant{}, err
	}
	var out IngressGrant
	if err := decodeJSON(body, &out); err != nil {
		return IngressGrant{}, err
	}
	return out, nil
}

// RevokeIngressGrant submits POST /v1/ingress-grants/{id}/revoke. The identifier is a
// UUID or a grant name resolved client-side. The server-side revoke is
// idempotent. Returns the updated (revoked) grant.
func (c *Client) RevokeIngressGrant(ctx context.Context, identifier string) (IngressGrant, error) {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return IngressGrant{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ingress-grants/"+id+"/revoke", nil)
	if err != nil {
		return IngressGrant{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return IngressGrant{}, err
	}
	var out IngressGrant
	if err := decodeJSON(body, &out); err != nil {
		return IngressGrant{}, err
	}
	return out, nil
}

// DeleteIngressGrant submits DELETE /v1/ingress-grants/{id}, removing the grant and
// freeing its name for reuse (unlike RevokeIngressGrant, which keeps the row). The
// identifier is a UUID or a grant name resolved client-side. A 204 returns nil;
// any non-2xx surfaces as *APIError (e.g. 404 when the grant is already gone).
func (c *Client) DeleteIngressGrant(ctx context.Context, identifier string) error {
	id, err := c.resolveGrantIdentifier(ctx, identifier)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/ingress-grants/"+id, nil)
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

// resolveGrantName pages GET /v1/ingress-grants looking for a name match and
// returns its UUID. Matching is case-insensitive to mirror the store's
// lowercased name guard. Grant names are globally unique, so at most one row
// matches. The loop also stops on a non-advancing cursor so a misbehaving
// server cannot spin it forever. Returns ErrIngressGrantNotFound when no row
// matches across all pages.
func (c *Client) resolveGrantName(ctx context.Context, name string) (string, error) {
	cursor := ""
	for {
		page, err := c.ListIngressGrants(ctx, ListIngressGrantsParams{Limit: 200, Cursor: cursor})
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
			return "", fmt.Errorf("%w: %q", ErrIngressGrantNotFound, name)
		}
		cursor = *next
	}
}
