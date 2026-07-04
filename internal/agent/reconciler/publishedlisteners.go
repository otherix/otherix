// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// ListenerManager is the narrow socket-binding seam the published-listener
// reconciler needs. The production binder opens a real TCP listener on all
// interfaces; tests inject a fake to observe bind/close bookkeeping without
// a live socket.
type ListenerManager interface {
	Listen(ctx context.Context, port int32) (net.Listener, error)
}

// netListenerManager is the production ListenerManager. It binds :port on
// every interface via net.ListenConfig, matching the public reachability a
// gateway's published load balancer requires.
type netListenerManager struct{}

func (netListenerManager) Listen(ctx context.Context, port int32) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", ":"+strconv.Itoa(int(port)))
}

// listenerConfig is the per-connection datapath config a live published-port
// listener serves. It is carried behind an atomic pointer on boundListener so a
// heartbeat that changes the backend set or source allowlist reaches an already
// bound listener without rebinding the socket: reconcile Stores a fresh config
// and the accept goroutine Loads it per accepted connection. backendPort is the
// guest port each backend is dialed at, sourceCIDRs the optional source
// allowlist (empty means allow-all), backends the CP-resolved eligible set.
type listenerConfig struct {
	backendPort int32
	sourceCIDRs []string
	backends    []heartbeat.DeclaredBackend
}

// configFromLB snapshots the datapath-relevant fields of a declared load
// balancer into a fresh listenerConfig for atomic publication. The backend
// slice is copied so a later mutation of the caller's DeclaredLoadBalancer
// cannot race a reader that already Loaded this config.
func configFromLB(lb heartbeat.DeclaredLoadBalancer) *listenerConfig {
	return &listenerConfig{
		backendPort: lb.BackendPort,
		sourceCIDRs: append([]string(nil), lb.SourceCIDRs...),
		backends:    append([]heartbeat.DeclaredBackend(nil), lb.Backends...),
	}
}

// boundListener is one open (or attempted) published-port listener. ln is
// nil when the last bind attempt failed; err then carries the failure
// string. lbID keys the entry back to its owning load balancer so the
// observed up-channel report can name it. cfg carries the live datapath config
// (backend set, source allowlist, backend port) refreshed in place each
// reconcile pass; it is nil only on a failed bind (no live socket to serve).
type boundListener struct {
	ln   net.Listener
	lbID uuid.UUID
	err  string
	cfg  *atomic.Pointer[listenerConfig]
}

// PublishedListeners is the per-resource reconciler for published load
// balancer ports. It follows the ADR-0027 skeleton (atomic desired cache +
// buffered trigger + Run tick loop) but is the first reconciler that owns
// kernel sockets: it binds a raw TCP listener for every declared published
// port and releases them all on shutdown.
//
// Each accepted connection is served by the raw datapath (handleConn): a
// source-IP ACL, a backend selection, a slot acquire, an overlay-device
// resolve, an anti-SSRF neighbor-MAC pin, an SO_BINDTODEVICE dial, and a
// bidirectional splice. Every uncertain step closes the connection (fail toward
// inaction).
type PublishedListeners struct {
	log  *slog.Logger
	mgr  ListenerManager
	tick time.Duration

	// Datapath seams. slots and rnd are always set (safe defaults in the
	// constructor). devices, neighbors, and dialer are the production overlay
	// wiring, injected by the agent server; they are exercised only once a
	// listener has an eligible backend, so an empty-backend LB closes at the
	// select step before any is touched.
	devices   deviceResolver
	neighbors neighborResolver
	dialer    datapathDialer
	slots     *slotLimiter
	rnd       func(int) int

	// idleTimeout is the per-connection idle window handed to the splice. It is
	// always set to publishedIdleTimeout by the constructor; tests shrink it to
	// exercise the reclaim path. Production behavior is unchanged.
	idleTimeout time.Duration

	desired atomic.Pointer[[]heartbeat.DeclaredLoadBalancer]
	trigger chan struct{}

	mu    sync.Mutex
	bound map[int32]boundListener // published_port -> open listener + owning LB
}

