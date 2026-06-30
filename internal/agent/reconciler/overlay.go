// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// vxlanUDPPort is the IANA VXLAN destination port every overlay VTEP uses.
const vxlanUDPPort = 4789

// applyOverlay materialises one type=overlay network fail-closed. It brings up
// the otvb<vni> bridge and the otvx<vni> VXLAN VTEP bound to this node's otwg0
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
	// and the otvb<vni> bridge orphans with no GC (r.applied is in-process only).
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
	// An ingress gateway owns a tenant IP + a distinct unicast MAC on the bridge:
	// the host originates and answers at the tenant IP, and the bridge claims the
	// MAC advertised to peers in the overlay FDB so return traffic is delivered
	// here. Only a gateway recipient receives a gateway addr; a hypervisor node
	// gets nil and skips this, leaving the bridge MAC to the anycast services path.
	if err := r.applyGatewayAddr(d); err != nil {
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

	if r.overlayNeedsServices(d) {
		r.applyOverlayServices(ctx, d, vniVal)
	}

	return ready(d.ID)
}

// applyGatewayAddr pins the ingress gateway's tenant IP and its distinct unicast
// MAC onto the overlay bridge so the gateway host originates and answers at the
// tenant IP and the bridge owns the MAC advertised to peers in the overlay FDB.
// The tenant IP is assigned with the overlay subnet's prefix length (from
// d.Subnet, e.g. /24), never a /32: the subnet prefix gives the gateway host an
// on-link route to the whole overlay subnet via the bridge, so its dial to a
// guest VM leaves over the overlay rather than the host default route.
// It is a no-op for a hypervisor node (d.GatewayAddr nil), which leaves the
// bridge hardware address to the anycast services path. An unparseable IP or MAC
// is a corrupt declared entry that can never be programmed, so it surfaces as a
// hard error and holds the overlay divergent rather than silently materialising
// the network without its gateway address. A missing or unparseable subnet is
// likewise a hard error: a tenant IP without a known subnet could only be
// installed as an unroutable /32, so failing is safer than materialising a
// broken gateway.
func (r *Networks) applyGatewayAddr(d heartbeat.DeclaredNetwork) error {
	if d.GatewayAddr == nil {
		return nil
	}
	ip, err := netip.ParseAddr(d.GatewayAddr.IP)
	if err != nil {
		return fmt.Errorf("gateway addr ip %q: %v", d.GatewayAddr.IP, err)
	}
	mac, err := net.ParseMAC(d.GatewayAddr.MAC)
	if err != nil {
		return fmt.Errorf("gateway addr mac %q: %v", d.GatewayAddr.MAC, err)
	}
	if d.Subnet == nil {
		return fmt.Errorf("gateway addr %s: overlay has no subnet to route the tenant IP", ip)
	}
	subnet, err := netip.ParsePrefix(*d.Subnet)
	if err != nil {
		return fmt.Errorf("gateway addr subnet %q: %v", *d.Subnet, err)
	}
	tenant := netip.PrefixFrom(ip, subnet.Bits())
	return r.fabric.EnsureUnicastGateway(d.BridgeName, tenant, mac)
}

// overlayNeedsServices reports whether an overlay needs the host-side services
// pass (L3 gateway, NAT egress, or DHCP). A pure L2 overlay needs none. A
// gateway reconciler never runs the services pass: a gateway hosts no VMs and is
// never an anycast first-hop router, so it brings up the overlay datapath
// without the services plane regardless of the declared egress/DNS/DHCP flags.
func (r *Networks) overlayNeedsServices(d heartbeat.DeclaredNetwork) bool {
	if r.gatewayMode {
		return false
	}
	return d.Egress == "nat" || d.DNSEnabled || d.DhcpEnabled
}

