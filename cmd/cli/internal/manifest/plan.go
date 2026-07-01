// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
	"fmt"
	"sort"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// CreateOp is one resolved create operation. Exactly one of the
// payload pointers is non-nil, selected by Kind. Name is the
// resource's manifest name (for reporting).
type CreateOp struct {
	Kind    string
	Name    string
	Network *cpclient.CreateNetworkParams
	Pool    *cpclient.CreatePoolRequest
	VM      *cpclient.CreateVMRequest
	LB      *cpclient.CreateLoadBalancerParams
}

// DeleteTarget is one resolved delete target. For StoragePool, PoolNode
// carries the node so the command layer can resolve the (node, name)
// instance UUID before calling DeletePool.
type DeleteTarget struct {
	Kind     string
	Name     string
	PoolNode string // StoragePool only
}

// kindOrder gives a stable, deterministic apply order (Network ->
// StoragePool -> VM) for readable logs. It is no longer load-bearing for
// correctness on create: VM admission defers pool/network resolution and
// never fails on a missing or not-yet-ready dependency, so the order in
// which the documents are submitted does not matter. Delete still relies
// on the reverse order (tear down VMs before the pools/networks they use).
var kindOrder = map[string]int{
	KindNetwork:      0,
	KindStoragePool:  1,
	KindVM:           2,
	KindLoadBalancer: 3,
}

// BuildCreatePlan validates every document, expands StoragePool
// nodeList into per-node operations, and returns the create operations
// in a stable Network -> StoragePool -> VM order. The ordering is
// cosmetic (deterministic logs), not a correctness requirement: VM
// admission defers pool/network references, so a manifest listing a VM
// before its pool/network applies without the VM create failing.
// Document order is preserved within a kind (stable sort).
func BuildCreatePlan(docs []Document) ([]CreateOp, error) {
	var ops []CreateOp
	for _, d := range docs {
		// Kinds are gated by decodeHeader (parse.go); an unknown kind
		// cannot reach here, so the switch needs no default.
		switch d.Kind {
		case KindNetwork:
			op, err := networkCreateOp(d)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		case KindStoragePool:
			poolOps, err := poolCreateOps(d)
			if err != nil {
				return nil, err
			}
			ops = append(ops, poolOps...)
		case KindVM:
			op, err := vmCreateOp(d)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		case KindLoadBalancer:
			op, err := loadBalancerCreateOp(d)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		}
	}
	sort.SliceStable(ops, func(i, j int) bool {
		return kindOrder[ops[i].Kind] < kindOrder[ops[j].Kind]
	})
	return ops, nil
}

// BuildDeletePlan validates every document and returns delete targets
// in reverse create order (VM -> StoragePool -> Network). A StoragePool
// with nodeList expands to one target per node.
func BuildDeletePlan(docs []Document) ([]DeleteTarget, error) {
	var targets []DeleteTarget
	for _, d := range docs {
		// Kinds are gated by decodeHeader (parse.go); an unknown kind
		// cannot reach here, so the switch needs no default.
		switch d.Kind {
		case KindNetwork:
			if _, err := DecodeNetworkSpec(d); err != nil {
				return nil, err
			}
			targets = append(targets, DeleteTarget{Kind: KindNetwork, Name: d.Name})
		case KindStoragePool:
			s, err := DecodeStoragePoolSpec(d)
			if err != nil {
				return nil, err
			}
			for _, node := range poolNodes(s) {
				targets = append(targets, DeleteTarget{Kind: KindStoragePool, Name: d.Name, PoolNode: node})
			}
		case KindVM:
			if _, err := DecodeVMSpec(d); err != nil {
				return nil, err
			}
			targets = append(targets, DeleteTarget{Kind: KindVM, Name: d.Name})
		case KindLoadBalancer:
			if _, err := DecodeLoadBalancerSpec(d); err != nil {
				return nil, err
			}
			targets = append(targets, DeleteTarget{Kind: KindLoadBalancer, Name: d.Name})
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		return kindOrder[targets[i].Kind] > kindOrder[targets[j].Kind] // reverse
	})
	return targets, nil
}

