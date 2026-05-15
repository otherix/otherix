// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
)

// QMPClient wraps a digitalocean/go-qemu/qmp SocketMonitor over the
// per-VM QMP unix socket. It exposes the command set the agent
// currently uses (query-status, system_powerdown, quit, stop, cont,
// system_reset); future iterations layer migrate on top.
//
// Connect() handles the qmp_capabilities handshake automatically, so
// callers receive a fully negotiated client.
type QMPClient struct {
	monitor *qmp.SocketMonitor
}

// DialQMP opens socketPath, runs the qmp_capabilities handshake, and
// returns a connected client. The dialTimeout bounds the unix-socket
// connect attempt; the qmp_capabilities exchange itself is bounded by
// the same deadline.
func DialQMP(socketPath string, dialTimeout time.Duration) (*QMPClient, error) {
	mon, err := qmp.NewSocketMonitor("unix", socketPath, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("open qmp socket %s: %w", socketPath, err)
	}
	if err := mon.Connect(); err != nil {
		return nil, fmt.Errorf("qmp connect: %w", err)
	}
	return &QMPClient{monitor: mon}, nil
}

// Close releases the underlying socket. Safe to call multiple times.
func (c *QMPClient) Close() error {
	if c == nil || c.monitor == nil {
		return nil
	}
	return c.monitor.Disconnect()
}

// QueryStatus issues query-status and returns the run-state string
// reported by qemu (e.g. "running", "paused", "shutdown").
func (c *QMPClient) QueryStatus() (string, error) {
	raw, err := c.monitor.Run([]byte(`{"execute":"query-status"}`))
	if err != nil {
		return "", fmt.Errorf("query-status: %w", err)
	}
	var resp struct {
		Return struct {
			Status string `json:"status"`
		} `json:"return"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("query-status decode: %w", err)
	}
	return resp.Return.Status, nil
}

// SystemPowerdown sends an ACPI shutdown signal to the guest. The guest
// chooses whether to honour it; the agent must poll for actual exit and
// fall back to Quit if the timeout elapses.
func (c *QMPClient) SystemPowerdown() error {
	if _, err := c.monitor.Run([]byte(`{"execute":"system_powerdown"}`)); err != nil {
		return fmt.Errorf("system_powerdown: %w", err)
	}
	return nil
}

// Quit force-exits the qemu process. Used when system_powerdown does not
// land within the agent's grace window.
func (c *QMPClient) Quit() error {
	if _, err := c.monitor.Run([]byte(`{"execute":"quit"}`)); err != nil {
		return fmt.Errorf("quit: %w", err)
	}
	return nil
}

// Stop issues QMP `stop` — freezes the guest vCPUs while keeping the
// VM resident in memory. Drives the pause endpoint. The QMP command
// returns immediately after dispatching; the guest run-state
// transitions to `paused` and a subsequent QueryStatus surfaces it.
func (c *QMPClient) Stop() error {
	if _, err := c.monitor.Run([]byte(`{"execute":"stop"}`)); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// Cont issues QMP `cont` — resumes the guest vCPUs after a prior
// `stop`. Drives the resume endpoint. Idempotent on the QMP side
// for an already-running guest, but the agent gates the call on
// observed phase to keep the wire shape honest.
func (c *QMPClient) Cont() error {
	if _, err := c.monitor.Run([]byte(`{"execute":"cont"}`)); err != nil {
		return fmt.Errorf("cont: %w", err)
	}
	return nil
}

// SystemReset issues QMP `system_reset` — equivalent to pressing
// the physical reset button: the QEMU process keeps running while
// the guest CPU is reset, so the guest OS reboots without notifying
// userspace. Drives the reset endpoint. QMP returns immediately
// after dispatching; the agent reports the post-call run-state to
// the caller for the synchronous 200 response.
func (c *QMPClient) SystemReset() error {
	if _, err := c.monitor.Run([]byte(`{"execute":"system_reset"}`)); err != nil {
		return fmt.Errorf("system_reset: %w", err)
	}
	return nil
}
