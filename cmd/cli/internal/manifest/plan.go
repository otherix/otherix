// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
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
}

// DeleteTarget is one resolved delete target. For StoragePool, PoolNode
// carries the node so the command layer can resolve the (node, name)
// instance UUID before calling DeletePool.
type DeleteTarget struct {
	Kind     string
	Name     string
	PoolNode string // StoragePool only
}

// kindOrder is the create precedence: a VM references a pool and a
// network by name, so those must exist first. Delete uses the reverse.
var kindOrder = map[string]int{
	KindNetwork:     0,
	KindStoragePool: 1,
	KindVM:          2,
}

// BuildCreatePlan validates every document, expands StoragePool
// nodeList into per-node operations, and returns the create operations
// sorted Network -> StoragePool -> VM. Document order is preserved
// within a kind (stable sort).
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
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		return kindOrder[targets[i].Kind] > kindOrder[targets[j].Kind] // reverse
	})
	return targets, nil
}

// poolNodes returns the node list for a pool spec (single node or the
// explicit nodeList).
func poolNodes(s StoragePoolSpec) []string {
	if len(s.NodeList) > 0 {
		return s.NodeList
	}
	return []string{s.Node}
}

func networkCreateOp(d Document) (CreateOp, error) {
	s, err := DecodeNetworkSpec(d)
	if err != nil {
		return CreateOp{}, err
	}
	params := cpclient.CreateNetworkParams{
		Name:       d.Name,
		Type:       s.Type,
		BridgeName: s.BridgeName,
		Managed:    s.Managed,
		Egress:     s.Egress,
		Subnet:     s.Subnet,
		Gateway:    s.Gateway,
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

func vmCreateOp(d Document) (CreateOp, error) {
	s, err := DecodeVMSpec(d)
	if err != nil {
		return CreateOp{}, err
	}
	req := cpclient.CreateVMRequest{
		Name:              d.Name,
		ImageURL:          s.ImageURL,
		ImageSHA256:       s.ImageSHA256,
		Arch:              s.Arch,
		Firmware:          s.Firmware,
		FirmwareID:        s.FirmwareID,
		Format:            s.Format,
		DiskGiB:           s.DiskGiB,
		Pool:              s.Pool,
		Network:           s.Network,
		VCPUs:             s.VCPUs,
		MemoryMB:          s.MemoryMB,
		CloudInitDisabled: s.CloudInitDisabled,
	}
	if s.Node != "" {
		node := s.Node
		req.Node = &node
	}
	if s.CloudInit != "" {
		ci := s.CloudInit
		req.UserData = &ci
	}
	return CreateOp{Kind: KindVM, Name: d.Name, VM: &req}, nil
}
