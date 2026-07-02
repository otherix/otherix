// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// joinRequest mirrors components/schemas/NodeJoinRequest in
// control-plane.yaml. Hand-written because agent-side oapi-codegen
// targets agent.yaml only.
type joinRequest struct {
	Token                     string `json:"token"`
	CSRPEM                    string `json:"csr_pem"`
	NodeName                  string `json:"node_name"`
	Architecture              string `json:"architecture"`
	AdvertisedEndpoint        string `json:"advertised_endpoint"`
	IngressAdvertisedEndpoint string `json:"ingress_advertised_endpoint"`
	MigrationHost             string `json:"migration_host"`
	MigrationPortRangeStart   int    `json:"migration_port_range_start"`
	MigrationPortRangeEnd     int    `json:"migration_port_range_end"`
}

// joinResponse mirrors components/schemas/NodeJoinResponse.
type joinResponse struct {
	NodeID    string `json:"node_id"`
	CertPEM   string `json:"cert_pem"`
	CACertPEM string `json:"ca_cert_pem"`
}

// errorEnvelope mirrors the standard error response shape emitted
// by internal/api/response.WriteError. Used to extract a structured
// code/message from non-2xx responses so the bootstrap log records
// the CP's rejection reason without leaking raw bytes.
type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

// submitCSR posts the CSR + token to /v1/nodes/join over a transport
// that VERIFIES the CP serving cert against the operator-pinned cluster
// CA specifically (pinnedCA, the single /v1/ca bundle entry matching the
// operator fingerprint) - NOT the whole TOFU-served bundle, which a MITM
// could have padded with its own CA. The secret token is never
// transmitted to a server not authenticated by the pinned CA.
// verifyResponseChain downstream of the response decode stays as
// defense-in-depth on the returned cert material.
//
// Non-2xx response → wrapped ErrCSRRejected with the parsed envelope.
// Network / decode failure → plain fmt.Errorf chain. Token consumption
// happens at the CP side when this function returns nil; partial
// success (response observed by CP but lost in transit) leaves the
// token consumed without a usable cert and forces operator intervention.
func submitCSR(ctx context.Context, cfg *config.BootstrapConfig, token, csrPEM string, pinnedCA *x509.Certificate, timeout time.Duration) (*joinResponse, error) {
	transport, err := verifyingTransport(pinnedCA)
	if err != nil {
		return nil, err
	}

	body := joinRequest{
		Token:                     token,
		CSRPEM:                    csrPEM,
		NodeName:                  cfg.NodeName,
		Architecture:              cfg.Architecture,
		AdvertisedEndpoint:        cfg.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: cfg.IngressAdvertisedEndpoint,
		MigrationHost:             cfg.MigrationHost,
		MigrationPortRangeStart:   cfg.MigrationPortRangeStart,
		MigrationPortRangeEnd:     cfg.MigrationPortRangeEnd,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal /v1/nodes/join request: %v", err)
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// The CP never redirects /v1/nodes/join. Following a 307/308 would
		// re-send the token body (possibly cross-scheme to plain http), so
		// return any redirect as-is instead of following it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.CPURL+"/v1/nodes/join", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build /v1/nodes/join request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post /v1/nodes/join: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var env errorEnvelope
		_ = json.Unmarshal(preview, &env) // best-effort
		code := env.Error.Code
		msg := env.Error.Message
		if code == "" && msg == "" {
			msg = strings.TrimSpace(string(preview))
		}
		return nil, fmt.Errorf("%w: HTTP %d code=%q message=%q", ErrCSRRejected, resp.StatusCode, code, msg)
	}

	var out joinResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /v1/nodes/join response: %v", err)
	}
	if out.NodeID == "" || out.CertPEM == "" || out.CACertPEM == "" {
		return nil, fmt.Errorf("/v1/nodes/join response missing required fields: node_id=%q cert_pem=%v ca_cert_pem=%v",
			out.NodeID, out.CertPEM != "", out.CACertPEM != "")
	}
	return &out, nil
}
