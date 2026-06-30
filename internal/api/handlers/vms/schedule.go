// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/netid"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// ScheduleStore is the storage surface the vms.schedule loop needs: list the
// unscheduled VMs, bind one (placement-locked), and record an unschedulable
// reason. *etcdstore.Store satisfies it.
type ScheduleStore interface {
	AcquirePlacementLock(ctx context.Context, lockKey int64) (func(), error)
	ListUnscheduledVMs(ctx context.Context, limit int) ([]store.VM, error)
	BindScheduledVM(ctx context.Context, vmID uuid.UUID, plan func(store.PlacementReader) (store.VMBindWrites, error)) error
	UpdateVMSchedulingReason(ctx context.Context, vmID uuid.UUID, reason, message string, details []byte) error
}

// ScheduleConfig threads the placement knobs (mirrors the create handler's).
type ScheduleConfig struct {
	Algorithm string
}

// scheduleBatchLimit caps the number of unscheduled VMs a single tick binds, so
// a large pending backlog does not stall one reconcile pass; the rest are picked
// up on the next tick.
const scheduleBatchLimit = 100

// diskBytesPerGiB converts the requested disk_gib to the bytes the scheduler's
// disk fit check consumes.
const diskBytesPerGiB = 1073741824

// errNetworkNotFound is the in-flight signal that the deferred network name on a
// VM's SchedulingSpec does not resolve to a live network. schedulingReasonFor
// maps it to SchedReasonNetworkNotFound; the VM stays pending.
var errNetworkNotFound = errors.New("requested network not found")

// ScheduleFunc returns the periodic body for the vms.schedule loop. Each tick
// scans unscheduled VMs and tries to bind each via scheduler.SchedulePlacement;
// success commits the bind (BindScheduledVM), unschedulable records a reason and
// leaves the VM pending for the next tick. It never fails a VM: there is no
// server-side deadline. A per-VM error is logged and the loop continues.
func ScheduleFunc(st ScheduleStore, cfg ScheduleConfig, log *slog.Logger, res scheduler.ResourcesConfig) func(context.Context) error {
	return func(ctx context.Context) error {
		vmsList, err := st.ListUnscheduledVMs(ctx, scheduleBatchLimit)
		if err != nil {
			return fmt.Errorf("list unscheduled vms: %v", err)
		}
		for _, vm := range vmsList {
			scheduleOne(ctx, st, cfg, log, res, vm)
		}
		return nil
	}
}

// scheduleOne binds a single unscheduled VM or records why it stays pending. A
// lost CAS (bound by another replica, or deleted) is a clean skip, not an error
// - it must never resurrect the VM or clobber the winning state.
func scheduleOne(ctx context.Context, st ScheduleStore, cfg ScheduleConfig, log *slog.Logger, res scheduler.ResourcesConfig, vm store.VM) {
	spec, err := store.UnmarshalSchedulingSpec(vm.SchedulingSpec)
	if err != nil {
		log.ErrorContext(ctx, "decode scheduling spec", "vm_id", vm.ID, "error", err)
		return
	}

	bindErr := bindWithMACRetry(ctx, st, vm, spec, cfg, res)
	switch {
	case bindErr == nil:
		log.InfoContext(ctx, "vm scheduled", "vm_id", vm.ID, "name", vm.Name)
	case errors.Is(bindErr, store.ErrVMNotUnscheduled), errors.Is(bindErr, store.ErrNotFound):
		// Bound by another replica or deleted concurrently - nothing to do.
	default:
		reason, msg, details := schedulingReasonFor(bindErr, spec)
		if uerr := st.UpdateVMSchedulingReason(ctx, vm.ID, reason, msg, details); uerr != nil {
			log.ErrorContext(ctx, "update scheduling reason", "vm_id", vm.ID, "error", uerr)
		}
	}
}

