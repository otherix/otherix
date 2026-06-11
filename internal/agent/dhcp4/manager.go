// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// packetConn is one bridge's raw L2 socket: it yields inbound DHCP request
// payloads (UDP/67) and sends reply payloads back.
type packetConn interface {
	// recv returns the next DHCP request payload (the UDP body, i.e. the bytes
	// dhcpv4.FromBytes expects).
	recv() (payload []byte, err error)
	// send frames payload as Ethernet/IPv4/UDP (server 169.254.1.1:67 -> client
	// :68) to dstMAC and writes it.
	send(payload []byte, dstMAC net.HardwareAddr) error
	close() error
}

// socketFactory opens a packetConn on the named overlay bridge.
type socketFactory func(bridge string) (packetConn, error)

// Config parametrises a responder.
type Config struct {
	Log *slog.Logger
	// Gateway is the server identifier and option-121 next hop. Defaults to
	// netfabric.OverlayGatewayAddr when zero.
	Gateway netip.Addr
	// DNS is option 6. Defaults to netfabric.OverlayGatewayAddr when zero.
	DNS netip.Addr
	// Lease is option 51. Defaults to DefaultLease when zero.
	Lease time.Duration
}

// responder serves DHCPv4 on overlay bridges for CP-IPAM reservations. It owns
// one raw AF_PACKET socket per active bridge, opened lazily on the first
// RegisterNetwork for that bridge and closed on DeregisterNetwork.
//
// Ordering: RegisterNetwork may be called before or after Run. Calls before Run
// buffer the config; Run opens their sockets and starts serving once it holds a
// context. Calls after Run open the socket and start serving immediately.
type responder struct {
	log     *slog.Logger
	reply   ReplyOptions
	newConn socketFactory

	ready chan struct{}

	mu     sync.Mutex
	runCtx context.Context
	nets   map[string]*bridgeServer // keyed by networkID
}

// snapshot is the immutable per-bridge serving state swapped atomically on
// re-register so the read-loop never holds a lock to look up a reservation.
type snapshot struct {
	subnet netip.Prefix
	byMAC  map[string]Reservation // key = MAC string
}

// bridgeServer owns one bridge's socket and its read-loop. conn is nil until the
// server starts (a network registered before Run buffers here with no socket).
type bridgeServer struct {
	bridge string
	conn   packetConn
	snap   atomic.Pointer[snapshot]

	cancel context.CancelFunc
	done   chan struct{}
}

// New validates cfg and returns a responder. Gateway and DNS default to the
// overlay anycast gateway; Lease defaults to DefaultLease.
func New(cfg Config) (*responder, error) {
	if cfg.Log == nil {
		return nil, errors.New("dhcp4: Log is required")
	}
	gw := cfg.Gateway
	if !gw.IsValid() {
		gw = netfabric.OverlayGatewayAddr
	}
	dns := cfg.DNS
	if !dns.IsValid() {
		dns = netfabric.OverlayGatewayAddr
	}
	lease := cfg.Lease
	if lease == 0 {
		lease = DefaultLease
	}
	return &responder{
		log:     cfg.Log,
		reply:   ReplyOptions{Gateway: gw, DNS: dns, Lease: lease},
		newConn: openAFPacket,
		ready:   make(chan struct{}),
		nets:    make(map[string]*bridgeServer),
	}, nil
}

// Ready is closed once Run has taken its context and started any buffered
// servers, so callers (and tests) can wait for serving to begin.
func (r *responder) Ready() <-chan struct{} { return r.ready }

// Run takes the serving context, starts read-loops for any networks registered
// before Run, and blocks until ctx is cancelled. On cancel it closes every
// bridge socket and waits for their read-loops to drain.
func (r *responder) Run(ctx context.Context) error {
	r.mu.Lock()
	r.runCtx = ctx
	for id, srv := range r.nets {
		if err := r.startServerLocked(srv); err != nil {
			// A buffered network whose socket fails to open is dropped rather
			// than aborting startup of the other bridges; the reconciler will
			// re-register on its next pass.
			r.log.Error("dhcp open socket failed", "bridge", srv.bridge, "network_id", id, "error", err.Error())
			delete(r.nets, id)
		}
	}
	r.mu.Unlock()

	close(r.ready)
	r.log.Info("dhcp responder serving")

	<-ctx.Done()

	r.mu.Lock()
	servers := make([]*bridgeServer, 0, len(r.nets))
	for _, srv := range r.nets {
		servers = append(servers, srv)
	}
	r.nets = make(map[string]*bridgeServer)
	r.mu.Unlock()

	for _, srv := range servers {
		r.stopServer(srv)
	}
	return ctx.Err()
}