// NewPublishedListeners builds the published-listener reconciler. A nil mgr
// falls back to the production net-backed binder; tests pass a fake. A nil dialer
// falls back to the production overlay dialer (SO_BINDTODEVICE via
// netfabric.BindToDeviceControl); tests inject a fake. devices and neighbors are
// the overlay-device resolver and neighbor-MAC resolver the datapath consumes
// (the agent server passes the real *Networks and netfabric.Fabric). slots and
// rnd always get safe defaults. tick==0 falls back to DefaultTickInterval.
func NewPublishedListeners(mgr ListenerManager, devices deviceResolver, neighbors neighborResolver, dialer datapathDialer, log *slog.Logger, tick time.Duration) *PublishedListeners {
	if mgr == nil {
		mgr = netListenerManager{}
	}
	if dialer == nil {
		dialer = overlayDialer{}
	}
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	return &PublishedListeners{
		log:         log,
		mgr:         mgr,
		tick:        tick,
		devices:     devices,
		neighbors:   neighbors,
		dialer:      dialer,
		slots:       newSlotLimiter(publishedPerBackendCap, publishedGatewayCap),
		rnd:         rand.IntN,
		idleTimeout: publishedIdleTimeout,
		trigger:     make(chan struct{}, 1),
		bound:       map[int32]boundListener{},
	}
}

// PublishedListenerReports builds the observed up-channel projection: one report
// per entry in the bound map, carrying the owning LB id, the published port, and
// the bind verdict (Bound == the last bind succeeded; Error is the failure string
// otherwise). Ports no longer in bound are naturally absent - a torn-down listener
// stops being reported, mirroring how PoolReports only reports live pools.
func (r *PublishedListeners) PublishedListenerReports() []heartbeat.PublishedListenerReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]heartbeat.PublishedListenerReport, 0, len(r.bound))
	for port, bl := range r.bound {
		out = append(out, heartbeat.PublishedListenerReport{
			LBID:  bl.lbID,
			Port:  port,
			Bound: bl.err == "",
			Error: bl.err,
		})
	}
	return out
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. Invoked by
// the sender immediately after a successful heartbeat POST, outside this
// reconciler's goroutine. We copy the slice (the sender's struct may be
// reused) and nudge the trigger. Storing a pointer to the copy makes the
// desired cache non-nil after the first response even when the slice is
// empty, which lets reconcile distinguish "no response yet" from "no
// published LBs".
func (r *PublishedListeners) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	lbs := append([]heartbeat.DeclaredLoadBalancer(nil), resp.DeclaredLoadBalancers...)
	r.desired.Store(&lbs)
	select {
	case r.trigger <- struct{}{}:
	default:
		// Earlier nudge already queued — collapse into one reconcile.
	}
}

// Run blocks until ctx is cancelled. Ticks every r.tick OR on trigger; each
// tick runs one reconcile pass. On ctx cancel it closes every bound
// listener — this reconciler owns sockets and must release them on
// shutdown rather than leak the bound ports.
func (r *PublishedListeners) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	// Initial pass in case the first heartbeat lands before this loop boots.
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			r.closeAll(ctx)
			return ctx.Err()
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.trigger:
			r.reconcile(ctx)
		}
	}
}

