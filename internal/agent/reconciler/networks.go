// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// Networks is the per-resource reconciler for cluster-wide networks.
// Single instance per agent process; owned by the agent's server-level
// glue, which starts Run in a goroutine and supplies the fabric.
//
// Implements heartbeat.ResponseHandler (HandleHeartbeatResponse) and
// heartbeat.NetworkReporter (NetworkReports). The sender wires the same
// reconciler instance to both seams.
//
// Unlike the pool reconciler the network reconciler has no agent-side
// "manager.List()" of observed bridges to diff against, so it tracks
// what it materialised in its own applied map. Same-process config
// changes to an already-applied network (bridge rename, nat->none,
// subnet change) ARE handled: reconcileDelta tears down the stale host
// resources before materialising the new state, so nothing leaks while
// the process is up. That memory is in-process only: a restart loses it,
// so managed bridges declared once and then removed across a restart are
// orphaned on the host. Acceptable for this iteration - the next
// reconcile after re-declaration re-adopts them, and stale-bridge GC is
// future work.
type Networks struct {
	log    *slog.Logger
	fabric netfabric.Fabric
	dhcp   dhcp4.Responder // per-node DHCPv4 responder for dhcp-enabled overlays; may be nil
	tick   time.Duration

	// hostsVMs is true when this node runs the hypervisor plane: it materialises
	// type=bridge networks and the overlay services plane (anycast/NAT/DNS) for its
	// own VMs. A gateway-only node sets it false and materialises only the overlay
	// transport (bridge + VTEP + FDB) plus its ingress veths. It is orthogonal to
	// the per-network veth axis, which keys on the declared GatewayAddr.
	hostsVMs bool

	desired atomic.Pointer[networksDesired]
	trigger chan struct{}

	mu      sync.Mutex
	reports map[string]heartbeat.NetworkReport

	// sessionMu guards sessions. The connect/splice plane mutates the counter
	// from request goroutines (AcquireSession/ReleaseSession) while the heartbeat
	// collector reads it (NetworkReports); a dedicated mutex keeps the
	// session-count path off the reconcile-goroutine lock.
	sessionMu sync.Mutex
	// sessions counts live ingress sessions per network id, keyed the same way
	// the NetworkReport is (the declared network id). Only an ingress gateway
	// populates it; a network with no live session drops out of the map.
	sessions map[string]int

	// applied records the networks this reconciler has materialised,
	// keyed by network id, so removals can be detected and torn down
	// with the right primitives. Mutated only from the reconcile
	// goroutine; no lock needed.
	applied map[string]appliedNetwork

	// dhcpRegisterErr remembers the last-logged DHCP RegisterNetwork error
	// string per network id, so a permanently failing registration logs a
	// WARN only on transition (first failure or a changed error) instead of
	// every reconcile pass. Cleared on a successful register (emitting a
	// single recovery INFO) and on teardown, so a later re-failure re-logs.
	// Mutated only from the reconcile goroutine; no lock needed, same as
	// applied.
	dhcpRegisterErr map[string]string
}

// appliedNetwork is the reconciler's memory of one materialised managed
// network, retained so a later removal tears down exactly what was put
// in place (bridge, and gateway+masquerade when the network was NAT).
type appliedNetwork struct {
	BridgeName string
	Managed    bool
	HasNAT     bool
	Subnet     netip.Prefix
	Gateway    netip.Prefix
	Overlay    bool   // true for a type=overlay network: teardown removes the VTEP too
	VNI        uint32 // VXLAN id, for RemoveVXLAN on teardown (Overlay only)
	HasEgress  bool   // true for an overlay materialised with egress=nat: teardown removes the iface masquerade
	HasDHCP    bool   // true for a dhcp-enabled overlay registered with the responder: teardown deregisters it
	Anycast    bool   // true for a managed bridge materialised with the dhcp/dns anycast services path (anycast gateway + DNS at 169.254.1.1, plus masquerade by subnet when nat). Teardown removes the masquerade and deregisters the DHCP responder; the anycast /32 goes with RemoveBridge. Not a real-gateway path, so teardownNAT skips RemoveGatewayAddr for it.
	HasVeth    bool   // true for a gateway overlay whose ingress veth (otvg<vni>) is up: teardown removes it, and a later GatewayAddr->nil reaps it.
}