// bindWithMACRetry runs SchedulePlacement + BindScheduledVM, re-minting a NIC
// MAC on a per-network collision (mirrors the former createWithMACRetry). The
// plan callback re-runs each attempt, so a fresh MAC is generated on retry.
func bindWithMACRetry(ctx context.Context, st ScheduleStore, vm store.VM, spec store.SchedulingSpec, cfg ScheduleConfig, res scheduler.ResourcesConfig) error {
	// Serialize the placement decision window against all other placements so
	// concurrent binds spread across nodes. The lock MUST wrap BindScheduledVM,
	// not just the plan callback: BindScheduledVM commits the pin AFTER the
	// callback returns, so releasing at callback-return would reopen the race.
	// Deliberately held across the MAC-retry loop: a collision re-mints a fresh
	// random MAC, so retries are rare and each is one fast local CAS (<=8). If a
	// future backend makes acquire/commit expensive (the HA etcd lock), revisit
	// to a per-attempt acquire so one VM's retries do not serialize all placements.
	release, err := st.AcquirePlacementLock(ctx, store.LockKeyPlacement)
	if err != nil {
		return fmt.Errorf("acquire placement lock: %v", err)
	}
	defer release()

	const maxMACRetries = 8
	var lastErr error
	for i := 0; i < maxMACRetries; i++ {
		lastErr = st.BindScheduledVM(ctx, vm.ID, func(pr store.PlacementReader) (store.VMBindWrites, error) {
			return planBind(ctx, pr, vm, spec, cfg, res)
		})
		if !errors.Is(lastErr, store.ErrVMNicMACConflict) {
			return lastErr
		}
	}
	return lastErr
}

// planBind runs placement and builds the bind writes - the moved back-half of
// the old admission-time scheduleAndEnqueueCreate, now reading from the VM row +
// SchedulingSpec. It resolves the deferred network name, scores a (node, pool)
// via scheduler.SchedulePlacement, and assembles the disk / optional NIC / task
// / job for the placement-locked commit.
func planBind(ctx context.Context, pr store.PlacementReader, vm store.VM, spec store.SchedulingSpec, cfg ScheduleConfig, res scheduler.ResourcesConfig) (store.VMBindWrites, error) {
	networkIDs, nicNetworkID, err := resolveBindNetwork(ctx, pr, spec)
	if err != nil {
		return store.VMBindWrites{}, err
	}

	decision, err := scheduler.SchedulePlacement(ctx, pr, scheduler.PlacementRequest{
		PoolName:   spec.PoolName,
		NodeHint:   spec.NodeHint,
		VCPUs:      int(vm.CpuCores),
		MemoryMiB:  int(vm.MemoryMib),
		DiskBytes:  int64(spec.DiskGiB) * diskBytesPerGiB,
		NetworkIDs: networkIDs,
	}, scheduler.PlacementConfig{Algorithm: cfg.Algorithm, Resources: res})
	if err != nil {
		return store.VMBindWrites{}, err
	}
	if decision.PoolInstance.Type != "local_dir" {
		return store.VMBindWrites{}, &poolNotWritableError{poolType: decision.PoolInstance.Type}
	}

	nic, err := buildBindNIC(vm, nicNetworkID)
	if err != nil {
		return store.VMBindWrites{}, err
	}

	taskID := uuid.New()
	resID := vm.ID
	owner := vm.OwnerID
	argsMap := map[string]any{
		"vm_id":   vm.ID.String(),
		"pool_id": decision.PoolInstance.ID.String(),
		"node_id": decision.Node.ID.String(),
	}
	if vm.SourceSnapshotID != nil {
		argsMap["source_snapshot_id"] = vm.SourceSnapshotID.String()
	}
	argsJSON, _ := json.Marshal(argsMap)

	return store.VMBindWrites{
		PinnedNodeID: decision.Node.ID,
		Disk: store.CreateVMDiskParams{
			VmID: vm.ID, StoragePoolID: decision.PoolInstance.ID, DeviceOrder: 0,
			Bus: store.DiskBusVirtio, SizeGib: spec.DiskGiB, SourceKind: "image",
			Format: vm.ImageFormat, ReadOnly: false,
			CacheMode: store.DiskCacheModeWriteback, Discard: store.DiskDiscardUnmap,
		},
		Nic: nic,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.create", Status: store.TaskStatusPending,
			ResourceType: "vm", ResourceID: &resID, Args: argsJSON,
			MaxAttempts: 25, CreatedBy: &owner,
		},
		Job: VMCreateArgs{
			TaskID: taskID, VMID: vm.ID, PoolID: decision.PoolInstance.ID, NodeID: decision.Node.ID,
			SourceSnapshotID: vm.SourceSnapshotID,
		},
	}, nil
}

// resolveBindNetwork resolves the deferred network name on the spec (captured at
// admission) to its id, so the scheduler's network-aware filter can run and the
// NIC can be minted. A missing network surfaces as errNetworkNotFound (a pending
// reason). When the spec names no network the VM gets no NIC (legacy SLIRP).
func resolveBindNetwork(ctx context.Context, pr store.PlacementReader, spec store.SchedulingSpec) (networkIDs []uuid.UUID, nicNetworkID *uuid.UUID, err error) {
	if spec.NetworkName == nil || *spec.NetworkName == "" {
		return nil, nil, nil
	}
	net, lerr := pr.NetworkByName(ctx, *spec.NetworkName)
	if lerr != nil {
		if errors.Is(lerr, store.ErrNotFound) {
			return nil, nil, errNetworkNotFound
		}
		return nil, nil, fmt.Errorf("resolve network: %v", lerr)
	}
	id := net.ID
	return []uuid.UUID{id}, &id, nil
}

