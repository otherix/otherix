// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"sort"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/config"
)

// wgInterfaceName is the single WireGuard interface every agent brings up. All
// peers share it (matches the netfabric primitive).
const wgInterfaceName = "otwg0"

// wgEstablishedWindow bounds how recent a peer's last handshake must be for the
// agent to report it as established. WireGuard rekeys at ~120s under traffic and
// persistent_keepalive (25s) keeps the tunnel warm between 30s heartbeats, so
// 180s flags a genuinely dead peer without false negatives on a healthy one.
const wgEstablishedWindow = 180 * time.Second

// WireGuard is the per-resource reconciler for the agent's WG fabric
// interface. Single instance per agent process. Implements
// heartbeat.ResponseHandler (consumes self_overlay_ip + declared_wireguard_peers)
// and heartbeat.WireGuardReporter (reports observed WG state). Mirrors Pools but
// holds a single desired snapshot rather than a per-name map.
type WireGuard struct {
	log    *slog.Logger
	fabric netfabric.Fabric
	key    wgtypes.Key
	cfg    config.WireGuardConfig
	tick   time.Duration

	desired atomic.Pointer[wgDesired]
	status  atomic.Pointer[wgStatus]
	trigger chan struct{}
	now     func() time.Time
}

// wgDesired is the latest CP-declared WG intent for this node.
type wgDesired struct {
	selfOverlayIP string // CIDR "10.42.0.1/16"; "" until the CP allocates
	peers         []heartbeat.DeclaredWireGuardPeer
	otwg0MTU      *int32 // CP-declared otwg0 link MTU; nil falls back to WireGuardMTU
}

// wgStatus is the outcome of the WG reconciler's most recent pass, surfaced up
// the heartbeat so an otwg0 failure is operator-visible. state is one of
// pending / ready / failed; errMsg carries the failure detail when failed.
type wgStatus struct {
	state  string
	errMsg string
}

// NewWireGuard builds the WG reconciler. tick==0 falls back to
// DefaultTickInterval. Returns ErrNilFabric when fabric is nil.
func NewWireGuard(fabric netfabric.Fabric, key wgtypes.Key, cfg config.WireGuardConfig, log *slog.Logger, tick time.Duration) (*WireGuard, error) {
	if fabric == nil {
		return nil, ErrNilFabric
	}
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	return &WireGuard{
		log:     log,
		fabric:  fabric,
		key:     key,
		cfg:     cfg,
		tick:    tick,
		trigger: make(chan struct{}, 1),
		now:     time.Now,
	}, nil
}

// WireGuardReport implements heartbeat.WireGuardReporter. The pubkey/endpoint/
// listen_port derive from the loaded key + config (independent of whether otwg0
// exists yet), so the CP can allocate the overlay address before the interface
// comes up. established_peers is the observed mesh health.
func (r *WireGuard) WireGuardReport() *heartbeat.WireGuardReport {
	rep := &heartbeat.WireGuardReport{
		PublicKey:            r.key.PublicKey().String(),
		Endpoint:             r.cfg.AdvertisedEndpoint,
		ListenPort:           wgListenPort(r.cfg.ListenPort),
		EstablishedPeers:     r.establishedPeers(),
		ReconciliationStatus: "pending",
	}
	if st := r.status.Load(); st != nil {
		rep.ReconciliationStatus = st.state
		if st.state == "failed" && st.errMsg != "" {
			msg := st.errMsg
			rep.ReconciliationError = &msg
		}
	}
	return rep
}

