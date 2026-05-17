// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cpclient is the operator-CLI's client for the Otherix
// Control Plane HTTP API. It wraps the shared error envelope, bearer
// auth, and JSON marshalling boilerplate so the cobra subcommands
// stay thin.
//
// **Wire-shape note:** the wire types here mirror the actual
// internal/api/handlers/vms shape (MVP simplified), NOT the richer
// design declared в api/openapi/control-plane.yaml. Reconciliation
// deferred к a future iteration; the package doc on
// internal/api/handlers/vms documents the truth on the server side.
package cpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultTimeout is the per-request deadline applied when Options.Timeout
// is zero. Generous enough к accommodate the create handler's atomic
// enqueue (one round-trip к Postgres + river insert) without masking a
// genuinely-stuck server.
const defaultTimeout = 30 * time.Second

// Client talks к the CP's /v1/* surface over HTTP с a bearer token.
// All methods are safe for concurrent use; the underlying http.Client
// is shared.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// Options configures a Client. BaseURL and Token are required; the
// rest are tuned by the callsite when needed.
type Options struct {
	// BaseURL is the CP's root URL ("http://localhost:8080" in the
	// dev workflow; an HTTPS URL behind a TLS terminator в production).
	// Trailing slashes are trimmed before use.
	BaseURL string

	// Token is the bearer credential. Either a JWT (no prefix) or an
	// API token (`otx_*`) — the CP's Authn middleware accepts both.
	Token string

	// Timeout is the per-request deadline. Zero defaults к
	// defaultTimeout (30s).
	Timeout time.Duration

	// HTTPClient is an optional override; tests pass a recorded /
	// faked client. Production code leaves this nil and uses the
	// stdlib default.
	HTTPClient *http.Client
}

// New constructs a Client. Returns an error when BaseURL or Token is
// empty so the caller's "credentials missing" branch fires at
// construction rather than on the first API call.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("cpclient: BaseURL is required")
	}
	if opts.Token == "" {
		return nil, errors.New("cpclient: Token is required (set OTHERIX_API_TOKEN or pass --token)")
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		token:      opts.Token,
		httpClient: httpClient,
	}, nil
}

// NewAnonymous constructs a Client without a bearer token. The only
// CP endpoints that accept anonymous calls are /v1/auth/login и
// /v1/auth/refresh — used by the `otherix config add cluster` flow
// to obtain а JWT before issuing an API token. Subsequent calls
// against authenticated endpoints will fail с 401.
//
// Callers that need to upgrade an anonymous client to authenticated
// (login → JWT → CreateAPIToken) can do so by constructing a fresh
// Client с New once the token is in hand.
func NewAnonymous(baseURL string, opts Options) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("cpclient: BaseURL is required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      "",
		httpClient: httpClient,
	}, nil
}

// WithToken returns a shallow copy of c authenticated с the
// supplied bearer. Used by `config add cluster` to upgrade an
// anonymous client после а successful Login → JWT. The receiver is
// not mutated, so this is safe против concurrent reads.
func (c *Client) WithToken(token string) *Client {
	clone := *c
	clone.token = token
	return &clone
}

// BaseURL returns the configured base URL. Useful for the CLI's
// "you are talking к https://..." preamble.
func (c *Client) BaseURL() string { return c.baseURL }

// HTTPClient returns the underlying *http.Client. Exposed so the
// CLI's WebSocket console subcommand can reuse the same TLS-trust /
// timeout policy when dialing wss://. Treat the returned value as
// read-only.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// do issues req, returning (status, body) on success and a typed
// *APIError on a non-2xx envelope. Network failures wrap the
// underlying error verbatim so callers can errors.Is them against
// context.DeadlineExceeded и friends.
func (c *Client) do(req *http.Request) (int, []byte, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if req.Body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cpclient: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("cpclient: read response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, body, nil
	}
	return resp.StatusCode, body, parseAPIError(resp.StatusCode, body)
}

// newRequest builds an *http.Request с the URL joined onto baseURL и
// the body marshalled as JSON. body=nil produces a body-less request
// (GET / DELETE). path may carry a `?key=value` suffix — the helper
// splits it out before url.JoinPath к keep the question mark unescaped.
func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	cleanPath, rawQuery := splitQuery(path)
	u, err := url.JoinPath(c.baseURL, cleanPath)
	if err != nil {
		return nil, fmt.Errorf("cpclient: join path %q: %v", path, err)
	}
	if rawQuery != "" {
		u += "?" + rawQuery
	}

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("cpclient: marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("cpclient: build request: %v", err)
	}
	return req, nil
}

// splitQuery separates path from query на the first `?`. Returns the
// path и the (already-encoded) query string. Callers concatenate
// rawQuery back onto the joined URL c a single `?` к keep the
// stdlib's parser happy при request construction.
func splitQuery(path string) (string, string) {
	for i := 0; i < len(path); i++ {
		if path[i] == '?' {
			return path[:i], path[i+1:]
		}
	}
	return path, ""
}

// decodeJSON unmarshals body into out. Returns a wrapped error so
// callers see the offending path / payload via errors.Is on the
// underlying json.SyntaxError.
func decodeJSON(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("cpclient: decode response: %v", err)
	}
	return nil
}
