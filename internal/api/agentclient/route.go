// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

const (
	// connectTimeout bounds the whole /v1/connect-node exchange on the OUTER conn.
	// The transport does not yet track this conn, so http.Client.Timeout cannot
	// interrupt a gateway that completes the outer handshake then stalls; this
	// deadline is the only backstop against a wedged dial goroutine + leaked fd.
	connectTimeout = 30 * time.Second
	// maxConnectHeaderBytes caps the CONNECT response head so a malicious gateway
	// streaming unbounded headers cannot OOM the CP. connectTimeout bounds time on
	// the exchange; this bounds bytes.
	maxConnectHeaderBytes = 64 << 10
)

// DialURL returns the identity URL the CP dials to reach node nodeName: an https
// URL whose host is the node's cluster-CA identity SAN (auth.NodeIdentitySAN).
// Handlers pass DialURL(node.Name) instead of node.AdvertisedEndpoint so that
//   - the transport's default TLS ServerName is the node identity (not the
//     dialed IP), pinning the connection to the node's leaf cert, and
//   - the injected RouteResolver can splice a NAT'd node through a gateway
//     without any method-signature change (the resolver keys on the identity).
//
// The URL carries no port; the geo DialContext resolves the real dial target.
func DialURL(nodeName string) string {
	return "https://" + auth.NodeIdentitySAN(nodeName)
}

// AgentRoute is how a RouteResolver tells the transport to reach a node.
// For a resolved node exactly one of Direct / GatewayDial is set; the zero
// value means "no special route" and the transport dials the host:port it
// already computed from the URL (used for raw-endpoint callers and tests).
type AgentRoute struct {
	// Direct is the host:port to TCP-dial for a publicly reachable node. The
	// transport then runs the ordinary agent TLS over it (ServerName == the
	// identity URL host), so the identity pin holds for free.
	Direct string
	// GatewayDial is the gateway control host:port to dial for a NAT'd node.
	GatewayDial string
	// GatewaySAN is the gateway's identity SAN, used as the OUTER TLS ServerName
	// so the gateway's RequireCPIdentity passes and the CP verifies the gateway.
	GatewaySAN string
	// TargetOverlay is "overlayIP:controlPort" the gateway splices the inner
	// connection to (the NAT'd node's overlay-IP control listener).
	TargetOverlay string
}

// RouteResolver maps a node name to the route the CP uses to reach it. A nil
// AgentRoute-error (zero route) tells the transport to dial the address it
// already computed, so a resolver may fall back for hosts it does not own.
type RouteResolver interface {
	Resolve(nodeName string) (AgentRoute, error)
}

// DirectResolver is the default resolver: it never rewrites a route, so the
// transport dials the host:port the URL already names. Callers (and tests) that
// pass a real advertised endpoint as the dial URL are unaffected.
type DirectResolver struct{}

// Resolve always returns the zero route.
func (DirectResolver) Resolve(string) (AgentRoute, error) { return AgentRoute{}, nil }

// geoDialer is the transport's DialContext: it resolves the identity host in the
// dialed address to a route and returns a conn the transport runs the (inner)
// agent TLS over. It holds the CP client material only for the OUTER gateway
// dial; the inner agent TLS is the transport's own TLSClientConfig.
type geoDialer struct {
	resolver RouteResolver
	dialer   *net.Dialer
	tlsCerts []tls.Certificate // CP client cert(s) for the OUTER gateway dial
	rootCAs  *x509.CertPool    // cluster CA, to verify the gateway (and agent)
}

// DialContext resolves addr's identity host to a route and dials it. A direct
// route yields a raw TCP conn; a via-gateway route yields the OUTER-TLS conn
// post-CONNECT, so the transport's inner handshake tunnels opaquely through the
// gateway splice (two TLS layers). It never mutates the shared TLSClientConfig.
func (g *geoDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	route, err := g.resolver.Resolve(nodeNameFromDialHost(host))
	if err != nil {
		return nil, err
	}
	switch {
	case route.GatewayDial != "":
		return g.dialViaGateway(ctx, network, route)
	case route.Direct != "":
		return g.dialer.DialContext(ctx, network, route.Direct)
	default:
		return g.dialer.DialContext(ctx, network, addr)
	}
}

