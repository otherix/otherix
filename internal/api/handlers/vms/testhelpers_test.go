// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
)

// captureWriter is the minimal http.ResponseWriter the validation
// helpers need. The validation tests do not assert on body shape —
// only on the boolean return — so the recorder's default behaviour
// is sufficient.
type captureWriter = httptest.ResponseRecorder

// fakeRequest returns a minimal *http.Request the validation helpers
// can pass to response.WriteError without panicking.
func fakeRequest() *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "/v1/vms", nil)
	return req
}

// identityDialTransport is the shared streaming-proxy test transport. The CP
// handlers now dial each agent at the node's cluster-CA identity SAN
// (node-<name>.agents.otherix.local) so the geo route resolver can splice it;
// the httptest agents listen on a loopback host:port. DialContext maps a
// known identity SAN to its real httptest addr; an unmapped host dials
// unchanged, so a node whose identity SAN is absent still fails the dial (the
// negative tests rely on that). TLS verification is skipped because the
// handler pins ServerName to the identity SAN, which the self-signed httptest
// cert never carries.
func identityDialTransport(dialMap map[string]string) *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test dials httptest TLS servers
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			if mapped, ok := dialMap[host]; ok {
				addr = mapped
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}