// establishedPeers reads observed handshakes from the fabric and maps each peer
// whose last handshake is recent (wgEstablishedWindow) back to its node id via
// the declared peer set. Returns nil when otwg0 is absent or the read fails -
// the report still carries pubkey/endpoint/port so the CP can allocate the
// overlay IP before the interface exists (the N2c-1 invariant).
func (r *WireGuard) establishedPeers() []string {
	handshakes, err := r.fabric.WireGuardPeerHandshakes(wgInterfaceName)
	if err != nil {
		r.log.Debug("wireguard peer handshakes unavailable; no established peers",
			slog.String("error", err.Error()))
		return nil
	}
	d := r.desired.Load()
	if d == nil || len(d.peers) == 0 {
		return nil
	}
	byPub := make(map[string]string, len(d.peers)) // pubkey -> node_id
	for _, p := range d.peers {
		byPub[p.PublicKey] = p.NodeID
	}
	now := r.now()
	out := make([]string, 0, len(handshakes))
	for _, hs := range handshakes {
		nodeID, ok := byPub[hs.PublicKey.String()]
		if !ok {
			continue
		}
		if hs.LastHandshake.IsZero() || now.Sub(hs.LastHandshake) > wgEstablishedWindow {
			continue
		}
		out = append(out, nodeID)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// IsKnownNodeOverlayIP reports whether ip is the overlay address of a node the
// CP currently declares as this agent's WireGuard peer. The gateway
// /v1/connect-node splice validates a CP-supplied dial target against this set,
// so it can only ever be steered to a meshed node's overlay IP - never a guest
// VM address or the anycast service IP (the anti-SSRF gate). Reads the latest
// applied desired snapshot; safe for concurrent use, and returns false until the
// first heartbeat populates the peer set (fail closed).
func (r *WireGuard) IsKnownNodeOverlayIP(ip netip.Addr) bool {
	d := r.desired.Load()
	if d == nil {
		return false
	}
	want := ip.Unmap()
	for _, p := range d.peers {
		if p.OverlayIP == "" {
			continue
		}
		peerIP, err := netip.ParseAddr(p.OverlayIP)
		if err != nil {
			continue
		}
		if peerIP.Unmap() == want {
			return true
		}
	}
	return false
}

// SelfOverlayIP returns this node's CP-assigned overlay address and true once a
// heartbeat has delivered self_overlay_ip, or the zero Addr and false before
// then (or if the delivered value does not parse). The agent control server
// binds a second mTLS listener on this address so the gateway /v1/connect-node
// splice can reach a NAT'd node over otwg0. Reads the same atomic desired
// snapshot the reconcile goroutine applies; safe for concurrent use.
func (r *WireGuard) SelfOverlayIP() (netip.Addr, bool) {
	d := r.desired.Load()
	if d == nil || d.selfOverlayIP == "" {
		return netip.Addr{}, false
	}
	pfx, err := netip.ParsePrefix(d.selfOverlayIP)
	if err != nil {
		return netip.Addr{}, false
	}
	return pfx.Addr().Unmap(), true
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. Copies the
// declared intent and nudges the trigger.
func (r *WireGuard) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	d := &wgDesired{
		peers:    append([]heartbeat.DeclaredWireGuardPeer(nil), resp.DeclaredWireGuardPeers...),
		otwg0MTU: resp.Otwg0MTU,
	}
	if resp.SelfOverlayIP != nil {
		d.selfOverlayIP = *resp.SelfOverlayIP
	}
	r.desired.Store(d)
	select {
	case r.trigger <- struct{}{}:
	default:
		// Earlier nudge already queued — collapse into one reconcile.
	}
}

// Run blocks until ctx is cancelled, reconciling on each tick or trigger.
func (r *WireGuard) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.trigger:
			r.reconcile(ctx)
		}
	}
}

