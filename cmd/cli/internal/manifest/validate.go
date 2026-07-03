// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// resolveAlias follows a YAML alias node to its anchored target so the
// spec can be validated and decoded as the mapping it refers to. A
// non-alias node is returned unchanged.
func resolveAlias(n yaml.Node) yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = *n.Alias
	}
	return n
}

// specKeys returns the set of YAML keys present in a spec node. Used
// for unknown-field rejection: any key outside a kind's allow-set is a
// validation error, so a typo (e.g. `bridge_name` vs `bridgeName`) is
// caught at the CLI edge rather than silently dropped.
func specKeys(spec yaml.Node) []string {
	var keys []string
	// A mapping node stores [k0, v0, k1, v1, ...] in Content.
	for i := 0; i+1 < len(spec.Content); i += 2 {
		keys = append(keys, spec.Content[i].Value)
	}
	return keys
}

// rejectUnknownKeys returns an error naming the first spec key that is
// not in allowed.
func rejectUnknownKeys(d Document, allowed map[string]bool) error {
	spec := resolveAlias(d.Spec)
	if spec.Kind != 0 && spec.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest: document %d (%s/%s): spec must be a mapping", d.Index, d.Kind, d.Name)
	}
	for _, k := range specKeys(spec) {
		if !allowed[k] {
			return fmt.Errorf("manifest: document %d (%s/%s): unknown spec field %q", d.Index, d.Kind, d.Name, k)
		}
	}
	return nil
}

var networkSpecKeys = map[string]bool{
	"type": true, "bridgeName": true, "managed": true, "egress": true,
	"subnet": true, "gateway": true, "dhcp": true, "dns": true, "mtu": true, "vlan": true,
}

// DecodeNetworkSpec decodes and validates a Network document's spec.
func DecodeNetworkSpec(d Document) (NetworkSpec, error) {
	if err := rejectUnknownKeys(d, networkSpecKeys); err != nil {
		return NetworkSpec{}, err
	}
	var s NetworkSpec
	if err := d.Spec.Decode(&s); err != nil {
		return NetworkSpec{}, fmt.Errorf("manifest: document %d (Network/%s): spec: %v", d.Index, d.Name, err)
	}
	// bridgeName is the host bridge identifier; padding it would create a
	// bogus interface name, so trim like the StoragePool node fields.
	s.BridgeName = strings.TrimSpace(s.BridgeName)
	return s, nil
}

var storagePoolSpecKeys = map[string]bool{
	"type": true, "path": true, "node": true, "nodeList": true,
}

// DecodeStoragePoolSpec decodes and validates a StoragePool document's
// spec. `path` is required; exactly one of `node` / `nodeList` must be
// set (the (node, name) pair is the per-instance identity).
func DecodeStoragePoolSpec(d Document) (StoragePoolSpec, error) {
	if err := rejectUnknownKeys(d, storagePoolSpecKeys); err != nil {
		return StoragePoolSpec{}, err
	}
	var s StoragePoolSpec
	if err := d.Spec.Decode(&s); err != nil {
		return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): spec: %v", d.Index, d.Name, err)
	}
	if s.Path == "" {
		return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): spec.path is required", d.Index, d.Name)
	}
	// Trim node values so whitespace-only entries are treated as empty:
	// a padded `node` collapses to the one-of-node/nodeList error, and a
	// padded nodeList element triggers the empty-element error below.
	s.Node = strings.TrimSpace(s.Node)
	for i := range s.NodeList {
		s.NodeList[i] = strings.TrimSpace(s.NodeList[i])
	}
	hasNode, hasList := s.Node != "", len(s.NodeList) > 0
	switch {
	case hasNode && hasList:
		return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): spec.node and spec.nodeList are mutually exclusive", d.Index, d.Name)
	case !hasNode && !hasList:
		return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): one of spec.node or spec.nodeList is required", d.Index, d.Name)
	}
	for _, n := range s.NodeList {
		if n == "" {
			return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): spec.nodeList contains an empty node name", d.Index, d.Name)
		}
	}
	return s, nil
}

var loadBalancerSpecKeys = map[string]bool{
	"port": true, "selector": true, "healthCheck": true,
	"publishedPort": true, "protocol": true, "sourceCIDRs": true,
}