// reconcile is one pass over the (desired, bound) diff. It binds newly
// declared published ports, retries previously-failed binds, and closes
// listeners whose port the CP no longer declares.
//
// The nil-vs-empty guard is a POINTER check, not a length check: a nil
// desired pointer means "no heartbeat response received yet" (do nothing,
// fail toward inaction), while a non-nil pointer to an empty slice
// legitimately means "no published LBs" and correctly reaps every stale
// listener.
func (r *PublishedListeners) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	d := r.desired.Load()
	if d == nil {
		return
	}
	desired := *d

	desiredByPort := make(map[int32]heartbeat.DeclaredLoadBalancer, len(desired))
	for _, lb := range desired {
		desiredByPort[lb.PublishedPort] = lb
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Close listeners whose port is no longer declared.
	for port, bl := range r.bound {
		if _, ok := desiredByPort[port]; ok {
			continue
		}
		if bl.ln != nil {
			if err := bl.ln.Close(); err != nil {
				r.log.WarnContext(ctx, "published listener close failed",
					slog.Int("port", int(port)),
					slog.String("error", err.Error()),
				)
			}
		}
		delete(r.bound, port)
		r.log.InfoContext(ctx, "published listener closed (unpublished)",
			slog.Int("port", int(port)),
		)
	}

	// Bind newly declared ports; retry ports whose previous bind failed
	// (ln == nil); rebind a port whose owning LB changed so the listener
	// re-keys to the new owner. A live listener still owned by the same LB is
	// left untouched.
	for port, lb := range desiredByPort {
		if bl, ok := r.bound[port]; ok && bl.ln != nil {
			if bl.lbID == lb.LBID {
				// Same owner, listener already up: refresh the live datapath
				// config in place (no rebind). A heartbeat that changed the
				// backend set or source allowlist reaches the running accept
				// goroutine through the atomic pointer on the next connection.
				bl.cfg.Store(configFromLB(lb))
				continue
			}
			// Ownership changed without an intervening unpublished tick (the
			// old LB released the port and a new LB claimed it between two
			// heartbeats). Close the stale listener and fall through to rebind
			// under the new LB, so the observed report re-keys and, later, the
			// per-port datapath config follows the new owner rather than
			// serving the old owner's ACL/backends on this port.
			if err := bl.ln.Close(); err != nil {
				r.log.WarnContext(ctx, "published listener close failed (rebind on owner change)",
					slog.Int("port", int(port)),
					slog.String("error", err.Error()),
				)
			}
			delete(r.bound, port)
			r.log.InfoContext(ctx, "published listener rebinding (owner changed)",
				slog.Int("port", int(port)),
				slog.String("old_lb_id", bl.lbID.String()),
				slog.String("new_lb_id", lb.LBID.String()),
			)
		}
		ln, err := r.mgr.Listen(ctx, port)
		if err != nil {
			msg := err.Error()
			r.bound[port] = boundListener{lbID: lb.LBID, err: msg}
			r.log.WarnContext(ctx, "published listener bind failed",
				slog.Int("port", int(port)),
				slog.String("lb_id", lb.LBID.String()),
				slog.String("error", msg),
			)
			continue
		}
		cfg := &atomic.Pointer[listenerConfig]{}
		cfg.Store(configFromLB(lb))
		r.bound[port] = boundListener{ln: ln, lbID: lb.LBID, cfg: cfg}
		go r.accept(ctx, ln, cfg)
		r.log.InfoContext(ctx, "published listener bound",
			slog.Int("port", int(port)),
			slog.String("lb_id", lb.LBID.String()),
		)
	}
}

// accept runs one per-listener goroutine, dispatching each accepted connection
// to the raw datapath (handleConn). ctx is the reconciler's Run context and the
// atomic cfg pointer is the owning listener's live config, both PASSED in (not
// read off a shared struct field or the mutex-guarded bound map): cfg is Loaded
// per accepted connection so a heartbeat that refreshes the backend set or
// source allowlist reaches this loop without a rebind.
//
// It MUST return on the first Accept error. A closed listener makes Accept
// return an error on every call, so a continue-on-error loop would busy-spin
// a goroutine after Close. Returning on error lets the goroutine exit when
// reconcile (or closeAll) closes the listener.
func (r *PublishedListeners) accept(ctx context.Context, ln net.Listener, cfg *atomic.Pointer[listenerConfig]) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go r.handleConn(ctx, c, cfg.Load())
	}
}

// closeAll releases every bound listener. Called on Run shutdown so the
// process does not leak the public ports it bound.
func (r *PublishedListeners) closeAll(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for port, bl := range r.bound {
		if bl.ln != nil {
			if err := bl.ln.Close(); err != nil {
				r.log.WarnContext(ctx, "published listener close failed on shutdown",
					slog.Int("port", int(port)),
					slog.String("error", err.Error()),
				)
			}
		}
		delete(r.bound, port)
	}
}
