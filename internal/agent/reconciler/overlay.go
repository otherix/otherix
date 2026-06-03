// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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
// Once the VTEP is up, reconcileFDB drives its kernel FDB to exactly the declared
// set for this VNI (the VTEP comes up with learning off, controller-authoritative).
func (r *Networks) applyOverlay(ctx context.Context, d heartbeat.DeclaredNetwork, selfOverlayIP string, fdb []heartbeat.DeclaredFDBEntry) heartbeat.NetworkReport {
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
	r.reconcileFDB(ctx, vniVal, fdb)
	r.applied[d.ID] = appliedNetwork{BridgeName: d.BridgeName, Managed: true, Overlay: true, VNI: vniVal}
	return ready(d.ID)
}

// reconcileFDB drives the otvx<vni> kernel FDB to exactly the declared set for
// this VNI: list-diff-apply (append missing, delete stale). Best-effort - each
// failure is logged and the rest proceed; the overlay stays ready (the VTEP is
// up) and the FDB reconverges next tick. An unparseable entry is skipped + logged
// so one bad entry never drops the rest.
func (r *Networks) reconcileFDB(ctx context.Context, vni uint32, fdb []heartbeat.DeclaredFDBEntry) {
	desired := r.parseDesiredFDB(ctx, vni, fdb)

	current, err := r.fabric.FDBList(vni)
	if err != nil {
		r.log.WarnContext(ctx, "overlay fdb list failed; skipping fdb reconcile this pass",
			slog.Int("vni", int(vni)), slog.String("error", err.Error()))
		return
	}
	currentSet := make(map[string]netfabric.FDBEntry, len(current))
	for _, e := range current {
		currentSet[fdbKey(e)] = e
	}

	for k, e := range desired {
		if _, ok := currentSet[k]; ok {
			continue
		}
		if err := r.fabric.FDBAppend(vni, e); err != nil {
			r.log.WarnContext(ctx, "overlay fdb append failed",
				slog.Int("vni", int(vni)), slog.String("mac", e.MAC.String()),
				slog.String("dst", e.Dst.String()), slog.String("error", err.Error()))
		}
	}
	for k, e := range currentSet {
		if _, ok := desired[k]; ok {
			continue
		}
		if err := r.fabric.FDBDelete(vni, e); err != nil {
			r.log.WarnContext(ctx, "overlay fdb delete (prune) failed",
				slog.Int("vni", int(vni)), slog.String("mac", e.MAC.String()),
				slog.String("dst", e.Dst.String()), slog.String("error", err.Error()))
		}
	}
}

// parseDesiredFDB turns the declared entries for this VNI into the set of
// well-formed netfabric.FDBEntry values keyed by fdbKey. Entries for other VNIs
// are ignored; entries with an unparseable MAC or VTEP IP are skipped and logged
// so one bad entry never drops the rest.
func (r *Networks) parseDesiredFDB(ctx context.Context, vni uint32, fdb []heartbeat.DeclaredFDBEntry) map[string]netfabric.FDBEntry {
	desired := make(map[string]netfabric.FDBEntry)
	for _, e := range fdb {
		if e.VNI != int32(vni) { //nolint:gosec // vni is a 24-bit VXLAN id (<= 16777215); fits int32
			continue
		}
		mac, err := net.ParseMAC(e.MAC)
		if err != nil {
			r.log.WarnContext(ctx, "overlay fdb entry has unparseable mac; skipping",
				slog.Int("vni", int(vni)), slog.String("mac", e.MAC), slog.String("error", err.Error()))
			continue
		}
		dst, err := netip.ParseAddr(e.VtepIP)
		if err != nil {
			r.log.WarnContext(ctx, "overlay fdb entry has unparseable vtep_ip; skipping",
				slog.Int("vni", int(vni)), slog.String("vtep_ip", e.VtepIP), slog.String("error", err.Error()))
			continue
		}
		ent := netfabric.FDBEntry{MAC: mac, Dst: dst}
		desired[fdbKey(ent)] = ent
	}
	return desired
}

// fdbKey is the (mac, dst) identity of an FDB entry for set comparison.
func fdbKey(e netfabric.FDBEntry) string { return e.MAC.String() + "|" + e.Dst.String() }

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
