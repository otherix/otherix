// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// wgHeartbeatReport is the test-side observed WG state posted up the heartbeat
// channel. Mirrors the agent-side WireGuardReport JSON contract.
type wgHeartbeatReport struct {
	PublicKey        string   `json:"public_key"`
	Endpoint         string   `json:"endpoint"`
	ListenPort       int32    `json:"listen_port"`
	EstablishedPeers []string `json:"established_peers,omitempty"`
}

// wgHeartbeatRequest is the minimal heartbeat body the test posts: just enough
// to pass validation plus the optional wireguard report.
type wgHeartbeatRequest struct {
	AgentVersion string             `json:"agent_version"`
	Architecture string             `json:"architecture"`
	Capabilities wgHeartbeatCaps    `json:"capabilities"`
	Resources    wgHeartbeatRes     `json:"resources"`
	VMs          []struct{}         `json:"vms"`
	Networks     []struct{}         `json:"networks"`
	Wireguard    *wgHeartbeatReport `json:"wireguard,omitempty"`
}

type wgHeartbeatCaps struct {
	CPUModel       string     `json:"cpu_model"`
	CPUFlags       []string   `json:"cpu_flags"`
	CPUCoresTotal  int32      `json:"cpu_cores_total"`
	MemoryTotalMib int64      `json:"memory_total_mib"`
	KernelVersion  string     `json:"kernel_version"`
	QEMUVersion    string     `json:"qemu_version"`
	Firmwares      []struct{} `json:"firmwares"`
}

type wgHeartbeatRes struct {
	CPUCoresAvailable  int32 `json:"cpu_cores_available"`
	MemoryAvailableMib int64 `json:"memory_available_mib"`
}

// wgDeclaredPeer mirrors the declared_wireguard_peers entry in the heartbeat
// response down-channel.
type wgDeclaredPeer struct {
	NodeID     string   `json:"node_id"`
	PublicKey  string   `json:"public_key"`
	Endpoint   string   `json:"endpoint"`
	OverlayIP  string   `json:"overlay_ip"`
	AllowedIPs []string `json:"allowed_ips"`
}

// wgHeartbeatResponse decodes the fields under test from the heartbeat response.
type wgHeartbeatResponse struct {
	DeclaredWireGuardPeers []wgDeclaredPeer `json:"declared_wireguard_peers"`
	SelfOverlayIP          *string          `json:"self_overlay_ip"`
}

// wgAgent bundles a seeded node identity plus the mTLS client that presents its
// leaf cert to the TLS agent listener.
type wgAgent struct {
	nodeID uuid.UUID
	name   string
	client *http.Client
}

// TestWireguardPeerDistribution drives the CP-side WireGuard peer-distribution
// contract end to end: two agents report observed WG state up the heartbeat
// channel, and the CP redistributes every OTHER agent's CP-assigned fabric
// identity (overlay_ip + allowed_ips /32) down the declared_wireguard_peers
// channel, excluding self.
func TestWireguardPeerDistribution(t *testing.T) {
	h := newE2E(t)

	// Ephemeral CA the TLS agent listener trusts; each agent presents a leaf
	// signed by it, and the seeded agent_certs row binds the leaf fingerprint
	// to its node id (the agentMTLS identity contract).
	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)

	a := wgSeedAgent(t, h, caCert, caKey, "node-a")
	b := wgSeedAgent(t, h, caCert, caKey, "node-b")

	// A reports first; it is the only agent so far, so it sees no peers.
	respA := wgSendHeartbeat(t, agentSrv.URL, a, &wgHeartbeatReport{
		PublicKey:  "pkA",
		Endpoint:   "a.example:51820",
		ListenPort: 51820,
	})
	if len(respA.DeclaredWireGuardPeers) != 0 {
		t.Fatalf("A first heartbeat declared_wireguard_peers = %d, want 0 (%+v)",
			len(respA.DeclaredWireGuardPeers), respA.DeclaredWireGuardPeers)
	}
	// A reports first, so it takes overlay index 0; the CP renders its own
	// address with the supernet prefix in self_overlay_ip.
	if respA.SelfOverlayIP == nil || *respA.SelfOverlayIP != "10.42.0.1/16" {
		t.Errorf("agent A self_overlay_ip = %v, want 10.42.0.1/16", respA.SelfOverlayIP)
	}

	// B reports second; it must see exactly A (overlay index 0 -> 10.42.0.1).
	respB := wgSendHeartbeat(t, agentSrv.URL, b, &wgHeartbeatReport{
		PublicKey:  "pkB",
		Endpoint:   "b.example:51820",
		ListenPort: 51820,
	})
	if len(respB.DeclaredWireGuardPeers) != 1 {
		t.Fatalf("B heartbeat declared_wireguard_peers = %d, want 1 (%+v)",
			len(respB.DeclaredWireGuardPeers), respB.DeclaredWireGuardPeers)
	}
	// B reports second, so it takes overlay index 1.
	if respB.SelfOverlayIP == nil || *respB.SelfOverlayIP != "10.42.0.2/16" {
		t.Errorf("agent B self_overlay_ip = %v, want 10.42.0.2/16", respB.SelfOverlayIP)
	}
	wgAssertPeer(t, "B sees A", respB.DeclaredWireGuardPeers[0], wgDeclaredPeer{
		NodeID:     a.nodeID.String(),
		PublicKey:  "pkA",
		Endpoint:   "a.example:51820",
		OverlayIP:  "10.42.0.1",
		AllowedIPs: []string{"10.42.0.1/32"},
	})

	// A reports again; now it must see exactly B (overlay index 1 -> 10.42.0.2).
	respA2 := wgSendHeartbeat(t, agentSrv.URL, a, &wgHeartbeatReport{
		PublicKey:  "pkA",
		Endpoint:   "a.example:51820",
		ListenPort: 51820,
	})
	if len(respA2.DeclaredWireGuardPeers) != 1 {
		t.Fatalf("A second heartbeat declared_wireguard_peers = %d, want 1 (%+v)",
			len(respA2.DeclaredWireGuardPeers), respA2.DeclaredWireGuardPeers)
	}
	wgAssertPeer(t, "A sees B", respA2.DeclaredWireGuardPeers[0], wgDeclaredPeer{
		NodeID:     b.nodeID.String(),
		PublicKey:  "pkB",
		Endpoint:   "b.example:51820",
		OverlayIP:  "10.42.0.2",
		AllowedIPs: []string{"10.42.0.2/32"},
	})
}