// networksDesired is the latest CP-declared network intent for this node: the
// declared set plus the node's own overlay IP (self_overlay_ip), which the
// overlay path needs as the VTEP source. Bridge networks ignore the IP.
type networksDesired struct {
	networks      []heartbeat.DeclaredNetwork
	selfOverlayIP string                       // CIDR "10.42.0.1/16"; "" until the CP allocates
	fdb           []heartbeat.DeclaredFDBEntry // CP-declared VXLAN FDB entries, programmed per-VNI
}

// NewNetworks builds the network reconciler. tick==0 falls back to
// DefaultTickInterval. Returns ErrNilFabric when fabric is nil so
// boot-time misconfiguration surfaces at construction, not at the first
// reconcile. dhcp may be nil, in which case the reconciler skips DHCP
// registration entirely (a node with no DHCP responder support). hostsVMs is
// true for a hypervisor node (materialise type=bridge networks and the overlay
// services plane for its own VMs) and false for a gateway-only node (overlay
// transport plus ingress veths only); see the field doc.
func NewNetworks(f netfabric.Fabric, dhcp dhcp4.Responder, log *slog.Logger, tick time.Duration, hostsVMs bool) (*Networks, error) {
	if f == nil {
		return nil, ErrNilFabric
	}
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	return &Networks{
		log:             log,
		fabric:          f,
		dhcp:            dhcp,
		tick:            tick,
		hostsVMs:        hostsVMs,
		trigger:         make(chan struct{}, 1),
		reports:         map[string]heartbeat.NetworkReport{},
		sessions:        map[string]int{},
		applied:         map[string]appliedNetwork{},
		dhcpRegisterErr: map[string]string{},
	}, nil
}

// OverlayNetworkForIP returns the veth host-end device AND the network id whose
// CP-declared subnet contains ip, but only for an overlay this node holds a
// gateway membership on (GatewayAddr set), because ingress sources its dials from
// that overlay's veth host end. The connect path binds the dial and the neighbor
// lookup to the returned device and keys its per-network live-session counter by
// the network id. It consults only the latest declared state, the same
// authoritative source the reconciler applies from; ok is false when no declared
// gateway overlay's subnet contains ip, so a credential for an overlay this node
// does not gateway fails closed. Only overlay networks (VNI + subnet) with a
// gateway membership are considered.
func (r *Networks) OverlayNetworkForIP(ip netip.Addr) (device, networkID string, ok bool) {
	d := r.desired.Load()
	if d == nil {
		return "", "", false
	}
	for _, n := range d.networks {
		if n.VNI == nil || *n.VNI <= 0 || n.Subnet == nil || n.GatewayAddr == nil {
			continue
		}
		subnet, err := netip.ParsePrefix(*n.Subnet)
		if err != nil {
			continue
		}
		if subnet.Contains(ip) {
			return netfabric.GatewayVethHostName(uint32(*n.VNI)), n.ID, true //nolint:gosec // VNI guarded >0 and <= 24-bit
		}
	}
	return "", "", false
}

// AcquireSession records one live ingress session on the network. The connect
// plane calls it when a session takes a connection slot; the count folds into
// the network's heartbeat report so the CP keeps the gateway's membership sticky
// while the session drains. Keyed by the declared network id.
func (r *Networks) AcquireSession(networkID string) {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	r.sessions[networkID]++
}

// ReleaseSession releases a session previously recorded by AcquireSession. It
// never drives the count below zero (a spurious release is a no-op) and drops the
// entry at zero so an idle network reports no sessions. Mirrors the connect
// plane's slot-release discipline so the counter cannot leak.
func (r *Networks) ReleaseSession(networkID string) {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	if r.sessions[networkID] <= 0 {
		return
	}
	r.sessions[networkID]--
	if r.sessions[networkID] == 0 {
		delete(r.sessions, networkID)
	}
}

// activeSessions returns the live ingress-session count for the network id.
func (r *Networks) activeSessions(networkID string) int {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	return r.sessions[networkID]
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. The
// sender invokes this immediately after a successful POST returns,
// outside the reconciler's own goroutine. We copy the slice (the
// sender's struct may be reused) and nudge the trigger channel.
func (r *Networks) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	d := &networksDesired{
		networks: append([]heartbeat.DeclaredNetwork(nil), resp.DeclaredNetworks...),
		fdb:      append([]heartbeat.DeclaredFDBEntry(nil), resp.DeclaredFDB...),
	}
	if resp.SelfOverlayIP != nil {
		d.selfOverlayIP = *resp.SelfOverlayIP
	}
	r.desired.Store(d)
	select {
	case r.trigger <- struct{}{}:
	default:
		// Earlier nudge already queued — collapse into one reconcile.
	}
}

