// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAdvertisedEndpointURL(t *testing.T) {
	base := func(endpoint string) joinRequest {
		return joinRequest{
			Token: "otx_join_x", CSRPEM: "pem", NodeName: "node-1", Architecture: "amd64",
			AdvertisedEndpoint: endpoint, MigrationHost: "10.77.0.1",
			MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
		}
	}
	valid := []string{"https://10.77.0.1:9443", "https://node1.example.com:9443", "https://127.0.0.1:9444"}
	for _, e := range valid {
		req := base(e)
		if err := req.validate(); err != nil {
			t.Errorf("validate() advertised_endpoint=%q = %v, want nil", e, err)
		}
	}
	// "" is rejected by the existing errMissingAdvertisedEndpoint (presence), not the URL check.
	badURL := []string{"not-a-url", "http://10.77.0.1:9443", "https://", "://nohost"}
	for _, e := range badURL {
		req := base(e)
		if err := req.validate(); !errors.Is(err, errAdvertisedEndpointInvalidURL) {
			t.Errorf("validate() advertised_endpoint=%q = %v, want errAdvertisedEndpointInvalidURL", e, err)
		}
	}
}

func TestValidateIngressAdvertisedEndpoint(t *testing.T) {
	base := func(endpoint string) joinRequest {
		return joinRequest{
			Token: "otx_join_x", CSRPEM: "pem", NodeName: "node-1", Architecture: "amd64",
			AdvertisedEndpoint: "https://10.77.0.1:9443", IngressAdvertisedEndpoint: endpoint,
			MigrationHost:           "10.77.0.1",
			MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
		}
	}
	// Empty is valid: ingress_advertised_endpoint is optional (a non-gateway
	// node sends none), unlike the required advertised_endpoint.
	empty := base("")
	if err := empty.validate(); err != nil {
		t.Errorf("validate() ingress_advertised_endpoint=\"\" = %v, want nil", err)
	}
	valid := []string{"https://10.77.0.1:9443", "https://node1.example.com:9443", "https://127.0.0.1:9444"}
	for _, e := range valid {
		req := base(e)
		if err := req.validate(); err != nil {
			t.Errorf("validate() ingress_advertised_endpoint=%q = %v, want nil", e, err)
		}
	}
	tooLong := base("https://" + strings.Repeat("a", advertisedEndpointMaxLength) + ".example.com")
	if err := tooLong.validate(); !errors.Is(err, errIngressAdvertisedEndpointTooLong) {
		t.Errorf("validate() over-long ingress endpoint = %v, want errIngressAdvertisedEndpointTooLong", tooLong.validate())
	}
	badURL := []string{"not-a-url", "http://10.77.0.1:9443", "https://", "://nohost"}
	for _, e := range badURL {
		req := base(e)
		if err := req.validate(); !errors.Is(err, errIngressAdvertisedEndpointInvalidURL) {
			t.Errorf("validate() ingress_advertised_endpoint=%q = %v, want errIngressAdvertisedEndpointInvalidURL", e, err)
		}
	}
}

// base builds an otherwise-valid join request so each test can vary the
// node_name in isolation.
func baseJoinRequest(name string) joinRequest {
	return joinRequest{
		Token: "otx_join_x", CSRPEM: "pem", NodeName: name, Architecture: "amd64",
		AdvertisedEndpoint: "https://10.77.0.1:9443", MigrationHost: "10.77.0.1",
		MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
	}
}

func TestValidateNodeNameCharset(t *testing.T) {
	// The redeemed node_name becomes the server-authoritative cert CN
	// `node-<name>` + SAN, so it MUST be a lowercase RFC 1123 DNS label.
	invalid := []string{"Bad_Name", "a/b", "-x", "x-", "Node1", strings.Repeat("a", 64)}
	for _, name := range invalid {
		req := baseJoinRequest(name)
		if err := req.validate(); err == nil {
			t.Errorf("validate() node_name=%q = nil, want error", name)
		}
	}

	valid := []string{"node-1", "n", "abc123", "a-b-c"}
	for _, name := range valid {
		req := baseJoinRequest(name)
		if err := req.validate(); err != nil {
			t.Errorf("validate() node_name=%q = %v, want nil", name, err)
		}
	}

	// Empty stays the dedicated required-sentinel for handler mapping.
	empty := baseJoinRequest("")
	if err := empty.validate(); !errors.Is(err, errMissingNodeName) {
		t.Errorf("validate() node_name=\"\" = %v, want errMissingNodeName", err)
	}
}

// TestValidateNodeNameTrimsInPlace pins the trim-vs-cert invariant: the
// name validate() vetted (trimmed) must be the name that flows into the
// node row + SignCSR. validate() normalises req.NodeName in place so the
// downstream redeem() reads the trimmed value, not the raw padded one.
func TestValidateNodeNameTrimsInPlace(t *testing.T) {
	req := baseJoinRequest("  node-1  ")
	if err := req.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	if req.NodeName != "node-1" {
		t.Errorf("after validate() NodeName = %q, want %q (trimmed in place)", req.NodeName, "node-1")
	}
}
