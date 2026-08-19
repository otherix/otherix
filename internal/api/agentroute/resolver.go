// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package agentroute is the CP-side concrete RouteResolver the api-server injects
// into agentclient. It maps a node identity to a dial route: a direct control
// endpoint for a publicly reachable node, or a gateway CONNECT splice for a NAT'd
// one. It lives outside agentclient (which the handlers import) so the resolver
// can read node rows without an import cycle; agentclient never imports it.
package agentroute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// defaultControlPort mirrors the agent Server.Listen default (config/agent.go)
// and is used only when a node's advertised endpoint carries no explicit port.
const defaultControlPort = "9443"

// resolveTimeout bounds the store reads a single Resolve does. Resolve runs once
// per new connection (pooled conns skip it), so this is a per-dial floor, not a
// per-request cost.
const resolveTimeout = 5 * time.Second

// NodeReader is the narrow store surface the resolver consumes; *etcdstore.Store
// satisfies it.
type NodeReader interface {
	NodeByName(ctx context.Context, name string) (store.Node, error)
	AgentWireguardByNodeID(ctx context.Context, nodeID uuid.UUID) (store.AgentWireguard, error)
	AllNodes(ctx context.Context) ([]store.Node, error)
}

// Resolver resolves a node name to an agentclient.AgentRoute.
type Resolver struct {
	reader NodeReader
}

// New returns a Resolver over reader.
func New(reader NodeReader) *Resolver { return &Resolver{reader: reader} }

// Resolve implements agentclient.RouteResolver. A public node (or a node with no
// WireGuard state) resolves to a Direct control-endpoint dial; a NAT'd node (a WG
// row with an empty Endpoint) resolves to a splice through the lowest-UUID
// gateway it currently reaches. An unknown node name yields the zero route so the
// transport falls back to dialing the address it computed (raw-endpoint callers).
func (r *Resolver) Resolve(nodeName string) (agentclient.AgentRoute, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	node, err := r.reader.NodeByName(ctx, nodeName)
	if errors.Is(err, store.ErrNotFound) {
		// Not a known node (a raw advertised-endpoint host, a test URL, or truly
		// absent): no special route. The dial falls back to the computed addr.
		return agentclient.AgentRoute{}, nil
	}
	if err != nil {
		return agentclient.AgentRoute{}, fmt.Errorf("agentroute: resolve node %q: %w", nodeName, err)
	}

	wg, wgErr := r.reader.AgentWireguardByNodeID(ctx, node.ID)
	if wgErr != nil && !errors.Is(wgErr, store.ErrNotFound) {
		// A transient store error is NOT "no WG state": fail closed with a
		// retryable error rather than mis-route a NAT'd node to a broken Direct
		// dial (a wrong route would swallow the fault and hit the wrong host).
		return agentclient.AgentRoute{}, fmt.Errorf("agentroute: wireguard state for node %q: %w", nodeName, wgErr)
	}
	natd := wgErr == nil && wg.Endpoint == ""
	if !natd {
		// Public node (WG endpoint set, or no WG state yet): dial its control
		// endpoint directly. The identity SAN in the URL host pins the TLS.
		return agentclient.AgentRoute{Direct: endpointHostPort(node.AdvertisedEndpoint)}, nil
	}

	// A gateway only splices to a target on the control port IT listens on, so a
	// route can be composed only for a target that listens on that same port. An
	// unreported port (zero) is unknown, never a default: guessing one would
	// compose a target the gateway refuses permanently, with no diagnostic on this
	// side. Both conditions fold into the gateway selection so the error names the
	// real state.
	if node.ControlListenPort <= 0 {
		return agentclient.AgentRoute{}, fmt.Errorf(
			"agentroute: node %q is NAT'd and has not reported its control listener port", nodeName)
	}
	gw, ok := r.selectGateway(ctx, wg, node.ControlListenPort)
	if !ok {
		return agentclient.AgentRoute{}, fmt.Errorf(
			"agentroute: node %q is NAT'd and no reachable gateway serves the gateway plane on control port %d",
			nodeName, node.ControlListenPort)
	}
	return agentclient.AgentRoute{
		GatewayDial:   endpointHostPort(gw.AdvertisedEndpoint),
		GatewaySAN:    auth.NodeIdentitySAN(gw.Name),
		TargetOverlay: net.JoinHostPort(wg.OverlayIP.String(), strconv.Itoa(int(node.ControlListenPort))),
	}, nil
}

// selectGateway picks the lowest-UUID gateway-role node the NAT'd node currently
// reaches (in its EstablishedPeers) that the CP can dial (has an advertised
// endpoint), that actually serves the gateway plane, and that listens on the
// same control port as the target (controlPort) - the only port it accepts a
// splice target on. Empty intersection => (_, false): no route is wired.
//
// The gateway role is a CP-side bit an operator can set on any node; whether the
// node's agent serves the gateway plane is decided from its own config, and the
// reported ingress endpoint is the signal for it (an agent reports one only when
// the plane is up). Without that check the CP would splice to a node that never
// mounted the splice route - a permanent 404 on every call to the NAT'd nodes
// behind it. Skipping such a node instead makes the failure loud and localised.
func (r *Resolver) selectGateway(ctx context.Context, wg store.AgentWireguard, controlPort int32) (store.Node, bool) {
	reached := make(map[uuid.UUID]bool, len(wg.EstablishedPeers))
	for _, s := range wg.EstablishedPeers {
		if id, err := uuid.Parse(s); err == nil {
			reached[id] = true
		}
	}
	if len(reached) == 0 {
		return store.Node{}, false
	}
	nodes, err := r.reader.AllNodes(ctx)
	if err != nil {
		return store.Node{}, false
	}
	var best store.Node
	found := false
	for _, n := range nodes {
		if !n.GatewayRole || n.AdvertisedEndpoint == "" || n.IngressAdvertisedEndpoint == "" || !reached[n.ID] {
			continue
		}
		if n.ControlListenPort != controlPort {
			continue
		}
		if !found || bytes.Compare(n.ID[:], best.ID[:]) < 0 {
			best, found = n, true
		}
	}
	return best, found
}

// endpointHostPort extracts host:port from an https endpoint URL, defaulting the
// port to the agent control-listener default (defaultControlPort) when absent.
// A URL that will not parse is returned unchanged so the dial surfaces the real
// error rather than a rewritten one.
func endpointHostPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), defaultControlPort)
	}
	return u.Host
}