// buildBindNIC mints the NIC row for the bind when the VM attaches to a network.
// Returns nil when there is no network (the disk-only bind). The MAC is minted
// fresh so bindWithMACRetry can re-run planBind on a per-network MAC collision.
func buildBindNIC(vm store.VM, nicNetworkID *uuid.UUID) (*store.CreateVMNicParams, error) {
	if nicNetworkID == nil {
		return nil, nil
	}
	mac, err := netid.GenerateLocalMAC()
	if err != nil {
		return nil, fmt.Errorf("generate nic mac: %v", err)
	}
	return &store.CreateVMNicParams{
		ID: uuid.New(), VmID: vm.ID, NetworkID: *nicNetworkID,
		DeviceOrder: 0, Model: store.NicModelVirtio, MacAddress: mac,
	}, nil
}

// schedulingReasonFor maps a bind error to the (reason, message, details) the
// loop records on the still-pending VM. The mapped branches cover every
// scheduler sentinel that can reach the loop: ErrPoolNotFound,
// ErrInsufficientResources, ErrNoEligibleNodes (refined into network/pressure),
// and ErrPoolNotOnNode, plus the local errNetworkNotFound, the store-side
// ErrSubnetExhausted (IPAM full), and the non-writable-pool case. ErrNodeHintIsUUID is rejected at admission (a uuid
// node hint never reaches here), and ErrDefaultPoolNotSet / ErrUnknownAlgorithm
// are admission/config-time. The default fallthrough is pool_not_ready: a
// transient infra error retried on the next tick.
func schedulingReasonFor(err error, spec store.SchedulingSpec) (reason, message string, details []byte) {
	switch {
	case errors.Is(err, errNetworkNotFound):
		name := ""
		if spec.NetworkName != nil {
			name = *spec.NetworkName
		}
		return store.SchedReasonNetworkNotFound, fmt.Sprintf("network %q not found", name), nil
	case errors.Is(err, scheduler.ErrPoolNotFound):
		return store.SchedReasonPoolNotFound, fmt.Sprintf("pool %q not found in cluster", spec.PoolName), nil
	case errors.Is(err, scheduler.ErrPoolNotOnNode):
		hint := ""
		if spec.NodeHint != nil {
			hint = *spec.NodeHint
		}
		return store.SchedReasonPoolNotOnNode, fmt.Sprintf("pool %q not present on node %q", spec.PoolName, hint), nil
	case errors.Is(err, scheduler.ErrInsufficientResources):
		d, _ := scheduler.ExtractInsufficientResources(err)
		return store.SchedReasonInsufficientResources, "no node has sufficient resources", marshalDetail(d)
	case errors.Is(err, scheduler.ErrNoEligibleNodes):
		return noEligibleReasonFor(err)
	case errors.Is(err, store.ErrSubnetExhausted):
		return store.SchedReasonSubnetExhausted, "the network's subnet has no free addresses", nil
	default:
		var pnw *poolNotWritableError
		if errors.As(err, &pnw) {
			return store.SchedReasonPoolNotWritable, fmt.Sprintf("pool type %q not writable", pnw.poolType), nil
		}
		return store.SchedReasonPoolNotReady, "pool not ready", nil
	}
}

// noEligibleReasonFor refines an ErrNoEligibleNodes chain into the most specific
// pending reason: a network-unready detail -> network_not_ready, a node-pressure
// detail -> node_pressure, else the bare no_eligible_nodes.
func noEligibleReasonFor(err error) (reason, message string, details []byte) {
	if d, ok := scheduler.ExtractNetworkUnreadyDetail(err); ok {
		return store.SchedReasonNetworkNotReady, "requested network not ready on any node", marshalDetail(d)
	}
	if d, ok := scheduler.ExtractNodePressureDetail(err); ok {
		return store.SchedReasonNodePressure, "all candidate nodes under pressure", marshalDetail(d)
	}
	return store.SchedReasonNoEligibleNodes, "no eligible node available", nil
}

// marshalDetail JSON-encodes a structured scheduler detail for the VM's
// SchedulingDetails column; nil detail -> nil bytes.
func marshalDetail(v any) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}