// applyOverlayServices best-effort installs the per-capability host datapath for
// an overlay whose L2 plane has already converged: NAT egress (ip_forward +
// masquerade), the L3 service plane (anycast gateway + bridge route, needed for
// either NAT or the host-side DNS forwarder so its replies reach the VM), and
// DHCP registration. Each capability is gated independently so DHCP and DNS work
// without NAT egress. Every step is a sub-capability: a failure is logged and
// retried on the next reconcile pass, and must NOT fail the network. Marking the
// whole overlay failed on a services hiccup would wrongly exclude the node from
// VM placement even though the VTEP/bridge/FDB datapath is up. HasEgress is
// recorded only when the full NAT datapath installs, so teardown and
// observability reflect the real state.
func (r *Networks) applyOverlayServices(ctx context.Context, d heartbeat.DeclaredNetwork, vniVal uint32) {
	nat := d.Egress == "nat"
	// L3 services (anycast gateway + bridge route) are needed by NAT egress and
	// by the host-side DNS forwarder: in both cases the host must own the gateway
	// address on the bridge and route to the overlay subnet so reply traffic
	// reaches the VM.
	wantL3 := nat || d.DNSEnabled

	if nat {
		if err := r.fabric.EnableIPForwarding(); err != nil {
			r.log.WarnContext(ctx, "overlay egress: enable ip_forward failed; retrying next pass",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
			return
		}
	}
	if wantL3 {
		if !r.ensureAnycastL3Plane(ctx, d, netfabric.GatewayMAC(vniVal)) {
			return
		}
	}
	if nat {
		// Empty egress iface -> netfabric resolves the host default route.
		if err := r.fabric.EnsureMasqueradeIface(d.BridgeName, ""); err != nil {
			r.log.WarnContext(ctx, "overlay egress: masquerade failed; retrying next pass",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
			return
		}
	}
	r.registerDHCP(ctx, d, nat)
	r.applied[d.ID] = appliedNetwork{
		BridgeName: d.BridgeName,
		Managed:    true,
		Overlay:    true,
		VNI:        vniVal,
		HasEgress:  nat,
		HasDHCP:    d.DhcpEnabled && r.dhcp != nil,
	}
}

// ensureAnycastL3Plane installs the host L3 service plane for a network whose L2
// has converged: the anycast gateway (169.254.1.1) on the bridge with the given
// per-network MAC, plus a connected bridge route to the subnet so the host-side
// DNS forwarder's replies reach the VM. Shared by overlay and managed-bridge
// service paths. Returns false on a hard fabric error (caller should return and
// retry next pass); a bad/absent subnet is logged and skipped (route is
// best-effort), still returning true. Idempotent.
func (r *Networks) ensureAnycastL3Plane(ctx context.Context, d heartbeat.DeclaredNetwork, gwMAC net.HardwareAddr) bool {
	if err := r.fabric.EnsureAnycastGateway(d.BridgeName, netfabric.OverlayGatewayAddr, gwMAC); err != nil {
		r.log.WarnContext(ctx, "l3: anycast gateway failed; retrying next pass",
			slog.String("network_id", d.ID), slog.String("error", err.Error()))
		return false
	}
	if d.Subnet != nil {
		if subnet, err := netip.ParsePrefix(*d.Subnet); err != nil {
			r.log.WarnContext(ctx, "l3: bad subnet, skipping bridge route",
				slog.String("network_id", d.ID), slog.String("subnet", *d.Subnet), slog.String("error", err.Error()))
		} else if err := r.fabric.EnsureBridgeRoute(subnet, d.BridgeName); err != nil {
			r.log.WarnContext(ctx, "l3: bridge route failed; retrying next pass",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
			return false
		}
	}
	return true
}

// registerDHCP re-asserts the DHCP registration on EVERY pass when
// enabled: the responder is idempotent (it swaps reservations), so this
// converges new or changed reservations. AdvertiseDNS follows the network's
// explicit dns setting: an operator who sets dns=false withholds the resolver
// even under egress=nat (dns and egress are independent; nat still advertises
// the default route via AdvertiseDefaultRoute). Best-effort, fail toward
// connectivity: a register failure is logged and retried next pass, it must NOT
// fail the network.
func (r *Networks) registerDHCP(ctx context.Context, d heartbeat.DeclaredNetwork, nat bool) {
	if d.DhcpEnabled && r.dhcp != nil {
		if subnet, err := netip.ParsePrefix(deref(d.Subnet)); err != nil {
			r.log.WarnContext(ctx, "overlay dhcp: bad subnet, skipping registration",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
		} else if err := r.dhcp.RegisterNetwork(dhcp4.NetworkConfig{
			NetworkID:             d.ID,
			Bridge:                d.BridgeName,
			Subnet:                subnet,
			Reservations:          parseReservations(ctx, r.log, d.Reservations),
			AdvertiseDNS:          d.DNSEnabled,
			AdvertiseDefaultRoute: nat,
		}); err != nil {
			// Log on transition only: registration is re-asserted every pass,
			// so a permanent failure (e.g. missing CAP_NET_RAW) would spam a
			// WARN every tick forever and defeat alerting-on-WARN. Emit only on
			// the first failure or a changed error string. Best-effort: keep
			// the overlay ready, retried next pass.
			if last, seen := r.dhcpRegisterErr[d.ID]; !seen || last != err.Error() {
				r.log.WarnContext(ctx, "overlay dhcp: register failed; retrying next pass",
					slog.String("network_id", d.ID), slog.String("error", err.Error()))
				r.dhcpRegisterErr[d.ID] = err.Error()
			}
		} else {
			r.clearDHCPRegisterErr(ctx, d.ID)
		}
	} else {
		// DHCP turned off (or no responder) while the network stays declared:
		// forget any prior register-error state, emitting the recovery INFO if
		// there was one, so a later re-enable re-logs a fresh first failure.
		r.clearDHCPRegisterErr(ctx, d.ID)
	}
}

// clearDHCPRegisterErr clears the remembered last-logged DHCP register error
// for id, emitting a single recovery INFO when there was a prior recorded
// failure. Clearing the entry means a later re-failure logs the WARN again.
// Called on a successful register and on teardown. Mutated only from the
// reconcile goroutine, same as r.applied; no lock needed.
func (r *Networks) clearDHCPRegisterErr(ctx context.Context, id string) {
	if _, had := r.dhcpRegisterErr[id]; !had {
		return
	}
	delete(r.dhcpRegisterErr, id)
	r.log.InfoContext(ctx, "overlay dhcp: register recovered",
		slog.String("network_id", id))
}

// parseReservations converts the wire reservations to dhcp4 form, skipping
// entries with an unparseable MAC or IP (logged), so one bad entry never drops
// the rest.
func parseReservations(ctx context.Context, log *slog.Logger, in []heartbeat.DhcpReservation) []dhcp4.Reservation {
	out := make([]dhcp4.Reservation, 0, len(in))
	for _, r := range in {
		mac, err := net.ParseMAC(r.MAC)
		if err != nil {
			log.WarnContext(ctx, "overlay dhcp: bad reservation mac, skipping",
				slog.String("mac", r.MAC), slog.String("error", err.Error()))
			continue
		}
		ip, err := netip.ParseAddr(r.IP)
		if err != nil {
			log.WarnContext(ctx, "overlay dhcp: bad reservation ip, skipping",
				slog.String("ip", r.IP), slog.String("error", err.Error()))
			continue
		}
		out = append(out, dhcp4.Reservation{MAC: mac, IP: ip})
	}
	return out
}

// deref returns the pointee of p, or the empty string when p is nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