// NetworkReports implements heartbeat.NetworkReporter. The collector
// calls this on every tick to fill HeartbeatRequest.networks. The
// snapshot covers every network the reconciler processed in its last
// pass; entries the CP no longer declares stop appearing.
func (r *Networks) NetworkReports() []heartbeat.NetworkReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		return nil
	}
	out := make([]heartbeat.NetworkReport, 0, len(r.reports))
	for _, rep := range r.reports {
		// Fold in the live ingress-session count so the CP can hold this
		// gateway's overlay membership sticky while sessions drain.
		rep.ActiveSessions = r.activeSessions(rep.ID)
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Run blocks until ctx is cancelled. Ticks every r.tick OR on trigger;
// each tick runs one reconcile pass.
func (r *Networks) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	// Initial pass — in case the first heartbeat lands before this loop
	// boots. Cheap when there's nothing to do.
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.trigger:
			r.reconcile(ctx)
		}
	}
}

// reconcile is one pass over the (desired, applied) diff. It materialises
// every declared type=bridge network (idempotent EnsureBridge /
// EnsureGatewayAddr / EnsureMasquerade) and tears down managed bridges
// that have dropped out of the declared set. Errors are recorded in the
// reports map; they do not propagate. The next tick retries.
func (r *Networks) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	d := r.desired.Load()
	var desired []heartbeat.DeclaredNetwork
	var selfOverlayIP string
	var fdb []heartbeat.DeclaredFDBEntry
	if d != nil {
		desired = d.networks
		selfOverlayIP = d.selfOverlayIP
		fdb = d.fdb
	}

	declared := make(map[string]struct{}, len(desired))
	nextReports := make(map[string]heartbeat.NetworkReport, len(desired))
	for _, dn := range desired {
		declared[dn.ID] = struct{}{}
		nextReports[dn.ID] = r.applyNetwork(ctx, dn, selfOverlayIP, fdb)
	}

	r.removeUndeclared(ctx, declared)

	r.mu.Lock()
	r.reports = nextReports
	r.mu.Unlock()
}

// applyNetwork materialises one declared network and returns its report.
// It also updates r.applied on success so a later removal knows what to
// tear down.
func (r *Networks) applyNetwork(ctx context.Context, d heartbeat.DeclaredNetwork, selfOverlayIP string, fdb []heartbeat.DeclaredFDBEntry) heartbeat.NetworkReport {
	switch d.Type {
	case "bridge":
		if !r.hostsVMs {
			// A node that hosts no VMs (a gateway-only node) is never a first-hop
			// router, so a node-local bridge has no role on it: it runs neither the
			// L3/NAT/DHCP services plane nor even a VM-less bridge to attach taps to.
			// Bridge VMs are reached through their owning agent, never directly
			// through the gateway, which carries tenant traffic only over overlays.
			// Report it reconciled so the CP sees the node converge on a network it
			// correctly does nothing about, rather than stall on a failed report.
			return ready(d.ID)
		}
		subnet, gateway, err := parseSubnetGateway(d)
		if err != nil {
			return r.failed(ctx, d, err.Error())
		}
		if !d.Managed {
			return r.applyUnmanaged(ctx, d)
		}
		return r.applyManaged(ctx, d, subnet, gateway)
	case "overlay":
		return r.applyOverlay(ctx, d, selfOverlayIP, fdb)
	default:
		return r.failed(ctx, d, fmt.Sprintf("unsupported network type %q", d.Type))
	}
}

