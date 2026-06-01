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
// what it materialised in its own applied map. That memory is in-process
// only: a restart loses it, so managed bridges declared once and then
// removed across a restart are orphaned on the host. Acceptable for this
// iteration - the next reconcile after re-declaration re-adopts them, and
// stale-bridge GC is future work.
type Networks struct {
	log    *slog.Logger
	fabric netfabric.Fabric
	tick   time.Duration

	desired atomic.Pointer[[]heartbeat.DeclaredNetwork]
	trigger chan struct{}

	mu      sync.Mutex
	reports map[string]heartbeat.NetworkReport

	// applied records the networks this reconciler has materialised,
	// keyed by network id, so removals can be detected and torn down
	// with the right primitives. Mutated only from the reconcile
	// goroutine; no lock needed.
	applied map[string]appliedNetwork
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
}

// NewNetworks builds the network reconciler. tick==0 falls back to
// DefaultTickInterval. Returns ErrNilFabric when fabric is nil so
// boot-time misconfiguration surfaces at construction, not at the first
// reconcile.
func NewNetworks(f netfabric.Fabric, log *slog.Logger, tick time.Duration) (*Networks, error) {
	if f == nil {
		return nil, ErrNilFabric
	}
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	return &Networks{
		log:     log,
		fabric:  f,
		tick:    tick,
		trigger: make(chan struct{}, 1),
		reports: map[string]heartbeat.NetworkReport{},
		applied: map[string]appliedNetwork{},
	}, nil
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. The
// sender invokes this immediately after a successful POST returns,
// outside the reconciler's own goroutine. We copy the slice (the
// sender's struct may be reused) and nudge the trigger channel.
func (r *Networks) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	networks := append([]heartbeat.DeclaredNetwork(nil), resp.DeclaredNetworks...)
	r.desired.Store(&networks)
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
	desiredPtr := r.desired.Load()
	var desired []heartbeat.DeclaredNetwork
	if desiredPtr != nil {
		desired = *desiredPtr
	}

	declared := make(map[string]struct{}, len(desired))
	nextReports := make(map[string]heartbeat.NetworkReport, len(desired))
	for _, d := range desired {
		declared[d.ID] = struct{}{}
		nextReports[d.ID] = r.applyNetwork(ctx, d)
	}

	r.removeUndeclared(ctx, declared)

	r.mu.Lock()
	r.reports = nextReports
	r.mu.Unlock()
}

// applyNetwork materialises one declared network and returns its report.
// It also updates r.applied on success so a later removal knows what to
// tear down.
func (r *Networks) applyNetwork(ctx context.Context, d heartbeat.DeclaredNetwork) heartbeat.NetworkReport {
	if d.Type != "bridge" {
		// Overlay and other types land in N1b; do not touch the fabric.
		return r.failed(ctx, d, fmt.Sprintf("unsupported network type %q (overlay lands in N1b)", d.Type))
	}

	subnet, gateway, err := parseSubnetGateway(d)
	if err != nil {
		return r.failed(ctx, d, err.Error())
	}

	if !d.Managed {
		return r.applyUnmanaged(ctx, d)
	}
	return r.applyManaged(ctx, d, subnet, gateway)
}

// applyManaged ensures the bridge (and, for egress=nat, the gateway
// address and masquerade rule) exist for a CP-managed network.
func (r *Networks) applyManaged(ctx context.Context, d heartbeat.DeclaredNetwork, subnet, gateway netip.Prefix) heartbeat.NetworkReport {
	if err := r.fabric.EnsureBridge(d.BridgeName, int(d.Mtu)); err != nil {
		return r.failed(ctx, d, err.Error())
	}

	applied := appliedNetwork{BridgeName: d.BridgeName, Managed: true}

	if d.Egress == "nat" {
		if !subnet.IsValid() || !gateway.IsValid() {
			return r.failed(ctx, d, "egress=nat requires subnet+gateway")
		}
		gatewayAddr := netip.PrefixFrom(gateway.Addr(), subnet.Bits())
		if err := r.fabric.EnsureGatewayAddr(d.BridgeName, gatewayAddr); err != nil {
			return r.failed(ctx, d, err.Error())
		}
		// Empty egress iface — netfabric resolves the host default route.
		// Per-network egress-iface override is future work.
		if err := r.fabric.EnsureMasquerade(subnet, ""); err != nil {
			return r.failed(ctx, d, err.Error())
		}
		applied.HasNAT = true
		applied.Subnet = subnet
		applied.Gateway = gatewayAddr
	}

	r.applied[d.ID] = applied
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
		if a.HasNAT {
			if err := r.fabric.RemoveMasquerade(a.Subnet); err != nil {
				r.log.WarnContext(ctx, "remove masquerade failed during network teardown",
					slog.String("network_id", id),
					slog.String("error", err.Error()),
				)
			}
			if err := r.fabric.RemoveGatewayAddr(a.BridgeName, a.Gateway); err != nil {
				r.log.WarnContext(ctx, "remove gateway addr failed during network teardown",
					slog.String("network_id", id),
					slog.String("error", err.Error()),
				)
			}
		}
		if err := r.fabric.RemoveBridge(a.BridgeName); err != nil {
			r.log.WarnContext(ctx, "remove bridge failed during network teardown",
				slog.String("network_id", id),
				slog.String("error", err.Error()),
			)
		}
		r.log.InfoContext(ctx, "managed network torn down (CP-side delete reconciled)",
			slog.String("network_id", id),
			slog.String("bridge", a.BridgeName),
		)
		delete(r.applied, id)
	}
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

// ready builds a ready NetworkReport for the given network id.
func ready(id string) heartbeat.NetworkReport {
	return heartbeat.NetworkReport{ID: id, ReconciliationStatus: "ready"}
}

// ErrNilFabric guards against nil-injection at construction time.
// Returned by NewNetworks on a nil fabric so the agent fails-fast at boot
// rather than panic on the first reconcile.
var ErrNilFabric = errors.New("reconciler: netfabric.Fabric is required")
