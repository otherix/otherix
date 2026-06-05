// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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
	for _, k := range specKeys(d.Spec) {
		if !allowed[k] {
			return fmt.Errorf("manifest: document %d (%s/%s): unknown spec field %q", d.Index, d.Kind, d.Name, k)
		}
	}
	return nil
}

var networkSpecKeys = map[string]bool{
	"type": true, "bridgeName": true, "managed": true, "egress": true,
	"subnet": true, "gateway": true, "mtu": true, "vlan": true,
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
	hasNode, hasList := s.Node != "", len(s.NodeList) > 0
	switch {
	case hasNode && hasList:
		return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): spec.node and spec.nodeList are mutually exclusive", d.Index, d.Name)
	case !hasNode && !hasList:
		return StoragePoolSpec{}, fmt.Errorf("manifest: document %d (StoragePool/%s): one of spec.node or spec.nodeList is required", d.Index, d.Name)
	}
	return s, nil
}

var vmSpecKeys = map[string]bool{
	"imageURL": true, "imageSHA256": true, "arch": true, "firmware": true,
	"firmwareID": true, "format": true, "diskGiB": true, "vcpus": true,
	"memoryMB": true, "pool": true, "network": true, "node": true,
	"desiredPhase": true, "cloudInit": true, "cloudInitDisabled": true,
}

// DecodeVMSpec decodes and validates a VM document's spec. imageURL and
// arch are required; firmware/firmwareID and cloudInit/cloudInitDisabled
// are mutually exclusive.
func DecodeVMSpec(d Document) (VMSpec, error) {
	if err := rejectUnknownKeys(d, vmSpecKeys); err != nil {
		return VMSpec{}, err
	}
	var s VMSpec
	if err := d.Spec.Decode(&s); err != nil {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec: %v", d.Index, d.Name, err)
	}
	if s.ImageURL == "" {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.imageURL is required", d.Index, d.Name)
	}
	if s.Arch == "" {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.arch is required", d.Index, d.Name)
	}
	if s.Firmware != "" && s.FirmwareID != "" {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.firmware and spec.firmwareID are mutually exclusive", d.Index, d.Name)
	}
	if s.CloudInit != "" && s.CloudInitDisabled {
		return VMSpec{}, fmt.Errorf("manifest: document %d (VM/%s): spec.cloudInit and spec.cloudInitDisabled are mutually exclusive", d.Index, d.Name)
	}
	return s, nil
}
