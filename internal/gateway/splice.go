// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/otherix/otherix/internal/api/response"
)

// connectDialTimeout bounds the dial to the target over the overlay. A target
// that is unreachable (still booting, wrong address) fails fast rather than
// holding the request open.
const connectDialTimeout = 10 * time.Second

// dialFunc is the dial seam the connect handler uses to reach the target over
// the overlay. Production wires it to a plain TCP dialer; tests substitute a
// dialer that reaches a loopback stub.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// connectHandler serves POST /v1/connect: it dials a target on the overlay and
// splices the inbound connection to it byte for byte. The route is mounted
// under the gateway's CP-identity group, so the only caller is the control
// plane; the target (guest_ip, port) it carries is trusted as supplied. The
// gateway never originates the target itself and applies no per-session
// credential check here - operator-authenticated session credentials and the
// re-resolve-from-lease anti-SSRF binding are a later concern; in this form the
// CP-identity boundary on the route is the whole gate.
type connectHandler struct {
	dial dialFunc
	log  *slog.Logger
}

// newConnectHandler builds a connect handler with a plain TCP dialer.
func newConnectHandler(log *slog.Logger) *connectHandler {
	return &connectHandler{
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		log: log,
	}
}

// Connect handles POST /v1/connect?guest_ip=<ip>&port=<port>. It validates the
// target, dials it over the overlay, hijacks the inbound connection, and
// splices the two legs until either side closes. The target is validated to be
// a literal IP and an in-range port before any dial, so the handler never
// resolves a hostname or dials a non-port address.
func (h *connectHandler) Connect(w http.ResponseWriter, r *http.Request) {
	target, err := parseConnectTarget(r)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "connection does not support splicing", nil)
		return
	}

	// Dial the target before hijacking so a dial failure is reported as a
	// normal HTTP error rather than a half-open hijacked socket.
	dialCtx, dialCancel := context.WithTimeout(r.Context(), connectDialTimeout)
	upstream, err := h.dial(dialCtx, "tcp", target)
	dialCancel()
	if err != nil {
		h.log.Warn("connect: dial target failed", "target", target, "error", err.Error())
		response.WriteError(w, r, http.StatusBadGateway,
			response.CodeAgentUnreachable, "could not reach the forward target", nil)
		return
	}

	conn, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		h.log.Error("connect: hijack failed", "target", target, "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	// Clear any read/write deadline the HTTP server armed for the request so the
	// long-lived spliced session is not torn at the listener's timeout.
	_ = conn.SetDeadline(time.Time{})

	// Signal the caller the pipe is open; everything after is raw spliced bytes.
	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		_ = upstream.Close()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spliceConns(ctx, cancel, conn, upstream)
	h.log.Info("connect: session closed", "target", target)
}

// parseConnectTarget extracts and validates the (guest_ip, port) target from
// the request query. guest_ip must be a literal IP address (never a hostname,
// so the gateway cannot be steered into a DNS resolution) and port must be in
// 1..65535. It returns the joined "ip:port" dial target.
func parseConnectTarget(r *http.Request) (string, error) {
	q := r.URL.Query()
	ipStr := q.Get("guest_ip")
	if ipStr == "" {
		return "", errMissing("guest_ip")
	}
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return "", errInvalid("guest_ip must be a literal IP address")
	}
	portStr := q.Get("port")
	if portStr == "" {
		return "", errMissing("port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", errInvalid("port must be an integer in 1..65535")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

// validationError is a small typed error so parseConnectTarget can describe the
// exact field that failed without coupling to the response layer.
type validationError struct{ msg string }

func (e validationError) Error() string { return e.msg }

func errMissing(field string) error { return validationError{msg: field + " is required"} }
func errInvalid(msg string) error   { return validationError{msg: msg} }

// spliceConns copies bytes both directions until either side closes or ctx is
// cancelled, then tears both legs down (the kill-implies-teardown invariant: no
// goroutine, fd, or slot survives any exit path).
func spliceConns(ctx context.Context, cancel context.CancelFunc, a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)

	select {
	case <-ctx.Done():
	case <-done:
	}
	cancel()
	_ = a.Close()
	_ = b.Close()
}
