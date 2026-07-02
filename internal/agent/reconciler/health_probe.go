// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/otherix/otherix/internal/agent/netfabric"
	avm "github.com/otherix/otherix/internal/agent/vm"
)

// healthByNameManager and healthIPByMAC mirror the narrow surfaces the ssh-pipe
// handler uses; *vm.Manager and the dhcp4 responder satisfy them.
type healthByNameManager interface {
	ByName(name string) (*avm.VM, error)
}

type healthIPByMAC interface {
	LookupByMAC(mac string) (netip.Addr, bool)
}

// tcpHealthProbe is the production HealthProbe: it resolves the guest IP and
// bridge from local NIC/lease state and TCP-dials the port bound to that bridge.
type tcpHealthProbe struct {
	mgr    healthByNameManager
	leases healthIPByMAC
	log    *slog.Logger
}

// NewHealthProbe builds the production HealthProbe: it resolves the guest IP and
// bridge from local NIC/lease state (never from the CP) and TCP-dials bound to
// that bridge - the same anti-SSRF datapath as the ssh pipe and ingress splice.
func NewHealthProbe(mgr healthByNameManager, leases healthIPByMAC, log *slog.Logger) HealthProbe {
	return &tcpHealthProbe{mgr: mgr, leases: leases, log: log}
}

// Probe resolves vmName to a locally-hosted, running VM and TCP-dials the first
// NIC whose MAC has a managed-DHCP lease, bound to that NIC's bridge. The result
// is tri-state: probed reports whether a real TCP dial was attempted, and only
// then does healthy carry its outcome. A missing VM, a non-running VM, or a NIC
// with no managed-DHCP lease returns (false, false) - "could not probe" - so a
// valid backend with no managed-DHCP lease (statically-addressed guest, dhcp=false
// network, or the transient lease-warmup window) is never darkened; it stays
// warming and reports no verdict. Only a completed DialContext yields probed=true.
// The resolution boundary mirrors the ssh pipe's resolveSSHTarget, so a caller can
// never steer the dial at an arbitrary address.
func (p *tcpHealthProbe) Probe(ctx context.Context, vmName string, port int32, timeout time.Duration) (healthy, probed bool) {
	v, err := p.mgr.ByName(vmName)
	if err != nil || v.Status != avm.StatusRunning {
		return false, false // not hosted here / not running -> no verdict, stays warming
	}
	for _, n := range v.NICs {
		ip, ok := p.leases.LookupByMAC(n.MAC)
		if !ok || !ip.IsValid() {
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		conn, derr := (&net.Dialer{Control: netfabric.BindToDeviceControl(n.Bridge)}).
			DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
		cancel()
		if derr != nil {
			p.log.DebugContext(ctx, "health probe dial failed",
				"vm_name", vmName, "port", port, "error", derr)
			return false, true // real dial attempted and failed -> definite verdict input
		}
		_ = conn.Close()
		return true, true
	}
	return false, false // no NIC with a managed-DHCP lease -> could not probe, stays warming
}
