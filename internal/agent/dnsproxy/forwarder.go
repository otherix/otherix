// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package dnsproxy is a minimal per-node UDP DNS forwarder. It listens on the
// overlay anycast gateway address and relays queries to the node's upstream
// resolver, so overlay VMs get a node-independent resolver that survives live
// migration (the same anycast address is answered locally on every node). It
// is a stateless byte passthrough: no cache, no policy. TCP fallback for
// truncated responses is future work.
package dnsproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultResolvPaths is the ordered list dnsproxy reads upstream resolvers
// from: systemd-resolved's real upstream file first, then the classic file.
var defaultResolvPaths = []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"}

// fallbackResolver is used when no non-loopback nameserver is discoverable, so
// a VM still resolves names even on a host with only a 127.0.0.53 stub.
const fallbackResolver = "1.1.1.1:53"

// defaultMaxInFlight caps concurrent relays (and thus upstream sockets) when
// Config.MaxInFlight is unset, bounding the blast radius of a guest flooding
// the anycast resolver.
const defaultMaxInFlight = 256

// defaultMaxPerSource caps concurrent relays from a single client IP, so one
// guest cannot consume the global in-flight pool and starve other tenants' DNS
// 16 of the 256 global slots is ~1/16 of the pool: a single
// abusive source holds at most that share, leaving the rest for other guests,
// while honest DNS (which resolves fast) rarely reaches 16 concurrent in-flight.
const defaultMaxPerSource = 16

// Config parametrises a Forwarder.
type Config struct {
	// Listen is the UDP bind address, e.g. "169.254.1.1:53". Bound with
	// IP_FREEBIND so the forwarder starts before the reconciler assigns the
	// anycast address to any overlay bridge.
	Listen string
	// Upstreams is the ordered list of resolver host:port targets. Empty falls
	// back to upstreamResolvers() at New time. Upstreams are resolved ONCE at
	// construction: a later resolv.conf change requires an agent restart to take
	// effect.
	Upstreams []string
	Log       *slog.Logger
	// MaxInFlight caps concurrent relays. Excess inbound queries are dropped
	// (DNS clients retry) rather than queued, so a flooding guest cannot spawn
	// unbounded goroutines/sockets. <= 0 applies defaultMaxInFlight.
	MaxInFlight int
	// MaxPerSource caps concurrent relays from a single client IP. Queries from a
	// source already at its cap are dropped (the client retries) even when the
	// global pool has room, so one guest cannot monopolise the resolver. <= 0
	// applies defaultMaxPerSource.
	MaxPerSource int
}

// Forwarder relays UDP DNS queries to an upstream resolver.
type Forwarder struct {
	listen    string
	upstreams []string
	log       *slog.Logger

	ready chan struct{}

	// sem bounds concurrent relays; a non-blocking send acquires a slot.
	sem chan struct{}
	// wg tracks in-flight relay goroutines so Run drains them before returning,
	// preventing a relay from outliving Run and racing the dnsExchange seam.
	wg sync.WaitGroup
	// dropped counts queries shed because sem was full.
	dropped atomic.Uint64

	// maxPerSource caps concurrent relays per client IP (the fairness layer over
	// the global sem). inflight tracks live relays per source under smu and
	// self-cleans at zero so it cannot grow unbounded under spoofed source IPs.
	maxPerSource  int
	smu           sync.Mutex
	inflight      map[string]int
	sourceDropped atomic.Uint64

	mu   sync.Mutex
	conn net.PacketConn
}

// New validates cfg and returns a Forwarder. Upstreams default to the node's
// discovered resolvers when cfg.Upstreams is empty.
func New(cfg Config) (*Forwarder, error) {
	if cfg.Listen == "" {
		return nil, errors.New("dnsproxy: Listen is required")
	}
	if cfg.Log == nil {
		return nil, errors.New("dnsproxy: Log is required")
	}
	ups := cfg.Upstreams
	if len(ups) == 0 {
		ups = upstreamResolvers()
	}
	max := cfg.MaxInFlight
	if max <= 0 {
		max = defaultMaxInFlight
	}
	perSource := cfg.MaxPerSource
	if perSource <= 0 {
		perSource = defaultMaxPerSource
	}
	return &Forwarder{
		listen:       cfg.Listen,
		upstreams:    ups,
		log:          cfg.Log,
		ready:        make(chan struct{}),
		sem:          make(chan struct{}, max),
		maxPerSource: perSource,
		inflight:     map[string]int{},
	}, nil
}

// Dropped reports the number of queries shed because the in-flight relay cap
// was full. Monotonic; for observability and tests.
func (f *Forwarder) Dropped() uint64 { return f.dropped.Load() }

// SourceDropped reports the number of queries shed because the per-source
// in-flight cap was reached. Monotonic; for observability and tests.
func (f *Forwarder) SourceDropped() uint64 { return f.sourceDropped.Load() }

// acquireSource reserves a per-source in-flight slot for ip, returning false
// when ip is already at MaxPerSource. Paired with releaseSource.
func (f *Forwarder) acquireSource(ip string) bool {
	f.smu.Lock()
	defer f.smu.Unlock()
	if f.inflight[ip] >= f.maxPerSource {
		return false
	}
	f.inflight[ip]++
	return true
}

// releaseSource frees a per-source slot for ip, deleting the entry at zero so
// the map only holds currently-active sources (self-cleaning, bounded even
// under spoofed source IPs).
func (f *Forwarder) releaseSource(ip string) {
	f.smu.Lock()
	defer f.smu.Unlock()
	if n := f.inflight[ip] - 1; n > 0 {
		f.inflight[ip] = n
	} else {
		delete(f.inflight, ip)
	}
}