// poolNodes returns the deduplicated node list for a pool spec (single
// node or the explicit nodeList). Duplicates are dropped preserving
// first-seen order so a copy-paste typo in nodeList cannot turn one
// declared instance into a duplicate create that 409s mid-apply.
func poolNodes(s StoragePoolSpec) []string {
	if len(s.NodeList) == 0 {
		return []string{s.Node}
	}
	seen := make(map[string]bool, len(s.NodeList))
	var out []string
	for _, n := range s.NodeList {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func networkCreateOp(d Document) (CreateOp, error) {
	s, err := DecodeNetworkSpec(d)
	if err != nil {
		return CreateOp{}, err
	}
	// type is mandatory for a create (the server enum is [bridge, overlay]).
	// DecodeNetworkSpec is shared with the delete path, which needs only the
	// name, so the required-field check lives here on the create path -
	// mirroring the path/imageURL guards for StoragePool/VM. This also
	// catches a typo'd top-level spec key, which decodes to an empty spec.
	if s.Type == "" {
		return CreateOp{}, fmt.Errorf("manifest: document %d (Network/%s): spec.type is required", d.Index, d.Name)
	}
	params := cpclient.CreateNetworkParams{
		Name:       d.Name,
		Type:       s.Type,
		BridgeName: s.BridgeName,
		Managed:    s.Managed,
		Egress:     s.Egress,
		Subnet:     s.Subnet,
		Gateway:    s.Gateway,
		Dhcp:       s.Dhcp,
		DNS:        s.Dns,
		Mtu:        s.MTU,
		VlanTag:    s.VLAN,
	}
	return CreateOp{Kind: KindNetwork, Name: d.Name, Network: &params}, nil
}

func poolCreateOps(d Document) ([]CreateOp, error) {
	s, err := DecodeStoragePoolSpec(d)
	if err != nil {
		return nil, err
	}
	var ops []CreateOp
	for _, node := range poolNodes(s) {
		req := cpclient.CreatePoolRequest{
			Node: node,
			Name: d.Name,
			Type: s.Type,
			Path: s.Path,
		}
		ops = append(ops, CreateOp{Kind: KindStoragePool, Name: d.Name, Pool: &req})
	}
	return ops, nil
}

// defaultVMVCPUs and defaultVMMemoryMB mirror the `vm create` flag
// defaults (cmd/cli/vm/create.go). The wire fields have no omitempty and
// the server rejects 0, so an omitted vcpus/memoryMB must materialise to
// these client-side just as the flag path does. Keep in sync with the
// flag defaults.
const (
	defaultVMVCPUs    = 2
	defaultVMMemoryMB = 2048
)

func vmCreateOp(d Document) (CreateOp, error) {
	s, err := DecodeVMSpec(d)
	if err != nil {
		return CreateOp{}, err
	}
	vcpus := s.VCPUs
	if vcpus == 0 {
		vcpus = defaultVMVCPUs
	}
	memoryMB := s.MemoryMB
	if memoryMB == 0 {
		memoryMB = defaultVMMemoryMB
	}
	req := cpclient.CreateVMRequest{
		Name:              d.Name,
		Pool:              s.Pool,
		Network:           s.Network,
		VCPUs:             vcpus,
		MemoryMB:          memoryMB,
		CloudInitDisabled: s.CloudInitDisabled,
		SSHIngressEnabled: s.SSHIngressEnabled,
	}
	if s.SourceSnapshotID != "" {
		// Snapshot-sourced: the snapshot manifest is authoritative for
		// architecture / firmware / format / image digest, and the server rejects
		// them in the request (field_from_snapshot). Drop them so a `vm get -o yaml`
		// round-trip (which emits arch) applies cleanly.
		req.SourceSnapshotID = s.SourceSnapshotID
	} else {
		req.ImageURL = s.ImageURL
		req.ImageSHA256 = s.ImageSHA256
		req.Arch = s.Arch
		req.Firmware = s.Firmware
		req.FirmwareID = s.FirmwareID
		req.Format = s.Format
		req.ImagePullPolicy = s.ImagePullPolicy
		req.DiskGiB = s.DiskGiB
	}
	if s.Node != "" {
		node := s.Node
		req.Node = &node
	}
	if s.UserData != "" {
		ud := s.UserData
		req.UserData = &ud
	}
	if s.NetworkConfig != "" {
		nc := s.NetworkConfig
		req.NetworkConfig = &nc
	}
	return CreateOp{Kind: KindVM, Name: d.Name, VM: &req}, nil
}

func loadBalancerCreateOp(d Document) (CreateOp, error) {
	s, err := DecodeLoadBalancerSpec(d)
	if err != nil {
		return CreateOp{}, err
	}
	params := cpclient.CreateLoadBalancerParams{
		Name:     d.Name,
		Port:     int32(s.Port), //nolint:gosec // port validated in 1..65535 by DecodeLoadBalancerSpec.
		Selector: s.Selector,
	}
	return CreateOp{Kind: KindLoadBalancer, Name: d.Name, LB: &params}, nil
}
