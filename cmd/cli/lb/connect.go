// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/internal/sshconn"
)

// defaultConnectListen is the local bind address when --listen is omitted: a
// loopback ephemeral port, so the forward is reachable only from the operator's
// host and never collides with an in-use port.
const defaultConnectListen = "127.0.0.1:0"

// dialLoadBalancer is the broker+connect seam. Production wires it to the shared
// connector; tests substitute a stub.
var dialLoadBalancer = sshconn.DialLoadBalancer

// newConnectCommand returns the `otherix lb connect` command. It opens a local
// listener and, for each connection, brokers a connection to one of the load
// balancer's backends through the control plane and splices bytes to it.
func newConnectCommand() *cobra.Command {
	var listen string

	cmd := &cobra.Command{
		Use:   "connect <name>",
		Short: "Forward a local port to a load balancer's backend through the control plane.",
		Long: `Forwards a local TCP port to a load balancer.

The command opens a local listener and, for each connection, brokers a
connection to one of the load balancer's backend VMs through the control
plane and splices bytes to it. Backend selection is per connection, so a
long-lived listener balances across the eligible backends. Overlay backends
are reached directly through a converged gateway (the control plane stays out
of the data path); bridge backends are reached through the control-plane
relay. No inbound network path to the backend is required.

The local listener defaults to a loopback ephemeral port; override the bind
address with --listen.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(cmd, args[0], listen)
		},
	}
	cmd.Flags().StringVarP(&listen, "listen", "L", defaultConnectListen,
		"local address to listen on (host:port)")
	return cmd
}

// runConnect resolves the cluster credential, opens the local listener, prints
// the bound address, and serves connections until the context is cancelled.
func runConnect(cmd *cobra.Command, lbName, listen string) error {
	cfg, err := sshConfigFromFlags(cmd)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %v", listen, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forwarding %s -> load balancer %s (press Ctrl-C to stop)\n",
		ln.Addr().String(), lbName)

	// Close the listener when the command context is cancelled (operator Ctrl-C)
	// so the accept loop returns.
	ctx := cmd.Context()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	return connectLoop(ctx, cfg, lbName, ln)
}

// connectLoop accepts connections on ln and splices each to a freshly brokered
// connection for one of the load balancer's backends. It returns nil when the
// listener is closed or the context is cancelled, and an error on an unexpected
// accept failure.
func connectLoop(ctx context.Context, cfg sshconn.Config, lbName string, ln net.Listener) error {
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("lb connect: accept: %v", err)
		}
		go handleConn(ctx, cfg, lbName, local)
	}
}

// handleConn brokers a connection for one accepted local connection and splices
// the two byte-for-byte. A broker or connect failure tears the local connection
// down (reported on stderr) without affecting the listener.
func handleConn(ctx context.Context, cfg sshconn.Config, lbName string, local net.Conn) {
	defer func() { _ = local.Close() }()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	remote, err := dialLoadBalancer(connCtx, cfg, lbName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lb connect: connect to %s: %v\n", lbName, err)
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
// sshconn.Config the connector consumes. It mirrors the resolution the rest of
// the CLI uses, so connect trusts exactly what the rest of the CLI trusts: the
// cluster CA bundle (ADR 0026), or the operator's insecure-skip-verify opt-out.
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