// sourceIP extracts the client IP (the tenant identity; the UDP source port
// varies per query).
func sourceIP(a net.Addr) string {
	if ua, ok := a.(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// Ready is closed once the listener is bound, so callers (and tests) can wait
// for serving to begin.
func (f *Forwarder) Ready() <-chan struct{} { return f.ready }

// LocalAddr returns the bound listen address (valid after Ready is closed).
func (f *Forwarder) LocalAddr() net.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == nil {
		return nil
	}
	return f.conn.LocalAddr()
}

// Run binds the UDP listener (IP_FREEBIND) and serves until ctx is cancelled.
func (f *Forwarder) Run(ctx context.Context) error {
	lc := net.ListenConfig{Control: freebindControl}
	conn, err := lc.ListenPacket(ctx, "udp4", f.listen)
	if err != nil {
		return fmt.Errorf("dnsproxy: listen %s: %w", f.listen, err)
	}
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	close(f.ready)

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	f.log.Info("dns forwarder serving", "listen", conn.LocalAddr().String(), "upstreams", strings.Join(f.upstreams, ","))

	buf := make([]byte, 1500)
	for {
		n, client, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				f.wg.Wait()
				return ctx.Err()
			}
			f.log.Warn("dns forwarder read error", "error", err.Error())
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		ip := sourceIP(client)
		if !f.acquireSource(ip) {
			// This source already holds MaxPerSource relays: drop (the client
			// retries) so one guest cannot starve the global pool. Accounted
			// separately from the global capacity drop.
			if d := f.sourceDropped.Add(1); d == 1 || d%1000 == 0 {
				f.log.Warn("dns forwarder dropping queries: per-source cap",
					"source", ip, "dropped_total", d, "max_per_source", f.maxPerSource)
			}
			continue
		}
		select {
		case f.sem <- struct{}{}:
			f.wg.Add(1)
			go func() {
				defer func() { f.wg.Done(); <-f.sem; f.releaseSource(ip) }()
				f.relay(conn, client, query)
			}()
		default:
			// Global pool full: release the per-source slot we just took and drop
			// this query rather than spawn an unbounded goroutine/socket. DNS
			// clients retry; this caps the blast radius of a flood.
			f.releaseSource(ip)
			if d := f.dropped.Add(1); d == 1 || d%1000 == 0 {
				f.log.Warn("dns forwarder dropping queries at capacity", "dropped_total", d, "max_in_flight", cap(f.sem))
			}
		}
	}
}

// dnsExchange forwards one query to a single upstream. It is a package-level
// seam over exchangeUDP (the accepted mutable-global exception) so tests can
// force an exchange panic and prove relay's recover keeps the forwarder alive;
// production code never reassigns it.
var dnsExchange = exchangeUDP

// relay forwards one query to the first responsive upstream and writes the
// response back to client. Best-effort: a failed upstream is logged and the
// query is dropped (the VM resolver retries). It recovers from panics (query
// and the upstream response are untrusted network bytes): a panicking query is
// logged and dropped rather than killing the agent process.
func (f *Forwarder) relay(conn net.PacketConn, client net.Addr, query []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			f.log.Error("dns relay panicked, dropping query", "panic", fmt.Sprintf("%v", rec), "stack", string(debug.Stack()))
		}
	}()
	for _, up := range f.upstreams {
		resp, err := dnsExchange(up, query, 3*time.Second)
		if err != nil {
			f.log.Warn("dns upstream exchange failed", "upstream", up, "error", err.Error())
			continue
		}
		if _, err := conn.WriteTo(resp, client); err != nil {
			f.log.Warn("dns forwarder write back failed", "error", err.Error())
		}
		return
	}
}

// exchangeUDP sends query to a single upstream over UDP and returns the reply.
func exchangeUDP(upstream string, query []byte, timeout time.Duration) ([]byte, error) {
	c, err := net.DialTimeout("udp4", upstream, timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	resp := make([]byte, 1500)
	for {
		n, err := c.Read(resp)
		if err != nil {
			return nil, err
		}
		// Validate the DNS transaction id (first 2 bytes) matches the query.
		// The connected socket already filters by upstream addr; the txid check
		// rejects an off-path forged reply racing the real one.
		if n >= 2 && len(query) >= 2 && resp[0] == query[0] && resp[1] == query[1] {
			return resp[:n], nil
		}
		// Mismatched txid: keep reading until the deadline (the read deadline set
		// above bounds this loop; a timeout surfaces as an error from c.Read).
	}
}

// upstreamResolvers discovers the node's upstream resolvers from the default
// resolv.conf paths, falling back to a public resolver.
func upstreamResolvers() []string { return upstreamResolversFrom(defaultResolvPaths) }

// upstreamResolversFrom reads the first readable path in paths, returning every
// non-loopback nameserver as host:53. Falls back to fallbackResolver when none
// is found. Mirrors the dev write_netns_resolv bash logic.
func upstreamResolversFrom(paths []string) []string {
	for _, p := range paths {
		// p comes from defaultResolvPaths or a test temp file, never untrusted
		// network input, so opening it by name is safe.
		fh, err := os.Open(p) // #nosec G304
		if err != nil {
			continue
		}
		var out []string
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}
			ip := net.ParseIP(fields[1])
			if ip == nil || ip.IsLoopback() {
				continue
			}
			out = append(out, net.JoinHostPort(fields[1], "53"))
		}
		_ = fh.Close()
		if len(out) > 0 {
			return out
		}
	}
	return []string{fallbackResolver}
}
