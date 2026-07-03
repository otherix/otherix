// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
)

// plDeclaredBackend mirrors one resolved backend in the heartbeat response's
// declared_load_balancers down-channel.
type plDeclaredBackend struct {
	VMID      string `json:"vm_id"`
	OverlayIP string `json:"overlay_ip"`
	MAC       string `json:"mac"`
	Healthy   bool   `json:"healthy"`
}

// plDeclaredLB mirrors one published load balancer in the heartbeat response.
type plDeclaredLB struct {
	LBID          string              `json:"lb_id"`
	PublishedPort int32               `json:"published_port"`
	Protocol      string              `json:"protocol"`
	BackendPort   int32               `json:"backend_port"`
	SourceCIDRs   []string            `json:"source_cidrs,omitempty"`
	Backends      []plDeclaredBackend `json:"backends"`
}

// plHeartbeatResp decodes the declared_load_balancers field under test from the
// heartbeat response.
type plHeartbeatResp struct {
	DeclaredLoadBalancers []plDeclaredLB `json:"declared_load_balancers"`
}

// plListenerReport mirrors one published_listeners entry the gateway reports up
// the heartbeat channel: its observed bind verdict for a published listener.
type plListenerReport struct {
	LBID  string `json:"lb_id"`
	Port  int32  `json:"port"`
	Bound bool   `json:"bound"`
	Error string `json:"error,omitempty"`
}