// reconcile is one pass: bring up otwg0 with the CP-assigned overlay address
// (carrying the supernet prefix) and replace its peer set. No-op until the
// overlay IP is known. Errors are logged and retried on the next tick; the
// observed report is independent of reconcile success.
func (r *WireGuard) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	d := r.desired.Load()
	if d == nil || d.selfOverlayIP == "" {
		r.setStatus("pending", "")
		return
	}
	addr, err := netip.ParsePrefix(d.selfOverlayIP)
	if err != nil {
		r.log.WarnContext(ctx, "wireguard self_overlay_ip not a prefix; skipping",
			slog.String("self_overlay_ip", d.selfOverlayIP),
			slog.String("error", err.Error()))
		r.setStatus("failed", fmt.Sprintf("self_overlay_ip %q not a prefix: %v", d.selfOverlayIP, err))
		return
	}
	mtu := netfabric.WireGuardMTU // fallback: older CP / underlay MTU not yet known
	if d.otwg0MTU != nil {
		mtu = int(*d.otwg0MTU)
	}
	if err := r.fabric.EnsureWireGuard(netfabric.WGConfig{
		Name:       wgInterfaceName,
		PrivateKey: r.key,
		ListenPort: r.cfg.ListenPort,
		Address:    addr,
		MTU:        mtu, // otwg0 carries VXLAN; reapplied each pass (drift-heal)
	}); err != nil {
		r.log.WarnContext(ctx, "wireguard ensure interface failed",
			slog.String("interface", wgInterfaceName),
			slog.String("error", err.Error()))
		r.setStatus("failed", fmt.Sprintf("ensure %s: %v", wgInterfaceName, err))
		return
	}
	peers := r.toWGPeers(ctx, d.peers)
	if err := r.fabric.SetWireGuardPeers(wgInterfaceName, peers); err != nil {
		r.log.WarnContext(ctx, "wireguard set peers failed",
			slog.String("interface", wgInterfaceName),
			slog.String("error", err.Error()))
		r.setStatus("failed", fmt.Sprintf("set peers on %s: %v", wgInterfaceName, err))
		return
	}
	r.setStatus("ready", "")
}

// setStatus records the outcome of the latest reconcile pass for the next
// WireGuardReport.
func (r *WireGuard) setStatus(state, errMsg string) {
	r.status.Store(&wgStatus{state: state, errMsg: errMsg})
}

// toWGPeers translates declared peers into netfabric.WGPeer. A peer that fails
// to parse is skipped + logged so one bad entry never drops the whole mesh. When
// the declared list is empty (single agent), this returns an
// empty slice.
func (r *WireGuard) toWGPeers(ctx context.Context, declared []heartbeat.DeclaredWireGuardPeer) []netfabric.WGPeer {
	out := make([]netfabric.WGPeer, 0, len(declared))
	for _, p := range declared {
		pk, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			r.log.WarnContext(ctx, "wireguard peer pubkey unparseable; skipping",
				slog.String("node_id", p.NodeID), slog.String("error", err.Error()))
			continue
		}
		var endpoint *net.UDPAddr
		if p.Endpoint != "" {
			endpoint, err = net.ResolveUDPAddr("udp", p.Endpoint)
			if err != nil {
				r.log.WarnContext(ctx, "wireguard peer endpoint unresolvable; skipping",
					slog.String("node_id", p.NodeID), slog.String("error", err.Error()))
				continue
			}
		}
		allowed := make([]netip.Prefix, 0, len(p.AllowedIPs))
		for _, a := range p.AllowedIPs {
			pfx, perr := netip.ParsePrefix(a)
			if perr != nil {
				r.log.WarnContext(ctx, "wireguard peer allowed_ip unparseable; skipping prefix",
					slog.String("node_id", p.NodeID), slog.String("allowed_ip", a))
				continue
			}
			allowed = append(allowed, pfx)
		}
		out = append(out, netfabric.WGPeer{
			PublicKey:           pk,
			Endpoint:            endpoint,
			AllowedIPs:          allowed,
			PersistentKeepalive: r.cfg.PersistentKeepalive,
		})
	}
	return out
}

// wgListenPort narrows the config listen port to the wire schema's int32,
// saturating on overflow. Mirrors the heartbeat collector's clampToInt32,
// which is unexported in that package and so cannot be reused here. Ports are
// always within [0, 65535], well below math.MaxInt32; the clamp keeps gosec
// G115 quiet without obscuring intent.
func wgListenPort(p int) int32 {
	if p > math.MaxInt32 {
		return math.MaxInt32
	}
	if p < 0 {
		return 0
	}
	return int32(p)
}