// RegisterNetwork registers or updates the DHCP service for one overlay bridge.
// It is idempotent: a re-register for the same networkID atomically swaps the
// reservation set and subnet without reopening the socket. If the bridge name
// changed for an existing networkID, the old socket is closed and a new one
// opened.
func (r *responder) RegisterNetwork(cfg NetworkConfig) error {
	snap := buildSnapshot(cfg)

	r.mu.Lock()
	defer r.mu.Unlock()

	if srv, ok := r.nets[cfg.NetworkID]; ok {
		if srv.bridge == cfg.Bridge {
			srv.snap.Store(snap)
			return nil
		}
		// Bridge changed: tear the old server down (cancel + close + drain) and
		// fall through to reopen. The read-loop never takes r.mu, so waiting on
		// its done channel while holding r.mu cannot deadlock; it exits via the
		// ctx cancel and closed socket independent of the lock.
		r.stopServer(srv)
		delete(r.nets, cfg.NetworkID)
	}

	srv := &bridgeServer{bridge: cfg.Bridge}
	srv.snap.Store(snap)
	r.nets[cfg.NetworkID] = srv

	// Before Run the server is buffered with no socket; Run opens it. After Run
	// open the socket and start serving now.
	if r.runCtx != nil {
		if err := r.startServerLocked(srv); err != nil {
			delete(r.nets, cfg.NetworkID)
			return fmt.Errorf("dhcp4: open socket on %s: %w", cfg.Bridge, err)
		}
	}
	return nil
}

// DeregisterNetwork closes the socket and stops serving the network. It is a
// no-op if the network is not registered.
func (r *responder) DeregisterNetwork(networkID string) error {
	r.mu.Lock()
	srv, ok := r.nets[networkID]
	if ok {
		delete(r.nets, networkID)
	}
	r.mu.Unlock()

	if !ok {
		return nil
	}
	r.stopServer(srv)
	return nil
}

// startServerLocked opens srv's socket if needed and launches its read-loop.
// Caller holds r.mu and has set r.runCtx.
func (r *responder) startServerLocked(srv *bridgeServer) error {
	if srv.done != nil {
		return nil // already running
	}
	if srv.conn == nil {
		conn, err := r.newConn(srv.bridge)
		if err != nil {
			return err
		}
		srv.conn = conn
	}
	ctx, cancel := context.WithCancel(r.runCtx)
	srv.cancel = cancel
	srv.done = make(chan struct{})
	go r.serve(ctx, srv)
	return nil
}

// stopServer cancels srv's read-loop, closes its socket, and waits for the loop
// to drain. Safe to call without holding r.mu.
func (r *responder) stopServer(srv *bridgeServer) {
	if srv.cancel != nil {
		srv.cancel()
	}
	if srv.conn != nil {
		_ = srv.conn.close()
	}
	if srv.done != nil {
		<-srv.done
	}
}

// serveErrBackoffThreshold is the number of consecutive recv errors tolerated
// at full speed before the loop starts backing off. A transient blip (EINTR,
// ENOBUFS) clears well under this; a wedged fd (e.g. ifindex removed, recv
// erroring every call without blocking) trips it into a slow poll.
const serveErrBackoffThreshold = 4

// serveErrBackoffBase and serveErrBackoffMax bound the backoff: once the loop is
// wedged it sleeps base, doubling per error up to max, so a stuck socket polls
// at most ~5/s instead of burning a core.
const (
	serveErrBackoffBase = 10 * time.Millisecond
	serveErrBackoffMax  = 500 * time.Millisecond
)

