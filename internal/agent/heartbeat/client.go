// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// defaultRequestTimeout is the per-request budget when the caller has
// not pinned one. Long enough to absorb a cold TLS handshake plus
// the receiver's transaction (typical: ~50 ms), short enough that a
// slow CP cannot stall the loop indefinitely.
const defaultRequestTimeout = 10 * time.Second

// Client posts heartbeats over mTLS. Safe for concurrent use; the
// underlying *http.Transport pools connections, so back-to-back
// heartbeats reuse the TLS session.
//
// The client identifies itself to the CP by node NAME (parsed from the
// cert CN at agent startup), not UUID. The heartbeat URL is
// `/v1/nodes/{name}/heartbeat`.
type Client struct {
	httpClient *http.Client
	endpoint   string
	nodeName   string
	timeout    time.Duration
}

// ClientConfig bundles the construction-time inputs. CPEndpoint must
// be the CP base URL (e.g. "https://cp.local:8443"); the per-node
// heartbeat path is appended internally so callers do not need to
// know the URL shape.
type ClientConfig struct {
	CPEndpoint     string
	NodeName       string
	TLS            config.TLSConfig
	RequestTimeout time.Duration
}

// NewClient constructs a Client. Cert/key/CA paths are read at
// construction time so misconfiguration surfaces at agent boot, not
// in the middle of the run loop.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.CPEndpoint == "" {
		return nil, fmt.Errorf("heartbeat client: cp_endpoint is required")
	}
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("heartbeat client: node_name is required")
	}
	tlsCfg, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:       tlsCfg,
				MaxIdleConns:          4,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
		},
		endpoint: cfg.CPEndpoint,
		nodeName: cfg.NodeName,
		timeout:  timeout,
	}, nil
}

// Send posts the report to the CP and returns the parsed response.
// Returns the HTTP status, the decoded response (including
// declared_pools), and a non-nil error if the request fails before
// getting a response or the response status is non-2xx (the body is
// read and embedded in the error for diagnostics).
func (c *Client) Send(ctx context.Context, report Report) (int, *Response, error) {
	body, err := json.Marshal(report)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal report: %v", err)
	}
	url := fmt.Sprintf("%s/v1/nodes/%s/heartbeat", c.endpoint, c.nodeName)

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post heartbeat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, nil, fmt.Errorf("non-2xx %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %v", err)
	}
	var decoded Response
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("decode response: %v", err)
		}
	}
	return resp.StatusCode, &decoded, nil
}

// buildTLSConfig wires the mTLS material on disk into a *tls.Config.
// Errors surface at construction time per the no-half-configured-agent
// rule.
func buildTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	if cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, fmt.Errorf("heartbeat client: cert_path and key_path are required")
	}
	if cfg.CACertPath == "" {
		return nil, fmt.Errorf("heartbeat client: ca_cert_path is required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %v", err)
	}
	caBytes, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("read ca cert: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("ca cert %s contains no PEM blocks", cfg.CACertPath)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