// nodeNameFromDialHost inverts DialURL's host: node-<name>.agents.otherix.local
// -> <name>. A host that is not an identity SAN (a raw advertised-endpoint host
// or a test URL host) is returned unchanged, so the resolver can fall back to
// dialing it directly.
func nodeNameFromDialHost(host string) string {
	name := strings.TrimSuffix(host, ".agents.otherix.local")
	if name == host { // suffix absent
		return host
	}
	return strings.TrimPrefix(name, "node-")
}

// dialViaGateway performs the two-TLS-layer CP-splice leg for a NAT'd node.
func (g *geoDialer) dialViaGateway(ctx context.Context, network string, route AgentRoute) (net.Conn, error) {
	raw, err := g.dialer.DialContext(ctx, network, route.GatewayDial)
	if err != nil {
		return nil, fmt.Errorf("agentclient: dial gateway %s: %w", route.GatewayDial, err)
	}
	// OUTER TLS: present the CP client cert and pin ServerName to the gateway
	// identity so the gateway's RequireCPIdentity passes and the CP verifies it.
	outer := tls.Client(raw, &tls.Config{
		Certificates: g.tlsCerts,
		RootCAs:      g.rootCAs,
		ServerName:   route.GatewaySAN,
		MinVersion:   tls.VersionTLS13,
	})
	if err := outer.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("agentclient: gateway %s TLS handshake: %w", route.GatewaySAN, err)
	}
	if err := connectNode(ctx, outer, route); err != nil {
		_ = outer.Close()
		return nil, err
	}
	// Return the post-CONNECT outer conn; the transport runs the INNER agent TLS
	// over it, and the gateway forwards the opaque inner bytes to the node.
	return outer, nil
}

// connectNodeRequest is the CP-splice CONNECT body the gateway's connect-node
// handler validates. Keep this shape stable:
// {"overlay_ip":"<ip>","port":<control-port>}.
type connectNodeRequest struct {
	OverlayIP string `json:"overlay_ip"`
	Port      int    `json:"port"`
}

// connectNode sends POST /v1/connect-node over the OUTER TLS conn naming the
// overlay target and consumes the response head. On 200 the conn is left exactly
// at the tunnel boundary (no inner bytes yet), ready for the inner handshake.
func connectNode(ctx context.Context, conn net.Conn, route AgentRoute) error {
	ip, portStr, err := net.SplitHostPort(route.TargetOverlay)
	if err != nil {
		return fmt.Errorf("agentclient: bad target overlay %q: %v", route.TargetOverlay, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("agentclient: bad target overlay port %q: %v", portStr, err)
	}
	body, err := json.Marshal(connectNodeRequest{OverlayIP: ip, Port: port})
	if err != nil {
		return fmt.Errorf("agentclient: marshal connect-node: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://"+route.GatewaySAN+"/v1/connect-node", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("agentclient: build connect-node: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Bound the exchange in time before any I/O: a gateway that completes the
	// outer handshake but never answers must not wedge this goroutine. Prefer a
	// sooner caller deadline when the ctx carries one.
	deadline := time.Now().Add(connectTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("agentclient: set connect-node deadline: %v", err)
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("agentclient: write connect-node: %w", err)
	}

	// Read the status line + headers only, bounded in bytes so an unbounded header
	// stream cannot OOM. The gateway must not send any inner bytes before the CP's
	// ClientHello, so nothing may be buffered past the header terminator; fail
	// closed if it is, rather than lose tunnel bytes.
	br := bufio.NewReader(io.LimitReader(conn, maxConnectHeaderBytes))
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		return fmt.Errorf("agentclient: read connect-node status: %w", err)
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		return fmt.Errorf("agentclient: read connect-node headers: %w", err)
	}
	if code := statusCode(statusLine); code != http.StatusOK {
		return fmt.Errorf("agentclient: gateway connect-node status %q", statusLine)
	}
	if n := br.Buffered(); n != 0 {
		return fmt.Errorf("agentclient: gateway sent %d unexpected bytes after connect-node 200", n)
	}
	// Success: clear the deadline so the INNER agent TLS handshake and all later
	// I/O on this conn are unaffected. Load-bearing - a stale deadline here would
	// break the inner handshake or a subsequent pooled reuse of the conn.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("agentclient: clear connect-node deadline: %v", err)
	}
	return nil
}

// statusCode extracts the numeric code from an HTTP status line
// ("HTTP/1.1 200 OK" -> 200); a malformed line yields 0.
func statusCode(statusLine string) int {
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return code
}
