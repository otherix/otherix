// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedDhcpNetwork creates a type=overlay network with a subnet and
// DhcpEnabled=true - the real supported shape that turns on CP-IPAM in the bind
// txn. Overlay gateways are link-local (169.254.1.1), NOT in the VM subnet, so
// Gateway is nil and the subnet's first host (.1) is a valid VM address. The
// store stamps BridgeName/Managed/Mtu/VNI for overlays; it returns the
// persisted row.
func seedDhcpNetwork(t *testing.T, s *etcdstore.Store, subnet string) store.Network {
	t.Helper()
	p := netip.MustParsePrefix(subnet)
	n, err := s.CreateNetwork(context.Background(), store.CreateNetworkParams{
		ID:          uuid.New(),
		Name:        uniqueNetName("dhcp"),
		Type:        store.NetworkTypeOverlay,
		Egress:      store.NetworkEgressNAT,
		Subnet:      &p,
		Gateway:     nil,
		DhcpEnabled: true,
		Config:      []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("seedDhcpNetwork: %v", err)
	}
	return n
}

// bindVMWithNIC drives the production BindScheduledVM bind txn with a single NIC
// on netID, returning the bound VM id and the NIC params used (so the caller can
// inspect the IP the allocator stamped). The plan mirrors the real scheduler:
// resolve an eligible pool, then bind to the seeded node/pool.
func bindVMWithNIC(t *testing.T, s *etcdstore.Store, name string, nodeID, poolID uuid.UUID, poolName string, netID uuid.UUID, mac string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	vmID, err := s.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, name))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}
	nicID := uuid.New()
	taskID := uuid.New()
	err = s.BindScheduledVM(ctx, vmID, func(pr store.PlacementReader) (store.VMBindWrites, error) {
		pools, perr := pr.ListEligiblePoolsByName(ctx, poolName)
		if perr != nil {
			return store.VMBindWrites{}, perr
		}
		if len(pools) == 0 {
			return store.VMBindWrites{}, fmt.Errorf("no eligible pool")
		}
		return store.VMBindWrites{
			PinnedNodeID: nodeID,
			Disk: store.CreateVMDiskParams{
				VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
				Bus: store.DiskBusVirtio, SizeGib: 0, SourceKind: "image",
				Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeWriteback,
				Discard: store.DiskDiscardUnmap,
			},
			Nic: &store.CreateVMNicParams{
				ID: nicID, VmID: vmID, NetworkID: netID, DeviceOrder: 0,
				Model: store.NicModelVirtio, MacAddress: mustMAC(t, mac),
			},
			Task: store.CreateTaskParams{
				ID: taskID, Type: "vm.create", Status: store.TaskStatusPending,
				ResourceType: "vm", ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 25,
			},
			Job: stubJobArgs{},
		}, nil
	})
	if err != nil {
		t.Fatalf("BindScheduledVM: %v", err)
	}
	return vmID, nicID
}

// TestBindScheduledVMAllocatesNICIPv4 is the allocate SEAM test: it drives the
// real BindScheduledVM bind txn with a NIC on a DHCP-enabled network and asserts
// the bound NIC carries a CP-allocated IPv4 inside the subnet and that the
// reservation guard key was written in the same commit.
func TestBindScheduledVMAllocatesNICIPv4(t *testing.T) {
	st, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	nodeID, poolID, poolName := schedulingFixture(t, st)
	net := seedDhcpNetwork(t, st, "10.62.0.0/24")

	vmID, _ := bindVMWithNIC(t, st, "vm-ipam", nodeID, poolID, poolName, net.ID, "52:54:00:00:00:01")

	nics, err := st.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 1 {
		t.Fatalf("nics = %d, want 1", len(nics))
	}
	nic := nics[0]
	if nic.Ipv4Address == nil {
		t.Fatalf("nic Ipv4Address = nil, want a CP-allocated host in the subnet")
	}
	ip := *nic.Ipv4Address
	if !net.Subnet.Contains(ip) {
		t.Errorf("allocated ip %v not in subnet %v", ip, net.Subnet)
	}
	if want := netip.MustParseAddr("10.62.0.1"); ip != want {
		t.Errorf("allocated ip = %v, want %v (first free host)", ip, want)
	}

	// The reservation guard rode in the same bind txn.
	resKey := etcd.Key("uniq", "vm_nics", "ipv4", net.ID.String(), ip.String())
	if _, found, err := cli.Get(ctx, resKey); err != nil || !found {
		t.Errorf("reservation key %q present=%v err=%v, want present", resKey, found, err)
	}
}

