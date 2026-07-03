// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	agentheartbeat "github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/store"
)

// publishedLBSpy stubs the two reads loadDeclaredLoadBalancers gates on: NodeByID
// (the gateway-role gate) and ListPublishedLoadBalancerBackends (the resolved
// published backend set). It embeds store.HeartbeatProjection (left nil) so it
// satisfies the interface while implementing only the methods the builder
// exercises; any other call would panic, the desired failure mode for an
// unexpected projection step.
type publishedLBSpy struct {
	store.HeartbeatProjection
	node      store.Node
	published []store.PublishedLoadBalancer
	// listCalls counts ListPublishedLoadBalancerBackends invocations so the
	// non-gateway test can assert the builder short-circuits before the scan.
	listCalls int
}

func (s *publishedLBSpy) NodeByID(_ context.Context, _ uuid.UUID) (store.Node, error) {
	return s.node, nil
}

func (s *publishedLBSpy) ListPublishedLoadBalancerBackends(_ context.Context) ([]store.PublishedLoadBalancer, error) {
	s.listCalls++
	return s.published, nil
}

// TestLoadDeclaredLoadBalancersGateway verifies the gateway recipient gets the
// published load balancer with its resolved backend set, the overlay address
// rendered to its canonical string form, and that the wire JSON round-trips into
// the agent-side Response field-for-field (the manual-sync contract between the
// CP marshaller and the agent unmarshaller).
func TestLoadDeclaredLoadBalancersGateway(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	lbID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	overlayIP := netip.MustParseAddr("10.244.0.5")
	mac, err := net.ParseMAC("52:54:00:12:34:56")
	if err != nil {
		t.Fatalf("ParseMAC(...) = %v, want nil", err)
	}

	spy := &publishedLBSpy{
		node: store.Node{GatewayRole: true},
		published: []store.PublishedLoadBalancer{{
			LBID:          lbID,
			PublishedPort: 8080,
			Protocol:      "tcp",
			BackendPort:   80,
			SourceCIDRs:   []string{"10.0.0.0/8"},
			Backends: []store.PublishedBackend{{
				VMID:      vmID,
				OverlayIP: overlayIP,
				MAC:       mac,
				Healthy:   true,
			}},
		}},
	}
	h := newQuietHandler()

	got, err := h.loadDeclaredLoadBalancers(context.Background(), spy, nodeID)
	if err != nil {
		t.Fatalf("loadDeclaredLoadBalancers(...) = %v, want nil", err)
	}

	want := []declaredLoadBalancer{{
		LBID:          lbID,
		PublishedPort: 8080,
		Protocol:      "tcp",
		BackendPort:   80,
		SourceCIDRs:   []string{"10.0.0.0/8"},
		Backends: []declaredBackend{{
			VMID:      vmID,
			OverlayIP: "10.244.0.5",
			MAC:       "52:54:00:12:34:56",
			Healthy:   true,
		}},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("loadDeclaredLoadBalancers(...) mismatch (-want +got):\n%s", diff)
	}

	// Wire round-trip: the CP marshals responseBody and the agent unmarshals the
	// same JSON into its symmetric Response type. Assert the published LB and its
	// backend's overlay_ip/mac strings survive the crossing byte-for-byte.
	blob, err := json.Marshal(responseBody{DeclaredLoadBalancers: got})
	if err != nil {
		t.Fatalf("json.Marshal(responseBody) = %v, want nil", err)
	}
	var resp agentheartbeat.Response
	if err := json.Unmarshal(blob, &resp); err != nil {
		t.Fatalf("json.Unmarshal(agent Response) = %v, want nil", err)
	}
	wantAgent := []agentheartbeat.DeclaredLoadBalancer{{
		LBID:          lbID,
		PublishedPort: 8080,
		Protocol:      "tcp",
		BackendPort:   80,
		SourceCIDRs:   []string{"10.0.0.0/8"},
		Backends: []agentheartbeat.DeclaredBackend{{
			VMID:      vmID,
			OverlayIP: "10.244.0.5",
			MAC:       "52:54:00:12:34:56",
			Healthy:   true,
		}},
	}}
	if diff := cmp.Diff(wantAgent, resp.DeclaredLoadBalancers); diff != "" {
		t.Errorf("agent Response.DeclaredLoadBalancers mismatch (-want +got):\n%s", diff)
	}
}

// TestLoadDeclaredLoadBalancersNonGateway verifies the role gate: a non-gateway
// recipient gets a nil declared set and the builder never runs the published
// backend scan (it short-circuits on the role check, mirroring
// gatewayAddrsForNode).
func TestLoadDeclaredLoadBalancersNonGateway(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	spy := &publishedLBSpy{
		node: store.Node{GatewayRole: false},
		published: []store.PublishedLoadBalancer{{
			LBID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		}},
	}
	h := newQuietHandler()

	got, err := h.loadDeclaredLoadBalancers(context.Background(), spy, nodeID)
	if err != nil {
		t.Fatalf("loadDeclaredLoadBalancers(...) = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("loadDeclaredLoadBalancers on non-gateway = %v, want nil", got)
	}
	if spy.listCalls != 0 {
		t.Errorf("ListPublishedLoadBalancerBackends called %d times on non-gateway, want 0", spy.listCalls)
	}
}