// applyManaged ensures the bridge (and, for egress=nat, the gateway
// address and masquerade rule) exist for a CP-managed network. When the
// same network id was previously materialised with a now-stale config
// (renamed bridge, dropped/changed NAT subnet), the stale host resources
// are torn down first so they do not leak — see reconcileDelta.
func (r *Networks) applyManaged(ctx context.Context, d heartbeat.DeclaredNetwork, subnet, gateway netip.Prefix) heartbeat.NetworkReport {
	r.reconcileDelta(ctx, d, subnet)

	if err := r.fabric.EnsureBridge(d.BridgeName, int(d.Mtu)); err != nil {
		return r.failed(ctx, d, err.Error())
	}

	// Record the bridge in r.applied the instant it exists on the host,
	// before the NAT steps that may still fail. Without this an
	// EnsureBridge-ok-but-NAT-failed network is absent from r.applied, so a
	// later CP-side delete (while NAT is still failing) makes
	// removeUndeclared skip it and the bridge orphans with no GC. HasNAT
	// stays false here so reconcileDelta treats a re-recorded still-failing
	// network as an unchanged no-op rather than a NAT-drop teardown.
	r.applied[d.ID] = appliedNetwork{BridgeName: d.BridgeName, Managed: true}

	nat := d.Egress == "nat"
	if d.DhcpEnabled || d.DNSEnabled {
		// dhcp/dns -> overlay-style anycast services (gateway + DNS at
		// 169.254.1.1). The CP guarantees a subnet for this case.
		if !subnet.IsValid() {
			return r.failed(ctx, d, "dhcp/dns requires a subnet")
		}
		return r.applyManagedServices(ctx, d, subnet, nat)
	}

	if d.Egress == "nat" {
		if !subnet.IsValid() || !gateway.IsValid() {
			return r.failed(ctx, d, "egress=nat requires subnet+gateway")
		}
		gatewayAddr := netip.PrefixFrom(gateway.Addr(), subnet.Bits())
		if err := r.fabric.EnsureGatewayAddr(d.BridgeName, gatewayAddr); err != nil {
			return r.failed(ctx, d, err.Error())
		}
		// Empty egress iface — netfabric resolves the host default route.
		// Per-network egress-iface override is future work. The bridge name
		// scopes the rule to this network: managed-bridge subnets may overlap,
		// so a rule matched on source subnet alone could SNAT another
		// network's traffic.
		if err := r.fabric.EnsureMasquerade(subnet, d.BridgeName, ""); err != nil {
			return r.failed(ctx, d, err.Error())
		}
		// NAT converged: augment the recorded entry so a later teardown
		// reclaims the masquerade and gateway addr too.
		r.applied[d.ID] = appliedNetwork{
			BridgeName: d.BridgeName,
			Managed:    true,
			HasNAT:     true,
			Subnet:     subnet,
			Gateway:    gatewayAddr,
		}
	}

	return ready(d.ID)
}