// TestBindScheduledVMSecondNICDistinctIP is the allocate SEAM test for the gap
// scan: a second VM/NIC on the same DHCP network must get the next free host,
// not collide with the first.
func TestBindScheduledVMSecondNICDistinctIP(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	nodeID, poolID, poolName := schedulingFixture(t, st)
	net := seedDhcpNetwork(t, st, "10.62.0.0/24")

	vm1, _ := bindVMWithNIC(t, st, "vm-ipam-a", nodeID, poolID, poolName, net.ID, "52:54:00:00:00:01")
	vm2, _ := bindVMWithNIC(t, st, "vm-ipam-b", nodeID, poolID, poolName, net.ID, "52:54:00:00:00:02")

	ip1 := nicIP(t, st, ctx, vm1)
	ip2 := nicIP(t, st, ctx, vm2)
	if ip1 == ip2 {
		t.Fatalf("both NICs got %v, want distinct addresses", ip1)
	}
	if want := netip.MustParseAddr("10.62.0.1"); ip1 != want {
		t.Errorf("first ip = %v, want %v", ip1, want)
	}
	if want := netip.MustParseAddr("10.62.0.2"); ip2 != want {
		t.Errorf("second ip = %v, want %v", ip2, want)
	}
}

// TestBindScheduledVMHonoursPreSeededReservation proves the CP-IPAM
// reservation guard has teeth WITHOUT relying on real goroutine timing: it
// pre-seeds the reservation key for the IP the allocator would otherwise pick
// first (.1), then drives a real BindScheduledVM. A single bind attempt sees
// the existing reservation in its scan, skips it, and the CreateRevision==0
// guard on the chosen key fixes the next free host (.2) in the same txn. So two
// binds can never share an address: an IP already reserved (whether by a
// committed NIC or by a concurrent bind that won the race) is never re-handed.
// The guard is the backstop for the scan-then-commit window - a bind that lost
// the race fails its CreateRevision compare and the scheduler re-plans on the
// next tick, picking a fresh free IP.
func TestBindScheduledVMHonoursPreSeededReservation(t *testing.T) {
	st, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	nodeID, poolID, poolName := schedulingFixture(t, st)
	net := seedDhcpNetwork(t, st, "10.62.0.0/24")

	// Pre-seed the reservation for .1 (a stand-in for a NIC bound by another
	// replica), as if a concurrent bind already claimed the allocator's first
	// candidate.
	first := netip.MustParseAddr("10.62.0.1")
	preKey := etcd.Key("uniq", "vm_nics", "ipv4", net.ID.String(), first.String())
	if err := cli.Put(ctx, preKey, []byte(uuid.NewString())); err != nil {
		t.Fatalf("pre-seed reservation: %v", err)
	}

	vmID, _ := bindVMWithNIC(t, st, "vm-ipam-guard", nodeID, poolID, poolName, net.ID, "52:54:00:00:00:09")

	// The bind must NOT have re-used the pre-seeded .1; it gets the next free
	// host .2, with its own reservation guard committed in the bind txn.
	ip := nicIP(t, st, ctx, vmID)
	if ip == first {
		t.Fatalf("bind re-used pre-seeded reservation %v, want the next free host", first)
	}
	if want := netip.MustParseAddr("10.62.0.2"); ip != want {
		t.Errorf("bound ip = %v, want %v (first free host past the pre-seeded reservation)", ip, want)
	}
	resKey := etcd.Key("uniq", "vm_nics", "ipv4", net.ID.String(), ip.String())
	if _, found, err := cli.Get(ctx, resKey); err != nil || !found {
		t.Errorf("reservation key %q present=%v err=%v, want present", resKey, found, err)
	}
}

// TestProjectVMDeleteReleasesNICIPv4 is the release SEAM test: after a NIC is
// bound with a CP-allocated IP, the production VM-delete projection
// (ProjectVMDeleteSuccess) must drop the reservation guard so the IP is reusable.
func TestProjectVMDeleteReleasesNICIPv4(t *testing.T) {
	st, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	nodeID, poolID, poolName := schedulingFixture(t, st)
	net := seedDhcpNetwork(t, st, "10.62.0.0/24")

	vmID, _ := bindVMWithNIC(t, st, "vm-ipam-del", nodeID, poolID, poolName, net.ID, "52:54:00:00:00:01")
	ip := nicIP(t, st, ctx, vmID)
	resKey := etcd.Key("uniq", "vm_nics", "ipv4", net.ID.String(), ip.String())
	if _, found, err := cli.Get(ctx, resKey); err != nil || !found {
		t.Fatalf("reservation key before delete present=%v err=%v, want present", found, err)
	}

	vm, err := st.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := st.EnqueueTask(ctx, delTask, stubJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(delete): %v", err)
	}
	if err := st.ProjectVMDeleteSuccess(ctx, vm,
		store.UpdateTaskFinalizedParams{ID: delTask.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess: %v", err)
	}

	// The reservation guard is gone - the IP is reusable.
	if _, found, err := cli.Get(ctx, resKey); err != nil || found {
		t.Errorf("reservation key after delete present=%v err=%v, want dropped", found, err)
	}
}

// nicIP returns the single NIC's allocated IPv4 for vmID, failing the test if
// there is not exactly one NIC with an address.
func nicIP(t *testing.T, st *etcdstore.Store, ctx context.Context, vmID uuid.UUID) netip.Addr {
	t.Helper()
	nics, err := st.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 1 || nics[0].Ipv4Address == nil {
		t.Fatalf("nics = %+v, want exactly one with an Ipv4Address", nics)
	}
	return *nics[0].Ipv4Address
}
