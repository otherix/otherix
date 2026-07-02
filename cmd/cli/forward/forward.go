// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package forward implements the operator-facing `otherix forward` command.
//
// `otherix forward <vm> <port>` opens a local TCP listener and forwards every
// accepted connection to the named VM's guest port through the control plane's
// ingress broker. For each connection it brokers an ingress path (POST
// /v1/vms/{vm}/ingress) and splices bytes over the transport the control plane
// selects: a direct connection to a converged gateway for overlay VMs (the
// control plane stays out of the data path) or the control-plane relay for
// bridge VMs. The connector, broker call, TLS trust, and splice all live in
// cmd/internal/sshconn; this package is the thin operator-UX layer.
//
// Each accepted connection is brokered independently. The session credential a
// gateway broker mints is single-use and short-lived, so re-brokering per
// connection keeps a long-lived listener working past one credential's expiry
// without caching or refresh bookkeeping.
package forward

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/internal/sshconn"
)

// defaultAddress is the bind host when --address is omitted: loopback, so the
// forward is reachable only from the operator's own host.
const defaultAddress = "127.0.0.1"

// defaultListen is the local bind address the -L/--listen shortcut falls back to
// when the operator does not set it explicitly. It is only consulted when the
// flag was changed; the primary path composes the bind address from --address
// and the port spec instead.
const defaultListen = "127.0.0.1:0"

// forwardTarget is the resolved local listener address and guest port a forward
// serves.
type forwardTarget struct {
	listenAddr string
	guestPort  int
}

// resolveForwardTarget maps the kubectl-style port spec and bind flags to the
// concrete listener address and guest port. The bind host comes from --address
// (default loopback); the [LOCAL:]REMOTE positional supplies the local and guest
// ports (a bare REMOTE binds the same local port, kubectl-style; an empty LOCAL
// requests an ephemeral port). The -L/--listen shortcut is kept as an
// alternative that sets host:port in one value; it is mutually exclusive with
// --address and with an explicit local port in the spec.
func resolveForwardTarget(portSpec, address string, addressSet bool, listen string, listenSet bool) (forwardTarget, error) {
	localPort, guestPort, localSet, err := parsePortSpec(portSpec)
	if err != nil {
		return forwardTarget{}, err
	}
	if listenSet {
		if addressSet {
			return forwardTarget{}, fmt.Errorf("use either --address or --listen to set the bind address, not both")
		}
		if localSet {
			return forwardTarget{}, fmt.Errorf("set the local port with either --listen or the port spec, not both")
		}
		return forwardTarget{listenAddr: listen, guestPort: guestPort}, nil
	}
	return forwardTarget{
		listenAddr: net.JoinHostPort(address, strconv.Itoa(localPort)),
		guestPort:  guestPort,
	}, nil
}

// parsePortSpec parses a kubectl-style [LOCAL:]REMOTE port spec. localSet reports
// whether the spec carried an explicit local part (a leading "LOCAL:" or ":");
// for a bare REMOTE the returned localPort equals guestPort. An empty local part
// yields localPort 0 (ephemeral).
func parsePortSpec(spec string) (localPort, guestPort int, localSet bool, err error) {
	if spec == "" {
		return 0, 0, false, fmt.Errorf("missing port spec: expected [LOCAL:]REMOTE")
	}
	local, remote, hasColon := strings.Cut(spec, ":")
	if hasColon {
		if strings.Contains(remote, ":") {
			return 0, 0, false, fmt.Errorf("invalid port spec %q: expected [LOCAL:]REMOTE; set the bind address with --address, not in the port spec", spec)
		}
		guestPort, err = parsePort(remote, "remote port")
		if err != nil {
			return 0, 0, false, err
		}
		if local == "" {
			return 0, guestPort, true, nil
		}
		localPort, err = parsePort(local, "local port")
		if err != nil {
			return 0, 0, false, err
		}
		return localPort, guestPort, true, nil
	}
	guestPort, err = parsePort(spec, "remote port")
	if err != nil {
		return 0, 0, false, err
	}
	return guestPort, guestPort, false, nil
}

// parsePort parses a decimal port in 1..65535. label names the field for error
// messages ("remote port" / "local port").
func parsePort(s, label string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid %s %q: must be an integer in 1..65535", label, s)
	}
	return p, nil
}

// dialIngress is the broker+connect seam. Production wires it to the shared
// connector; tests substitute a stub.
var dialIngress = sshconn.DialIngress

