// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// vxlanUDPPort is the IANA VXLAN destination port every overlay VTEP uses.
const vxlanUDPPort = 4789

// applyOverlay materialises one type=overlay network fail-closed. It brings up
// the otb<vni> bridge and the otvx<vni> VXLAN VTEP bound to this node's otwg0
// overlay IP, enslaving the VTEP into the bridge. It is a no-op (pending) until
// otwg0 is up AND carries exactly the heartbeat-declared self_overlay_ip - the
// identity-check gate that stops a VTEP from binding the wrong source address.
// Per-MAC FDB programming is a later slice; this VTEP comes up with learning off
// and an empty FDB.
func (r *Networks) applyOverlay(ctx context.Context, d heartbeat.DeclaredNetwork, selfOverlayIP string) heartbeat.NetworkReport {
	if d.VNI == nil || *d.VNI <= 0 {
		return r.failed(ctx, d, "overlay network missing a valid vni")
	}
	if selfOverlayIP == "" {
		return r.pending(ctx, d, "overlay IP not yet assigned by the control plane")
	}
	prefix, err := netip.ParsePrefix(selfOverlayIP)
	if err != nil {
		return r.failed(ctx, d, fmt.Sprintf("self_overlay_ip %q not a prefix: %v", selfOverlayIP, err))
	}
	wantAddr := prefix.Addr()

	st, err := r.fabric.LinkState(wgInterfaceName)
	if err != nil {
		return r.failed(ctx, d, fmt.Sprintf("read %s state: %v", wgInterfaceName, err))
	}
	if !st.Up {
		return r.pending(ctx, d, fmt.Sprintf("%s not up", wgInterfaceName))
	}
	if !carries(st.Addrs, wantAddr) {
		return r.pending(ctx, d, fmt.Sprintf("%s missing overlay IP %s", wgInterfaceName, wantAddr))
	}

	// Bridge first: it is the enslave target for the VTEP.
	if err := r.fabric.EnsureBridge(d.BridgeName, int(d.Mtu)); err != nil {
		return r.failed(ctx, d, err.Error())
	}
	vniVal := uint32(*d.VNI) //nolint:gosec // *d.VNI is guarded > 0 above; VNI range <= 16777215 fits uint32
	if err := r.fabric.EnsureVXLAN(netfabric.VXLANConfig{
		VNI:    vniVal,
		Local:  wantAddr,
		Port:   vxlanUDPPort,
		MTU:    int(d.Mtu),
		Master: d.BridgeName,
	}); err != nil {
		return r.failed(ctx, d, err.Error())
	}
	r.applied[d.ID] = appliedNetwork{BridgeName: d.BridgeName, Managed: true, Overlay: true, VNI: vniVal}
	return ready(d.ID)
}

// carries reports whether any prefix in addrs has the host address want. The
// overlay gate matches the otwg0 link address against the heartbeat-declared
// self_overlay_ip host (the supernet prefix length on the link is irrelevant).
func carries(addrs []netip.Prefix, want netip.Addr) bool {
	for _, p := range addrs {
		if p.Addr() == want {
			return true
		}
	}
	return false
}