// serve is srv's read-loop: it receives frames, looks up the client MAC in the
// current snapshot, builds a reply for known MACs, and sends it.
//
// The only fatal path is ctx cancel (deregister/shutdown, which also closes the
// socket via stopServer): on it serve returns cleanly. Every other recv error
// is transient (EINTR from async-preemption/signals, ENOBUFS under flood) and
// must NOT dark the bridge: serve logs, counts it, and continues. A genuinely
// removed bridge is reaped by the reconciler's DeregisterNetwork (ctx cancel),
// not by serve closing its own socket - closing here without reopening would
// re-dark a live bridge. To avoid hot-spinning on a persistent error state, the
// loop backs off (ctx-interruptibly) once errors run consecutive.
func (r *responder) serve(ctx context.Context, srv *bridgeServer) {
	defer close(srv.done)
	consecErrs := 0
	for {
		payload, err := srv.conn.recv()
		if err != nil {
			if ctx.Err() != nil {
				// Deregister/shutdown cancelled the ctx (and closed the socket);
				// this is the only fatal path.
				return
			}
			consecErrs++
			r.log.Warn("dhcp recv error", "bridge", srv.bridge, "error", err.Error(), "consecutive_errors", consecErrs)
			if consecErrs > serveErrBackoffThreshold {
				d := serveErrBackoff(consecErrs)
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
				}
			}
			continue
		}
		consecErrs = 0
		r.handle(srv, payload)
	}
}

// serveErrBackoff returns the sleep for the nth consecutive recv error past the
// threshold: serveErrBackoffBase doubled once per extra error, capped at
// serveErrBackoffMax.
func serveErrBackoff(consecErrs int) time.Duration {
	d := serveErrBackoffBase
	for i := serveErrBackoffThreshold + 1; i < consecErrs; i++ {
		d *= 2
		if d >= serveErrBackoffMax {
			return serveErrBackoffMax
		}
	}
	return d
}

// dhcpParse parses one raw DHCP payload. It is a package-level seam over
// dhcpv4.FromBytes (the accepted mutable-global exception) so tests can force
// a parse panic and prove handle's recover keeps the serve loop alive;
// production code never reassigns it.
var dhcpParse = dhcpv4.FromBytes

// handle parses one DHCP request, resolves the reservation for the client MAC,
// and sends the reply. Unknown MACs are dropped. It recovers from panics
// (payload is guest-controlled and crosses a third-party parser): a panicking
// frame is logged and dropped rather than killing the agent process.
func (r *responder) handle(srv *bridgeServer, payload []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("dhcp handler panicked, dropping frame", "bridge", srv.bridge, "panic", fmt.Sprintf("%v", rec), "stack", string(debug.Stack()))
		}
	}()
	req, err := dhcpParse(payload)
	if err != nil {
		r.log.Debug("dhcp parse error", "bridge", srv.bridge, "error", err.Error())
		return
	}
	snap := srv.snap.Load()
	res, ok := snap.byMAC[req.ClientHWAddr.String()]
	if !ok {
		r.log.Debug("dhcp unknown client, dropping", "bridge", srv.bridge, "mac", req.ClientHWAddr.String())
		return
	}
	reply, err := buildReply(req, res, snap.subnet, r.reply)
	if err != nil {
		r.log.Warn("dhcp build reply failed", "bridge", srv.bridge, "mac", req.ClientHWAddr.String(), "error", err.Error())
		return
	}
	dst := req.ClientHWAddr
	if req.IsBroadcast() {
		dst = broadcastMAC
	}
	if err := srv.conn.send(reply.ToBytes(), dst); err != nil {
		r.log.Warn("dhcp send failed", "bridge", srv.bridge, "mac", req.ClientHWAddr.String(), "error", err.Error())
	}
}

// broadcastMAC is the Ethernet broadcast address used when a client sets the
// DHCP broadcast flag (it has no unicast address to receive on yet).
var broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// buildSnapshot builds the immutable serving snapshot for a network config.
func buildSnapshot(cfg NetworkConfig) *snapshot {
	byMAC := make(map[string]Reservation, len(cfg.Reservations))
	for _, res := range cfg.Reservations {
		byMAC[res.MAC.String()] = res
	}
	return &snapshot{subnet: cfg.Subnet, byMAC: byMAC}
}