// NewCommand returns the `otherix forward` command.
func NewCommand() *cobra.Command {
	var listen, address string

	cmd := &cobra.Command{
		Use:   "forward <vm-name> [LOCAL_PORT:]REMOTE_PORT",
		Short: "Forward a local port to a VM's guest port through the control plane.",
		Long: `Forwards a local TCP port to a port inside the named VM.

The command opens a local listener and, for each connection, brokers an
ingress path to the VM through the control plane and splices bytes to the
guest port. Overlay VMs are reached directly through a converged gateway
(the control plane stays out of the data path); bridge VMs are reached
through the control-plane relay. No inbound network path to the guest is
required - the broker rides the same control plane endpoint and credential
the rest of the CLI uses.

The port spec follows kubectl port-forward: "REMOTE_PORT" binds the same
local port; "LOCAL_PORT:REMOTE_PORT" pins a specific local port (useful when
the default is already in use); ":REMOTE_PORT" picks an ephemeral local port.
The listener binds loopback by default; set --address to bind another host
(e.g. 0.0.0.0). The -L/--listen host:port shortcut remains as an alternative
to --address plus a local port.`,
		Example: `  # local 5432 -> db01:5432
  otherix forward db01 5432

  # override a busy local port
  otherix forward db01 15432:5432

  # pick an ephemeral local port
  otherix forward db01 :5432

  # bind all interfaces
  otherix forward db01 15432:5432 --address 0.0.0.0`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			tgt, err := resolveForwardTarget(
				args[1],
				address, cmd.Flags().Changed("address"),
				listen, cmd.Flags().Changed("listen"),
			)
			if err != nil {
				return err
			}
			return runForward(cmd, args[0], tgt.guestPort, tgt.listenAddr)
		},
	}
	cmd.Flags().StringVar(&address, "address", defaultAddress,
		"bind address for the local listener (host only, e.g. 0.0.0.0)")
	cmd.Flags().StringVarP(&listen, "listen", "L", defaultListen,
		"local listen address host:port; alternative to --address plus a local port in the spec")
	return cmd
}

// runForward resolves the cluster credential, opens the local listener, prints
// the bound address, and serves connections until the context is cancelled.
func runForward(cmd *cobra.Command, vmName string, port int, listen string) error {
	cfg, err := sshConfigFromFlags(cmd)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %v", listen, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forwarding %s -> %s:%d (press Ctrl-C to stop)\n",
		ln.Addr().String(), vmName, port)

	// Close the listener when the command context is cancelled (operator Ctrl-C)
	// so the accept loop returns.
	ctx := cmd.Context()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	return forwardLoop(ctx, cfg, vmName, port, ln)
}

// forwardLoop accepts connections on ln and splices each to a freshly brokered
// ingress connection for vmName's guest port. It returns nil when the listener
// is closed or the context is cancelled, and an error on an unexpected accept
// failure.
func forwardLoop(ctx context.Context, cfg sshconn.Config, vmName string, port int, ln net.Listener) error {
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("forward: accept: %v", err)
		}
		go handleConn(ctx, cfg, vmName, port, local)
	}
}

// handleConn brokers an ingress connection for one accepted local connection and
// splices the two byte-for-byte. A broker or connect failure tears the local
// connection down (reported on stderr) without affecting the listener.
func handleConn(ctx context.Context, cfg sshconn.Config, vmName string, port int, local net.Conn) {
	defer func() { _ = local.Close() }()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	remote, err := dialIngress(connCtx, cfg, vmName, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forward: connect to %s:%d: %v\n", vmName, port, err)
		return
	}
	defer func() { _ = remote.Close() }()

	splice(local, remote)
}

// splice copies bytes both directions between a and b until either side closes.
// When a copy finishes it half-closes the destination's write side (clean EOF)
// where supported, otherwise closes it outright, so the peer's copy unwinds; both
// conns are then fully closed.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

// sshConfigFromFlags resolves the active (endpoint, token, TLS-trust) credential
// from the inherited persistent flags + env + config file and builds the
// sshconn.Config the connector consumes. It mirrors the resolution `otherix ssh`
// uses, so forward trusts exactly what the rest of the CLI trusts: the cluster CA
// bundle (ADR 0026), or the operator's insecure-skip-verify opt-out.
func sshConfigFromFlags(cmd *cobra.Command) (sshconn.Config, error) {
	auth, err := cliauth.ResolveAuth(cmd)
	if err != nil {
		return sshconn.Config{}, err
	}
	var caPEM []byte
	if auth.CACertData != "" {
		decoded, derr := base64.StdEncoding.DecodeString(auth.CACertData)
		if derr != nil {
			return sshconn.Config{}, fmt.Errorf("decode certificate-authority-data for cluster %q: %v", auth.ClusterName, derr)
		}
		caPEM = decoded
	}
	return sshconn.Config{
		ServerURL:             auth.Endpoint,
		BearerToken:           auth.Token,
		CACertPEM:             caPEM,
		InsecureSkipTLSVerify: auth.InsecureSkipTLSVerify,
	}, nil
}