// applyManagedServices installs the anycast DHCP/DNS datapath for a managed
// bridge (isolated per-host L2), reusing the overlay service primitives: the
// anycast gateway + bridge route (when dns or nat), NAT masquerade by subnet
// (when nat), and the DHCP responder (when dhcp). Each step is best-effort and
// retried next pass - a services hiccup must not exclude the node from VM
// placement, so the report is ready once the bridge exists. The full intended
// services shape is recorded up front so a later CP-side delete tears down every
// resource even if a step below fails midway; every teardown step is idempotent,
// so over-recording is safe.
func (r *Networks) applyManagedServices(ctx context.Context, d heartbeat.DeclaredNetwork, subnet netip.Prefix, nat bool) heartbeat.NetworkReport {
	r.applied[d.ID] = appliedNetwork{
		BridgeName: d.BridgeName,
		Managed:    true,
		Anycast:    true,
		HasNAT:     nat,
		Subnet:     subnet,
		HasDHCP:    d.DhcpEnabled && r.dhcp != nil,
	}

	wantL3 := nat || d.DNSEnabled
	if nat {
		if err := r.fabric.EnableIPForwarding(); err != nil {
			r.log.WarnContext(ctx, "managed bridge egress: enable ip_forward failed; retrying next pass",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
			return ready(d.ID)
		}
	}
	if wantL3 {
		if !r.ensureAnycastL3Plane(ctx, d, netfabric.GatewayMACFromID(d.ID)) {
			return ready(d.ID)
		}
	}
	if nat {
		// Empty egress iface -> netfabric resolves the host default route.
		// The bridge name scopes the rule to this network: managed-bridge
		// subnets may overlap, so a rule matched on source subnet alone could
		// SNAT another network's traffic.
		if err := r.fabric.EnsureMasquerade(subnet, d.BridgeName, ""); err != nil {
			r.log.WarnContext(ctx, "managed bridge egress: masquerade failed; retrying next pass",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
			return ready(d.ID)
		}
	}
	r.registerDHCP(ctx, d, nat)
	return ready(d.ID)
}

// applyUnmanaged verifies an operator-provisioned bridge exists for an
// attach-only network. It never creates or modifies the bridge; taps are
// attached later in the VM path.
func (r *Networks) applyUnmanaged(ctx context.Context, d heartbeat.DeclaredNetwork) heartbeat.NetworkReport {
	if d.Egress == "nat" {
		// CP rejects managed=false + nat; report defensively if seen.
		return r.failed(ctx, d, "egress=nat is not valid for an unmanaged network")
	}
	exists, err := r.fabric.BridgeExists(d.BridgeName)
	if err != nil {
		return r.failed(ctx, d, err.Error())
	}
	if !exists {
		return r.failed(ctx, d, fmt.Sprintf("bridge %s not present; managed=false requires an operator-provisioned bridge", d.BridgeName))
	}
	r.applied[d.ID] = appliedNetwork{BridgeName: d.BridgeName, Managed: false}
	return ready(d.ID)
}

// reconcileDelta tears down host resources from a prior materialisation
// of the SAME network id that the incoming desired state will not
// re-assert. The CP allows PATCH to mutate bridge_name, egress, subnet
// and gateway on an existing network; without this the old bridge and/or
// old NAT masquerade+gateway addr would leak, since removeUndeclared only
// fires when the id leaves the declared set entirely. Teardown is
// best-effort (warn and continue): a stale-cleanup hiccup must not block
// materialising the new state, so the overall report is driven by the
// NEW assertions, not these removals. Idempotent: it only acts on what
// changed, so a steady-state reconcile of an unchanged network is a no-op.
func (r *Networks) reconcileDelta(ctx context.Context, d heartbeat.DeclaredNetwork, newSubnet netip.Prefix) {
	prev, ok := r.applied[d.ID]
	if !ok || !prev.Managed {
		return
	}
	newNAT := d.Egress == "nat"
	// A dhcp/dns (anycast) managed bridge that keeps its bridge but changes
	// subnet leaves a stale link-scoped bridge route: EnsureBridgeRoute installs
	// the new Dst but cannot remove the old one, and nothing else GCs it (the
	// route only dies with the bridge). Reap the old route here. Same-bridge
	// only - a rename tears the whole old bridge down below, route and all.
	// Best-effort: a failure is logged and retried next pass (the id stays
	// declared), it must not block materialising the new state.
	if prev.Anycast && prev.BridgeName == d.BridgeName &&
		prev.Subnet.IsValid() && prev.Subnet != newSubnet {
		if err := r.fabric.RemoveBridgeRoute(prev.Subnet, prev.BridgeName); err != nil {
			r.log.WarnContext(ctx, "remove stale bridge route failed; retrying next pass",
				slog.String("network_id", d.ID), slog.String("error", err.Error()))
		}
	}
	switch {
	case prev.BridgeName != d.BridgeName:
		// Bridge rename: tear the old bridge down entirely. The new
		// bridge (and its NAT, if any) is created fresh below. Ignore a
		// teardown failure (already logged inside teardownManaged): the id
		// stays declared, so the next reconcile pass retries the stale
		// cleanup; a hiccup here must not block materialising the new state.
		_ = r.teardownManaged(ctx, d.ID, prev)
	case prev.HasNAT && (!newNAT || prev.Subnet != newSubnet):
		// Same bridge, but NAT was dropped or its subnet changed: drop
		// the stale masquerade+gateway. The new NAT state, if any, is
		// re-asserted with the new subnet/gateway below. Ignore a teardown
		// failure (already logged inside teardownNAT) for the same reason as
		// the rename case: the id stays declared and the next pass retries.
		_ = r.teardownNAT(ctx, d.ID, prev)
	}
}

// removeUndeclared tears down managed bridges no longer in the declared
// set. Unmanaged (operator) bridges are never touched - they are only
// forgotten.
func (r *Networks) removeUndeclared(ctx context.Context, declared map[string]struct{}) {
	for id, a := range r.applied {
		if _, ok := declared[id]; ok {
			continue
		}
		if !a.Managed {
			r.log.InfoContext(ctx, "network forgotten (unmanaged bridge left intact)",
				slog.String("network_id", id),
				slog.String("bridge", a.BridgeName),
			)
			delete(r.applied, id)
			continue
		}
		if err := r.teardownManaged(ctx, id, a); err != nil {
			// Teardown left host state partially in place (per-step causes
			// already logged inside teardownManaged). Keep the id in
			// r.applied so the next reconcile tick retries; do not forget it
			// yet, or the residual bridge/VTEP/NAT would orphan with no GC.
			r.log.WarnContext(ctx, "managed network teardown incomplete, will retry next reconcile",
				slog.String("network_id", id),
				slog.String("bridge", a.BridgeName),
			)
			continue
		}
		r.log.InfoContext(ctx, "managed network torn down (CP-side delete reconciled)",
			slog.String("network_id", id),
			slog.String("bridge", a.BridgeName),
		)
		delete(r.applied, id)
	}
}

// teardownManaged removes every host resource a managed network put in
// place: the NAT masquerade+gateway addr (when it had them) and the
// bridge itself. Best-effort - it attempts every step even when an earlier
// one fails, so a partial host state still converges as far as possible.
// Each failure is logged here (single owner) and the failures are joined
// into the returned error so the caller can gate forgetting the network on
// a clean teardown; callers MUST NOT re-log the returned error. Returns nil
// when every step succeeded. Every step is idempotent (nil on an
// already-absent resource), so retrying a partially-completed teardown is
// safe.
func (r *Networks) teardownManaged(ctx context.Context, id string, a appliedNetwork) error {
	if a.Overlay {
		var errs []error
		// Always attempt masquerade removal: it is idempotent (no-op when no
		// rule exists), and best-effort egress install can leave a rule while
		// HasEgress reads false, so gating on HasEgress could leak the rule.
		if err := r.fabric.RemoveMasqueradeIface(a.BridgeName); err != nil {
			r.log.WarnContext(ctx, "remove overlay egress masquerade failed during teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
		}
		// Always attempt deregistration: DeregisterNetwork is idempotent (no-op
		// when the network was never registered), and best-effort DHCP register
		// can leave the responder serving a bridge while HasDHCP reads false, so
		// gating on HasDHCP could leak the socket.
		if r.dhcp != nil {
			if err := r.dhcp.DeregisterNetwork(id); err != nil {
				r.log.WarnContext(ctx, "deregister overlay dhcp failed during teardown",
					slog.String("network_id", id),
					slog.String("error", err.Error()),
				)
				errs = append(errs, err)
			}
		}
		// Forget any remembered register-error state so a deleted-then-recreated
		// network re-logs its first failure. Best-effort, silent: the network is
		// going away, not recovering, so no recovery INFO here.
		delete(r.dhcpRegisterErr, id)
		// Always attempt veth removal: RemoveVeth is idempotent (nil on an absent
		// otvg<vni>, so a no-op for a hypervisor overlay that never had one), and a
		// partial create or a failed reap can leave a veth while HasVeth reads
		// false - gating on HasVeth could leak it. Same reasoning as the
		// unconditional RemoveMasqueradeIface / DeregisterNetwork above.
		if err := r.fabric.RemoveVeth(netfabric.GatewayVethHostName(a.VNI)); err != nil {
			r.log.WarnContext(ctx, "remove gateway veth failed during overlay teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
		}
		if err := r.fabric.RemoveVXLAN(a.VNI); err != nil {
			r.log.WarnContext(ctx, "remove vxlan failed during overlay teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
		}
		if err := r.fabric.RemoveBridge(a.BridgeName); err != nil {
			r.log.WarnContext(ctx, "remove bridge failed during overlay teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	var errs []error
	if err := r.teardownNAT(ctx, id, a); err != nil {
		errs = append(errs, err)
	}
	if a.Anycast && r.dhcp != nil {
		// The dhcp/dns anycast path registered the responder on this bridge;
		// deregister it (idempotent) so a CP-side delete does not leak the socket.
		if err := r.dhcp.DeregisterNetwork(id); err != nil {
			r.log.WarnContext(ctx, "deregister dhcp failed during managed bridge teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
		}
		// Forget any remembered register-error state so a deleted-then-recreated
		// network re-logs its first failure.
		delete(r.dhcpRegisterErr, id)
	}
	if err := r.fabric.RemoveBridge(a.BridgeName); err != nil {
		r.log.WarnContext(ctx, "remove bridge failed during network teardown",
			slog.String("network_id", id),
			slog.String("error", err.Error()),
		)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// teardownNAT removes the masquerade rule and gateway address a NAT
// network installed, leaving the bridge intact. A no-op (nil) when the
// network had no NAT. Best-effort: it attempts both steps even when the
// first fails, logs each failure here (single owner), and joins them into
// the returned error; callers MUST NOT re-log it. Returns nil when every
// step succeeded.
func (r *Networks) teardownNAT(ctx context.Context, id string, a appliedNetwork) error {
	if !a.HasNAT {
		return nil
	}
	var errs []error
	if err := r.fabric.RemoveMasquerade(a.Subnet, a.BridgeName); err != nil {
		r.log.WarnContext(ctx, "remove masquerade failed during network teardown",
			slog.String("network_id", id),
			slog.String("error", err.Error()),
		)
		errs = append(errs, err)
	}
	// The anycast (dhcp/dns) path installs no real in-subnet gateway addr - its
	// gateway is the link-local anycast /32 removed with the bridge - so skip the
	// gateway-addr removal for it (a.Gateway is the zero prefix there).
	if !a.Anycast {
		if err := r.fabric.RemoveGatewayAddr(a.BridgeName, a.Gateway); err != nil {
			r.log.WarnContext(ctx, "remove gateway addr failed during network teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// parseSubnetGateway parses the optional Subnet (CIDR) and Gateway (IP)
// fields. Absent fields yield zero (invalid) prefixes. A malformed value
// is an error that surfaces as a failed report.
func parseSubnetGateway(d heartbeat.DeclaredNetwork) (subnet, gateway netip.Prefix, err error) {
	if d.Subnet != nil {
		subnet, err = netip.ParsePrefix(*d.Subnet)
		if err != nil {
			return netip.Prefix{}, netip.Prefix{}, fmt.Errorf("parse subnet %q: %v", *d.Subnet, err)
		}
	}
	if d.Gateway != nil {
		addr, perr := netip.ParseAddr(*d.Gateway)
		if perr != nil {
			return netip.Prefix{}, netip.Prefix{}, fmt.Errorf("parse gateway %q: %v", *d.Gateway, perr)
		}
		gateway = netip.PrefixFrom(addr, addr.BitLen())
	}
	return subnet, gateway, nil
}

// failed builds a failed NetworkReport and logs the cause.
func (r *Networks) failed(ctx context.Context, d heartbeat.DeclaredNetwork, msg string) heartbeat.NetworkReport {
	r.log.WarnContext(ctx, "network reconcile failed",
		slog.String("network_id", d.ID),
		slog.String("network", d.Name),
		slog.String("bridge", d.BridgeName),
		slog.String("error", msg),
	)
	m := msg
	return heartbeat.NetworkReport{
		ID:                   d.ID,
		ReconciliationStatus: "failed",
		ReconciliationError:  &m,
	}
}

// pending builds a pending NetworkReport: the network cannot be materialised yet
// (e.g. otwg0 not ready) but this is a transient wait, not a failure. The reason
// is surfaced in ReconciliationError for operator visibility. Logged at debug so
// a healthy fail-closed wait does not spam warnings.
func (r *Networks) pending(ctx context.Context, d heartbeat.DeclaredNetwork, msg string) heartbeat.NetworkReport {
	r.log.DebugContext(ctx, "network reconcile pending",
		slog.String("network_id", d.ID),
		slog.String("network", d.Name),
		slog.String("reason", msg),
	)
	m := msg
	return heartbeat.NetworkReport{
		ID:                   d.ID,
		ReconciliationStatus: "pending",
		ReconciliationError:  &m,
	}
}

// ready builds a ready NetworkReport for the given network id.
func ready(id string) heartbeat.NetworkReport {
	return heartbeat.NetworkReport{ID: id, ReconciliationStatus: "ready"}
}

// ErrNilFabric guards against nil-injection at construction time.
// Returned by NewNetworks on a nil fabric so the agent fails-fast at boot
// rather than panic on the first reconcile.
var ErrNilFabric = errors.New("reconciler: netfabric.Fabric is required")
