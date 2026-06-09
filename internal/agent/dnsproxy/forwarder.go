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
	"strings"
	"sync"
	"time"
)

// defaultResolvPaths is the ordered list dnsproxy reads upstream resolvers
// from: systemd-resolved's real upstream file first, then the classic file.
var defaultResolvPaths = []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"}

// fallbackResolver is used when no non-loopback nameserver is discoverable, so
// a VM still resolves names even on a host with only a 127.0.0.53 stub.
const fallbackResolver = "1.1.1.1:53"

// Config parametrises a Forwarder.
type Config struct {
	// Listen is the UDP bind address, e.g. "169.254.1.1:53". Bound with
	// IP_FREEBIND so the forwarder starts before the reconciler assigns the
	// anycast address to any overlay bridge.
	Listen string
	// Upstreams is the ordered list of resolver host:port targets. Empty falls
	// back to upstreamResolvers() at New time.
	Upstreams []string
	Log       *slog.Logger
}

// Forwarder relays UDP DNS queries to an upstream resolver.
type Forwarder struct {
	listen    string
	upstreams []string
	log       *slog.Logger

	ready chan struct{}

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
	return &Forwarder{
		listen:    cfg.Listen,
		upstreams: ups,
		log:       cfg.Log,
		ready:     make(chan struct{}),
	}, nil
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
				return ctx.Err()
			}
			f.log.Warn("dns forwarder read error", "error", err.Error())
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go f.relay(conn, client, query)
	}
}

// relay forwards one query to the first responsive upstream and writes the
// response back to client. Best-effort: a failed upstream is logged and the
// query is dropped (the VM resolver retries).
func (f *Forwarder) relay(conn net.PacketConn, client net.Addr, query []byte) {
	for _, up := range f.upstreams {
		resp, err := exchangeUDP(up, query, 3*time.Second)
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
	n, err := c.Read(resp)
	if err != nil {
		return nil, err
	}
	return resp[:n], nil
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
