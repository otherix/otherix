// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package agentclient is the CP→agent HTTP client. It dials each
// agent over mutual TLS (CP presents a leaf signed by the cluster CA;
// agent's server cert is verified against the same CA bundle), and
// exposes the narrow surface a river worker needs to drive an async
// operation: submit a request that returns 202 + AsyncTaskAccepted,
// then poll the agent's GET /v1/tasks/{id} until the task reaches a
// terminal status.
//
// The storage_pool.scan vertical slice ships PostScan + PollTask.
// Future task types (template.import, vm.*, snapshot operations)
// extend this package with their own initiator methods, sharing the
// same Client / mTLS material / poll loop.
//
// The package is stdlib-only: net/http + crypto/tls. No third-party
// HTTP wrapper is in play, since the surface here is small enough
// that direct stdlib usage is cleaner than depending on retryable-
// http / resty / etc.
package agentclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// Client is the per-CP-process agent HTTP client. Safe for concurrent
// use; the underlying *http.Transport pools connections per host so
// repeated calls to the same agent reuse TLS sessions.
type Client struct {
	httpClient *http.Client
	cfg        config.AgentClientConfig
}

// New constructs a Client from cfg + a pre-built TLS material bundle.
// The CP-side mTLS material (replica's own client cert + cluster CA
// trust anchor) is produced upstream — either by the API binary's
// LoadOrGenerateCPCert hook, or by a CLI
// tool's filesystem loader (LoadMaterialFromFiles). Either way, this
// constructor takes primitives and does no I/O.
//
// cfg.Enabled must be true; the caller decides whether to construct
// the client at all (HTTP-only smoke loops can skip with workers.enabled
// = false).
func New(cfg config.AgentClientConfig, cert tls.Certificate, clusterCA *x509.Certificate) (*Client, error) {
	if !cfg.Enabled {
		return nil, errors.New("agentclient: cfg.Enabled is false")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("agentclient: %v", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("agentclient: cert is empty (no leaf bytes)")
	}
	if clusterCA == nil {
		return nil, errors.New("agentclient: clusterCA is nil")
	}

	pool := x509.NewCertPool()
	pool.AddCert(clusterCA)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}

	// Per-request safety net: each underlying HTTP call is bounded by
	// PollMaxInterval so a single hung connection cannot stall the
	// poll loop for the full Timeout budget. The overall poll budget
	// is enforced separately in PollTask via context.WithTimeout.
	httpClient := &http.Client{
		Timeout: cfg.PollMaxInterval,
		Transport: &http.Transport{
			TLSClientConfig:       tlsCfg,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: cfg.PollMaxInterval,
		},
	}

	return &Client{httpClient: httpClient, cfg: cfg}, nil
}

// Config returns the live config. Tests use it to assert defaults
// roundtripped correctly; production callers need it only for log
// fields.
func (c *Client) Config() config.AgentClientConfig { return c.cfg }

// HTTPClient returns the underlying *http.Client. Exposed so the
// CP-side WebSocket proxy can hand the same mTLS-configured client
// to coder/websocket.Dial — reusing the same TLS material the agent
// HTTP client already trusts. Treat the returned client as read-only;
// mutating its Transport breaks every concurrent call on this Client.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// LoadMaterialFromFiles is a filesystem-based loader for callers that
// don't have DB access to the cluster CA — primarily the ping-agent
// CLI and agentclient tests. Returns the parsed cert pair + the first
// CA cert from the caFile PEM bundle.
//
// The api binary boot path does NOT use this helper — it threads
// in-memory material from LoadOrGenerateCPCert through agentclient.New
// directly.
func LoadMaterialFromFiles(certFile, keyFile, caFile string) (tls.Certificate, *x509.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("agentclient: load client cert: %v", err)
	}
	caBytes, err := os.ReadFile(caFile) //nolint:gosec // operator-supplied CA path for CLI tools that don't have DB access; the function exists exactly to open this file
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("agentclient: read ca: %v", err)
	}
	block, _ := pem.Decode(caBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, nil, fmt.Errorf("agentclient: ca file %s contains no CERTIFICATE PEM block", caFile)
	}
	clusterCA, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("agentclient: parse ca %s: %v", caFile, err)
	}
	return cert, clusterCA, nil
}

// do sends req and returns the *http.Response. Non-2xx responses are
// returned as *AgentError so callers can branch on Status / Code
// without re-reading the body. The body is fully consumed and closed
// before return; resp.Body on the returned response is io.NopCloser
// over the buffered bytes, safe to re-read.
func (c *Client) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, nil, fmt.Errorf("agentclient: read response body: %v", readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, body, parseAgentError(resp.StatusCode, body)
	}
	return resp, body, nil
}

// parseAgentError decodes the OpenAPI Error envelope from body. A
// missing / malformed body still yields a non-nil *AgentError so the
// caller has Status to branch on.
func parseAgentError(status int, body []byte) *AgentError {
	envelope := &AgentError{Status: status}
	if len(body) == 0 {
		return envelope
	}

	var parsed struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return envelope
	}
	envelope.Code = parsed.Error.Code
	envelope.Message = parsed.Error.Message
	envelope.Details = parsed.Error.Details
	return envelope
}

// newRequest is the thin wrapper around http.NewRequestWithContext
// every method uses. Centralised so common headers (Accept,
// Content-Type when applicable) land consistently and so future
// instrumentation has a single seam.
func newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("agentclient: build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}
