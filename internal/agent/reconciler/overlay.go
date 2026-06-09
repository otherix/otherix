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
	if *d.VNI > 0xFFFFFF {
		return r.failed(ctx, d, fmt.Sprintf("overlay vni %d exceeds the 24-bit VXLAN ceiling", *d.VNI))
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
	// Record the bridge in r.applied the instant it exists on the host, before
	// the VTEP step that may still fail. Without this an EnsureBridge-ok-but-
	// EnsureVXLAN-failed overlay is absent from r.applied, so a later CP-side
	// delete (while EnsureVXLAN is still failing) makes removeUndeclared skip it
	// and the otb<vni> bridge orphans with no GC (r.applied is in-process only).
	// Recording the VNI now is safe: teardownManaged's RemoveVXLAN is idempotent
	// (nil on an absent VTEP), so it no-ops while the bridge is still removed.
	r.applied[d.ID] = appliedNetwork{BridgeName: d.BridgeName, Managed: true, Overlay: true, VNI: vniVal}
	if err := r.fabric.EnsureVXLAN(netfabric.VXLANConfig{
		VNI:    vniVal,
		Local:  wantAddr,
		Port:   vxlanUDPPort,
		MTU:    int(d.Mtu),
		Master: d.BridgeName,
	}); err != nil {
		return r.failed(ctx, d, err.Error())
	}
	converged, unparseable := r.reconcileFDB(ctx, vniVal, fdb)
	// Unparseable declared entries take precedence in the reason: they are a
	// distinct, stable divergence (a corrupt declared entry can never be
	// programmed), unlike fdb_not_converged which covers transient apply failures.
	if unparseable > 0 {
		return r.pending(ctx, d, fmt.Sprintf("fdb_unparseable_entries=%d", unparseable))
	}
	if !converged {
		return r.pending(ctx, d, "fdb_not_converged")
	}

	if d.Egress == "nat" {
		if rep, ok := r.applyOverlayEgress(ctx, d, vniVal); !ok {
			return rep
		}
	}

	return ready(d.ID)
}

// applyOverlayEgress installs the egress=nat datapath for a converged overlay:
// IPv4 forwarding, the link-local anycast gateway on the bridge, and the
// interface-keyed SNAT for traffic entering via the bridge. It runs only after
// the overlay's FDB has converged, so egress is never programmed over a
// half-built overlay. On success it RE-records the network with HasEgress=true
// so a later teardown reclaims the masquerade, and returns ok=true. On any
// fabric error it returns a failed report and ok=false so the caller surfaces it.
func (r *Networks) applyOverlayEgress(ctx context.Context, d heartbeat.DeclaredNetwork, vniVal uint32) (heartbeat.NetworkReport, bool) {
	if err := r.fabric.EnableIPForwarding(); err != nil {
		return r.failed(ctx, d, fmt.Sprintf("enable ip_forward: %v", err)), false
	}
	if err := r.fabric.EnsureAnycastGateway(d.BridgeName, netfabric.OverlayGatewayAddr, netfabric.GatewayMAC(vniVal)); err != nil {
		return r.failed(ctx, d, fmt.Sprintf("anycast gateway: %v", err)), false
	}
	// Empty egress iface -> netfabric resolves the host default route.
	if err := r.fabric.EnsureMasqueradeIface(d.BridgeName, ""); err != nil {
		return r.failed(ctx, d, fmt.Sprintf("egress masquerade: %v", err)), false
	}
	r.applied[d.ID] = appliedNetwork{BridgeName: d.BridgeName, Managed: true, Overlay: true, VNI: vniVal, HasEgress: true}
	return heartbeat.NetworkReport{}, true
}

