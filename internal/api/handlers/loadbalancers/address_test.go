// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import "testing"

// TestGatewayListenerAddress locks the connect-target construction: the host is
// taken from the node's advertised endpoint (scheme stripped) and joined with
// the published port, for URL, bare host:port, and IPv6 endpoint forms.
func TestGatewayListenerAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		endpoint      string
		publishedPort int32
		want          string
	}{
		{name: "https url with ip", endpoint: "https://10.77.0.1:9443", publishedPort: 30080, want: "10.77.0.1:30080"},
		{name: "https url with host", endpoint: "https://gw.test:9444", publishedPort: 30080, want: "gw.test:30080"},
		{name: "bare host port", endpoint: "10.0.0.5:9443", publishedPort: 30080, want: "10.0.0.5:30080"},
		{name: "ipv6 url", endpoint: "https://[fd00::1]:9443", publishedPort: 30080, want: "[fd00::1]:30080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gatewayListenerAddress(tt.endpoint, tt.publishedPort); got != tt.want {
				t.Errorf("gatewayListenerAddress(%q, %d) = %q, want %q", tt.endpoint, tt.publishedPort, got, tt.want)
			}
		})
	}
}