// DecodeLoadBalancerSpec decodes and validates a LoadBalancer document's
// spec. port must be in 1..65535 and selector must be non-empty with a
// non-empty key AND value on every entry - mirroring the CLI --selector
// rejection (lb.parseSelector) so a `selector: {app: ""}` manifest is
// caught at the CLI edge, matching the server-side admission.
func DecodeLoadBalancerSpec(d Document) (LoadBalancerSpec, error) {
	if err := rejectUnknownKeys(d, loadBalancerSpecKeys); err != nil {
		return LoadBalancerSpec{}, err
	}
	var s LoadBalancerSpec
	if err := d.Spec.Decode(&s); err != nil {
		return LoadBalancerSpec{}, fmt.Errorf("manifest: document %d (LoadBalancer/%s): spec: %v", d.Index, d.Name, err)
	}
	if s.Port < 1 || s.Port > 65535 {
		return LoadBalancerSpec{}, fmt.Errorf("manifest: document %d (LoadBalancer/%s): spec.port must be in 1..65535", d.Index, d.Name)
	}
	if len(s.Selector) == 0 {
		return LoadBalancerSpec{}, fmt.Errorf("manifest: document %d (LoadBalancer/%s): spec.selector must have at least one key=value", d.Index, d.Name)
	}
	for k, v := range s.Selector {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return LoadBalancerSpec{}, fmt.Errorf("manifest: document %d (LoadBalancer/%s): spec.selector entries must have a non-empty key and value", d.Index, d.Name)
		}
	}
	if s.PublishedPort != nil && (*s.PublishedPort < 1 || *s.PublishedPort > 65535) {
		return LoadBalancerSpec{}, fmt.Errorf("manifest: document %d (LoadBalancer/%s): spec.publishedPort must be in 1..65535", d.Index, d.Name)
	}
	return s, nil
}

var vmSpecKeys = map[string]bool{
	"imageURL": true, "sourceSnapshotID": true, "imageSHA256": true,
	"arch": true, "firmware": true, "firmwareID": true, "format": true,
	"imagePullPolicy": true,
	"diskGiB":         true, "vcpus": true, "memoryMB": true, "pool": true,
	"network": true, "node": true, "userData": true, "networkConfig": true,
	"cloudInitDisabled": true, "sshIngressEnabled": true,
	"labels": true,
}

// DecodeVMSpec decodes and validates a VM document's spec. Exactly one
// of imageURL or sourceSnapshotID must be set; arch is required only on
// the image path (the snapshot manifest is authoritative for arch).
// firmware/firmwareID are mutually exclusive, and userData and
// networkConfig are each mutually exclusive with cloudInitDisabled.
func DecodeVMSpec(d Document) (VMSpec, error) {
	if err := rejectUnknownKeys(d, vmSpecKeys); err != nil {
		return VMSpec{}, err
	}
	var s VMSpec
	if err := d.Spec.Decode(&s); err != nil {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec: %v", d.Index, d.Name, err)
	}
	// node/pool/network are identity references; a padded value must not
	// leak into the (node, name) placement key or a resource lookup. Trim
	// like StoragePool, so whitespace-only collapses to unset.
	s.Node = strings.TrimSpace(s.Node)
	s.Pool = strings.TrimSpace(s.Pool)
	s.Network = strings.TrimSpace(s.Network)
	// Exactly one of imageURL / sourceSnapshotID (mirrors the server's
	// image_xor_snapshot admission). For the image path arch is required; for
	// the snapshot path architecture/firmware/format come from the snapshot
	// manifest and must NOT be supplied (the server rejects them), so they are
	// dropped at request-build time (see vmCreateOp).
	hasImage := s.ImageURL != ""
	hasSnapshot := s.SourceSnapshotID != ""
	if hasImage == hasSnapshot {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec: exactly one of imageURL or sourceSnapshotID is required", d.Index, d.Name)
	}
	if hasImage && s.Arch == "" {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.arch is required", d.Index, d.Name)
	}
	if s.Firmware != "" && s.FirmwareID != "" {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.firmware and spec.firmwareID are mutually exclusive", d.Index, d.Name)
	}
	if s.UserData != "" && s.CloudInitDisabled {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.userData and spec.cloudInitDisabled are mutually exclusive", d.Index, d.Name)
	}
	if s.NetworkConfig != "" && s.CloudInitDisabled {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.networkConfig and spec.cloudInitDisabled are mutually exclusive", d.Index, d.Name)
	}
	if s.SSHIngressEnabled && s.CloudInitDisabled {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.sshIngressEnabled and spec.cloudInitDisabled are mutually exclusive", d.Index, d.Name)
	}
	return s, nil
}
