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

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/internal/sshconn"
)

// defaultListen is the local bind address when --listen is omitted: a loopback
// ephemeral port, so the forward is reachable only from the operator's host and
// never collides with an in-use port.
const defaultListen = "127.0.0.1:0"

// dialIngress is the broker+connect seam. Production wires it to the shared
// connector; tests substitute a stub.
var dialIngress = sshconn.DialIngress

// NewCommand returns the `otherix forward` command.
func NewCommand() *cobra.Command {
	var listen string

	cmd := &cobra.Command{
		Use:   "forward <vm-name> <port>",
		Short: "Forward a local port to a VM's guest port through the control plane.",
		Long: `Forwards a local TCP port to a port inside the named VM.

The command opens a local listener and, for each connection, brokers an
ingress path to the VM through the control plane and splices bytes to the
guest port. Overlay VMs are reached directly through a converged gateway
(the control plane stays out of the data path); bridge VMs are reached
through the control-plane relay. No inbound network path to the guest is
required - the broker rides the same control plane endpoint and credential
the rest of the CLI uses.

The local listener defaults to a loopback ephemeral port; override the bind
address with --listen.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("invalid port %q: must be an integer in 1..65535", args[1])
			}
			return runForward(cmd, args[0], port, listen)
		},
	}
	cmd.Flags().StringVarP(&listen, "listen", "L", defaultListen,
		"local address to listen on (host:port)")
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