// TestLoadBalancerPublishedListenersRoundTrip drives the whole published-listener
// stack over the real heartbeat path: the CP declares a published load balancer's
// resolved backend set down to a gateway-role node (declared_load_balancers), and
// ingests that gateway's observed listener bind verdict back up
// (published_listeners -> lb_published_listener_status). A non-gateway node's
// heartbeat carries an empty declared set, proving the gateway-role gate holds
// end to end.
func TestLoadBalancerPublishedListenersRoundTrip(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()

	// One TLS agent listener, two seeded agent identities: a gateway-role node
	// (gets the declared set) and a plain hypervisor node (must get an empty set).
	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	gw := wgSeedAgent(t, h, caCert, caKey, "node-gw")
	hv := wgSeedAgent(t, h, caCert, caKey, "node-hv")
	if _, err := h.store.SetNodeGatewayRole(ctx, gw.nodeID, true); err != nil {
		t.Fatalf("SetNodeGatewayRole: %v", err)
	}

	// A running overlay VM owned by the admin, labelled to match the LB selector.
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	_, vmID, _, nodeID := seedIngressOverlayVM(t, h, admin, adminID)
	labelBackendVM(t, h, vmID, `{"app":"web"}`)
	markVMRunning(t, h, vmID, nodeID)

	// The addressable overlay NIC the CP resolves as the backend's dial target;
	// its IPv4 + MAC are what must arrive in declared_load_balancers.
	nics, err := h.store.ListVMNicsByVM(ctx, vmID)
	if err != nil || len(nics) == 0 {
		t.Fatalf("ListVMNicsByVM: nics=%d err=%v", len(nics), err)
	}
	nic := nics[0]
	if nic.Ipv4Address == nil {
		t.Fatalf("overlay NIC has no IPv4; CP-IPAM did not allocate")
	}
	wantIP := nic.Ipv4Address.String()
	wantMAC := nic.MacAddress.String()

	// A published load balancer selecting that VM (publish needs loadbalancer:publish;
	// admin holds it).
	const publishedPort = 8080
	create := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "web", "port": 22, "protocol": "tcp",
		"selector":       map[string]string{"app": "web"},
		"published_port": publishedPort,
		"source_cidrs":   []string{"203.0.113.0/24"},
	}, admin)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create published lb status = %d, want 201", create.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, create, &created)
	if created.ID == "" {
		t.Fatal("create response returned empty id")
	}

	// Down-channel: the gateway's heartbeat response declares the published LB
	// with the resolved backend's overlay_ip + mac and the published port.
	resp := plPostHeartbeat(t, agentSrv.URL, gw, nil)
	if len(resp.DeclaredLoadBalancers) != 1 {
		t.Fatalf("gateway declared_load_balancers = %d, want 1 (%+v)",
			len(resp.DeclaredLoadBalancers), resp.DeclaredLoadBalancers)
	}
	lb := resp.DeclaredLoadBalancers[0]
	if lb.LBID != created.ID {
		t.Errorf("declared lb_id = %q, want %q", lb.LBID, created.ID)
	}
	if lb.PublishedPort != publishedPort {
		t.Errorf("declared published_port = %d, want %d", lb.PublishedPort, publishedPort)
	}
	if len(lb.Backends) != 1 {
		t.Fatalf("declared backends = %d, want 1 (%+v)", len(lb.Backends), lb.Backends)
	}
	b := lb.Backends[0]
	if b.VMID != vmID.String() {
		t.Errorf("backend vm_id = %q, want %q", b.VMID, vmID)
	}
	if b.OverlayIP != wantIP {
		t.Errorf("backend overlay_ip = %q, want %q", b.OverlayIP, wantIP)
	}
	if b.MAC != wantMAC {
		t.Errorf("backend mac = %q, want %q", b.MAC, wantMAC)
	}

	// Up-channel: the gateway reports a bound listener; the CP stores it keyed to
	// the reporting node.
	plPostHeartbeat(t, agentSrv.URL, gw, []plListenerReport{
		{LBID: created.ID, Port: publishedPort, Bound: true},
	})

	lbID, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatalf("parse lb id %q: %v", created.ID, err)
	}
	statuses, err := h.store.ListLBPublishedListenerStatus(ctx, lbID)
	if err != nil {
		t.Fatalf("ListLBPublishedListenerStatus: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("ListLBPublishedListenerStatus = %d rows, want 1 (%+v)", len(statuses), statuses)
	}
	st := statuses[0]
	if st.NodeID != gw.nodeID {
		t.Errorf("status node_id = %v, want %v (reporting gateway)", st.NodeID, gw.nodeID)
	}
	if !st.Bound {
		t.Errorf("status bound = false, want true")
	}
	if st.Port != publishedPort {
		t.Errorf("status port = %d, want %d", st.Port, publishedPort)
	}

	// A non-gateway node's heartbeat response must carry an EMPTY declared set:
	// the gateway-role gate holds over the real heartbeat handler.
	hvResp := plPostHeartbeat(t, agentSrv.URL, hv, nil)
	if len(hvResp.DeclaredLoadBalancers) != 0 {
		t.Errorf("non-gateway declared_load_balancers = %d, want 0 (%+v)",
			len(hvResp.DeclaredLoadBalancers), hvResp.DeclaredLoadBalancers)
	}
}

// plPostHeartbeat posts a heartbeat over mTLS for ag, optionally carrying the
// observed published_listeners (nil omits the field), asserts 200, and returns
// the parsed response so the caller can read declared_load_balancers.
func plPostHeartbeat(t *testing.T, baseURL string, ag wgAgent, listeners []plListenerReport) plHeartbeatResp {
	t.Helper()
	body := map[string]any{
		"agent_version": "test-0.1.0",
		"architecture":  "amd64",
		"capabilities": map[string]any{
			"cpu_model":        "test-cpu",
			"cpu_flags":        []string{},
			"cpu_cores_total":  4,
			"memory_total_mib": 8192,
			"kernel_version":   "test",
			"qemu_version":     "test",
		},
		"resources": map[string]any{
			"cpu_cores_available":  4,
			"memory_available_mib": 8000,
		},
		"vms":      []any{},
		"networks": []any{},
	}
	if listeners != nil {
		body["published_listeners"] = listeners
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL+"/v1/nodes/"+ag.name+"/heartbeat", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new heartbeat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ag.client.Do(req)
	if err != nil {
		t.Fatalf("heartbeat Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("heartbeat status = %d, want 200; body=%s", resp.StatusCode, string(b))
	}
	var out plHeartbeatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode heartbeat response: %v", err)
	}
	return out
}