// TestNodeGetWireguardFabric drives the admin/operator-only WG fabric block on
// GET /v1/nodes/{name}: the node's own underlay identity plus every other agent
// as a mesh peer with an established flag sourced from this node's reported
// EstablishedPeers set.
func TestNodeGetWireguardFabric(t *testing.T) {
	h := newE2E(t)
	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)

	a := wgSeedAgent(t, h, caCert, caKey, "node-a")
	b := wgSeedAgent(t, h, caCert, caKey, "node-b")

	wgSendHeartbeat(t, agentSrv.URL, a, &wgHeartbeatReport{PublicKey: "pkA", Endpoint: "a.example:51820", ListenPort: 51820})
	wgSendHeartbeat(t, agentSrv.URL, b, &wgHeartbeatReport{PublicKey: "pkB", Endpoint: "b.example:51820", ListenPort: 51820})
	wgSendHeartbeat(t, agentSrv.URL, a, &wgHeartbeatReport{
		PublicKey: "pkA", Endpoint: "a.example:51820", ListenPort: 51820,
		EstablishedPeers: []string{b.nodeID.String()},
	})

	adminTok, _ := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/nodes/"+a.name, adminTok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET node-a status = %d, want 200", resp.StatusCode)
	}
	var node struct {
		WireGuard *struct {
			OverlayIP  string `json:"overlay_ip"`
			PublicKey  string `json:"public_key"`
			ListenPort int32  `json:"listen_port"`
			Endpoint   string `json:"endpoint"`
			Peers      []struct {
				NodeID      string  `json:"node_id"`
				NodeName    *string `json:"node_name"`
				OverlayIP   string  `json:"overlay_ip"`
				Established bool    `json:"established"`
			} `json:"peers"`
		} `json:"wireguard"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		t.Fatalf("decode node-a: %v", err)
	}
	if node.WireGuard == nil {
		t.Fatal("node get: wireguard block nil, want fabric identity")
	}
	if node.WireGuard.OverlayIP != "10.42.0.1" {
		t.Errorf("overlay_ip = %q, want 10.42.0.1", node.WireGuard.OverlayIP)
	}
	if len(node.WireGuard.Peers) != 1 {
		t.Fatalf("peers = %d, want 1 (%+v)", len(node.WireGuard.Peers), node.WireGuard.Peers)
	}
	p := node.WireGuard.Peers[0]
	if p.OverlayIP != "10.42.0.2" || !p.Established {
		t.Errorf("peer = {overlay %s established %v}, want {10.42.0.2 true}", p.OverlayIP, p.Established)
	}
	if p.NodeName == nil || *p.NodeName != b.name {
		t.Errorf("peer node_name = %v, want %s", p.NodeName, b.name)
	}
}

// TestNodeGetWireguardFabricHiddenFromViewer guards the security property that
// the admin/operator-only WireGuard fabric block on GET /v1/nodes/{name} never
// leaks to a lower-privilege caller: a viewer gets the reduced node summary,
// which carries no wireguard key.
func TestNodeGetWireguardFabricHiddenFromViewer(t *testing.T) {
	h := newE2E(t)
	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)

	a := wgSeedAgent(t, h, caCert, caKey, "node-a")
	wgSendHeartbeat(t, agentSrv.URL, a, &wgHeartbeatReport{PublicKey: "pkA", Endpoint: "a.example:51820", ListenPort: 51820})

	viewerTok, _ := loginAs(t, h, auth.RoleViewer)
	resp := h.get(t, "/v1/nodes/"+a.name, viewerTok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer GET node-a status = %d, want 200", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode node-a: %v", err)
	}
	if _, present := raw["wireguard"]; present {
		t.Errorf("viewer node get response contains a wireguard block; the fabric is admin/operator-only")
	}
}

// wgAssertPeer compares one declared peer against want, field by field.
func wgAssertPeer(t *testing.T, label string, got, want wgDeclaredPeer) {
	t.Helper()
	if got.NodeID != want.NodeID {
		t.Errorf("%s: node_id = %q, want %q", label, got.NodeID, want.NodeID)
	}
	if got.PublicKey != want.PublicKey {
		t.Errorf("%s: public_key = %q, want %q", label, got.PublicKey, want.PublicKey)
	}
	if got.Endpoint != want.Endpoint {
		t.Errorf("%s: endpoint = %q, want %q", label, got.Endpoint, want.Endpoint)
	}
	if got.OverlayIP != want.OverlayIP {
		t.Errorf("%s: overlay_ip = %q, want %q", label, got.OverlayIP, want.OverlayIP)
	}
	if len(got.AllowedIPs) != len(want.AllowedIPs) {
		t.Fatalf("%s: allowed_ips = %v, want %v", label, got.AllowedIPs, want.AllowedIPs)
	}
	for i := range want.AllowedIPs {
		if got.AllowedIPs[i] != want.AllowedIPs[i] {
			t.Errorf("%s: allowed_ips[%d] = %q, want %q", label, i, got.AllowedIPs[i], want.AllowedIPs[i])
		}
	}
}

// wgSendHeartbeat posts a heartbeat for ag over mTLS and decodes the response.
func wgSendHeartbeat(t *testing.T, baseURL string, ag wgAgent, rep *wgHeartbeatReport) wgHeartbeatResponse {
	t.Helper()
	body := wgHeartbeatRequest{
		AgentVersion: "test-0.1.0",
		Architecture: "amd64",
		Capabilities: wgHeartbeatCaps{
			CPUModel:       "test-cpu",
			CPUFlags:       []string{},
			CPUCoresTotal:  4,
			MemoryTotalMib: 8192,
			KernelVersion:  "test",
			QEMUVersion:    "test",
			Firmwares:      []struct{}{},
		},
		Resources: wgHeartbeatRes{
			CPUCoresAvailable:  4,
			MemoryAvailableMib: 8000,
		},
		VMs:       []struct{}{},
		Networks:  []struct{}{},
		Wireguard: rep,
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
		t.Fatalf("heartbeat status = %d, want 200", resp.StatusCode)
	}
	var out wgHeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode heartbeat response: %v", err)
	}
	return out
}

// wgStartAgentTLSServer stands up the agent router behind a TLS listener that
// requires + verifies client certs against caCert - the production agentMTLS
// posture, served over httptest so the test client can present a node leaf.
func wgStartAgentTLSServer(t *testing.T, h *harness, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) *httptest.Server {
	t.Helper()
	router := api.NewAgentRouter(api.RouterDeps{
		Store:             h.store,
		AuthService:       h.svc,
		RequestTimeout:    10 * time.Second,
		ClusterMembership: h.membership,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	srvCert, srvKey := wgSignLeaf(t, caCert, caKey, "wg-test-cp")
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	srvPair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvCert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER}),
	)
	if err != nil {
		t.Fatalf("build server key pair: %v", err)
	}
	srv := httptest.NewUnstartedServer(router)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{srvPair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// wgSeedAgent creates a node row plus an agent_certs row bound to a freshly
// signed leaf, and returns a wgAgent whose HTTP client presents that leaf and
// pins the test CA as its server trust root.
func wgSeedAgent(t *testing.T, h *harness, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, name string) wgAgent {
	t.Helper()
	ctx := context.Background()
	nodeID := uuid.New()
	if _, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID:                      nodeID,
		Name:                    name,
		Architecture:            "amd64",
		AdvertisedEndpoint:      "https://" + name + ".example:8443",
		MigrationHost:           name + ".example",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	}); err != nil {
		t.Fatalf("CreateNode(%s): %v", name, err)
	}

	leafCert, leafKey := wgSignLeaf(t, caCert, caKey, "node-"+name)
	fp := sha256.Sum256(leafCert.Raw)
	if _, err := h.store.CreateAgentCert(ctx, store.CreateAgentCertParams{
		ID:                uuid.New(),
		NodeID:            nodeID,
		Serial:            leafCert.SerialNumber.Bytes(),
		FingerprintSha256: fp[:],
		SubjectDn:         leafCert.Subject.String(),
		NotBefore:         leafCert.NotBefore,
		NotAfter:          leafCert.NotAfter,
	}); err != nil {
		t.Fatalf("CreateAgentCert(%s): %v", name, err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	clientPair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafCert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("build client key pair: %v", err)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{clientPair},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}},
	}
	return wgAgent{nodeID: nodeID, name: name, client: client}
}

// wgGenerateCA generates a self-signed ECDSA CA used to sign agent leafs and to
// anchor both directions of the test mTLS handshake.
func wgGenerateCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wg-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return cert, key
}

// wgSignLeaf signs a leaf cert (server+client auth, localhost SAN) for cn under
// the test CA, returning the parsed cert and its private key.
func wgSignLeaf(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("leaf serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return cert, key
}