// reconcileFDB drives the otvx<vni> kernel FDB to exactly the declared set for
// this VNI: list-diff-apply (prune stale first, then append missing). It reports whether
// the FDB fully converged this pass and how many declared entries it could not
// parse. converged is false when the FDB could not be listed or when any
// append/delete failed; unparseable counts declared entries dropped for a bad MAC
// or VTEP IP. Either signal holds the overlay at pending so the caller never
// reports a green status over a half-programmed or blackholed FDB. Per-entry
// failures are logged and the parseable entries still proceed; the VTEP stays up
// and the FDB reconverges next tick, so one bad entry never drops the rest.
func (r *Networks) reconcileFDB(ctx context.Context, vni uint32, fdb []heartbeat.DeclaredFDBEntry) (converged bool, unparseable int) {
	desired, unparseable := r.parseDesiredFDB(ctx, vni, fdb)

	current, err := r.fabric.FDBList(vni)
	if err != nil {
		r.log.WarnContext(ctx, "overlay fdb list failed; skipping fdb reconcile this pass",
			slog.Int("vni", int(vni)), slog.String("error", err.Error()))
		return false, unparseable
	}
	currentSet := make(map[string]netfabric.FDBEntry, len(current))
	for _, e := range current {
		currentSet[fdbKey(e)] = e
	}

	converged = true
	// failedPruneMACs collects the MAC of every stale entry whose prune-delete
	// failed this pass, so the append loop below can skip programming a new dst
	// for that same MAC (see the dual-homing rationale there).
	failedPruneMACs := make(map[string]struct{})
	// Prune BEFORE appending. When a MAC moves VTEPs (re-placement) the (mac,dst)
	// set-diff is a delete-old + add-new; pruning first guarantees the kernel FDB
	// never transiently holds both (mac,vtepA) and (mac,vtepB), which under VXLAN
	// nolearning would replicate the unicast to both dsts (duplicate delivery +
	// blackhole on the old node). The worst case of delete-first is a brief
	// single-pass window where the entry is absent (a recoverable drop) - strictly
	// less harmful than dual-dst delivery, and it reconverges in this same pass.
	for k, e := range currentSet {
		if _, ok := desired[k]; ok {
			continue
		}
		if err := r.fabric.FDBDelete(vni, e); err != nil {
			converged = false
			failedPruneMACs[e.MAC.String()] = struct{}{}
			r.log.WarnContext(ctx, "overlay fdb delete (prune) failed",
				slog.Int("vni", int(vni)), slog.String("mac", e.MAC.String()),
				slog.String("dst", e.Dst.String()), slog.String("error", err.Error()))
		}
	}
	for k, e := range desired {
		if _, ok := currentSet[k]; ok {
			continue
		}
		// If a stale entry for this MAC could not be pruned this pass, skip its
		// append: under nolearning, programming the new dst while the old one
		// lingers dual-homes the MAC (duplicate delivery + wrong-node flood). A
		// one-pass reachability delay is the lesser, recoverable harm; the next
		// pass retries the delete first. converged is already false, holding the
		// overlay pending.
		//
		// Keyed on MAC, not mac+dst: for the all-zeros flood MAC (many dsts, one
		// per flood target) one failed prune broadens the skip to every flood
		// append this pass. That is intentional - it still fails toward inaction
		// and reconverges next pass; do NOT narrow this to fdbKey or the move
		// dual-homing window reopens.
		if _, blocked := failedPruneMACs[e.MAC.String()]; blocked {
			continue
		}
		if err := r.fabric.FDBAppend(vni, e); err != nil {
			converged = false
			r.log.WarnContext(ctx, "overlay fdb append failed",
				slog.Int("vni", int(vni)), slog.String("mac", e.MAC.String()),
				slog.String("dst", e.Dst.String()), slog.String("error", err.Error()))
		}
	}
	return converged, unparseable
}

// parseDesiredFDB turns the declared entries for this VNI into the set of
// well-formed netfabric.FDBEntry values keyed by fdbKey, plus the number of
// entries it had to skip. Entries for other VNIs are ignored (not counted);
// entries with an unparseable MAC or VTEP IP are skipped, logged, and counted as
// unparseable so the caller can hold the overlay at pending - a corrupt declared
// entry can never be programmed, which is a real desired-vs-observed divergence,
// not a no-op. The parseable entries are still returned so one bad entry never
// drops the rest.
func (r *Networks) parseDesiredFDB(ctx context.Context, vni uint32, fdb []heartbeat.DeclaredFDBEntry) (desired map[string]netfabric.FDBEntry, unparseable int) {
	desired = make(map[string]netfabric.FDBEntry)
	for _, e := range fdb {
		if e.VNI != int32(vni) { //nolint:gosec // vni is a 24-bit VXLAN id (<= 16777215); fits int32
			continue
		}
		mac, err := net.ParseMAC(e.MAC)
		if err != nil {
			unparseable++
			r.log.WarnContext(ctx, "overlay fdb entry has unparseable mac; skipping",
				slog.Int("vni", int(vni)), slog.String("mac", e.MAC), slog.String("error", err.Error()))
			continue
		}
		dst, err := netip.ParseAddr(e.VtepIP)
		if err != nil {
			unparseable++
			r.log.WarnContext(ctx, "overlay fdb entry has unparseable vtep_ip; skipping",
				slog.Int("vni", int(vni)), slog.String("vtep_ip", e.VtepIP), slog.String("error", err.Error()))
			continue
		}
		ent := netfabric.FDBEntry{MAC: mac, Dst: dst}
		desired[fdbKey(ent)] = ent
	}
	return desired, unparseable
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
