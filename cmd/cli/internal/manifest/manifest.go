// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package manifest parses, validates, and projects otherix CLI YAML
// manifests. It is pure (no network) so the apply/delete fan-out
// commands stay thin orchestrators over cpclient. The supported kinds
// are Network, StoragePool, and VM; a spec maps to the matching
// cpclient create-request body.
package manifest

import "gopkg.in/yaml.v3"

// APIVersionV1 is the only accepted apiVersion in v1.
const APIVersionV1 = "otherix/v1"

// Kind names, matching the manifest `kind:` field exactly (case-sensitive).
const (
	KindNetwork     = "Network"
	KindStoragePool = "StoragePool"
	KindVM          = "VM"
)

// Document is one parsed manifest document. Spec is held as a raw YAML
// node so the per-kind decoder can both populate a typed struct and
// enumerate keys for unknown-field rejection.
type Document struct {
	Index      int       // 0-based position in the input stream, for error messages
	APIVersion string    // the apiVersion field
	Kind       string    // one of the Kind* constants
	Name       string    // metadata.name
	Spec       yaml.Node // the spec mapping node
}

// NetworkSpec is the spec body for kind Network. Fields mirror
// cpclient.CreateNetworkParams; MTU/VLAN are pointers so an omitted
// value lands as nil and the server applies its default.
type NetworkSpec struct {
	Type       string `yaml:"type"`
	BridgeName string `yaml:"bridgeName"`
	Managed    bool   `yaml:"managed"`
	Egress     string `yaml:"egress"`
	Subnet     string `yaml:"subnet"`
	Gateway    string `yaml:"gateway"`
	MTU        *int   `yaml:"mtu"`
	VLAN       *int   `yaml:"vlan"`
}

// StoragePoolSpec is the spec body for kind StoragePool. Node and
// NodeList are mutually exclusive; NodeList expands to one instance
// per node. NodeSelector is reserved for a future labels-based form
// and is rejected if set in v1.
type StoragePoolSpec struct {
	Type     string   `yaml:"type"`
	Path     string   `yaml:"path"`
	Node     string   `yaml:"node"`
	NodeList []string `yaml:"nodeList"`
}

// VMSpec is the spec body for kind VM. Required fields are ImageURL
// and Arch; everything else is optional with server/CLI defaults.
// CloudInit is the inline cloud-config payload (maps to user_data on
// the wire); CloudInitDisabled is mutually exclusive with it.
type VMSpec struct {
	ImageURL          string `yaml:"imageURL"`
	ImageSHA256       string `yaml:"imageSHA256"`
	Arch              string `yaml:"arch"`
	Firmware          string `yaml:"firmware"`
	FirmwareID        string `yaml:"firmwareID"`
	Format            string `yaml:"format"`
	DiskGiB           int    `yaml:"diskGiB"`
	VCPUs             int    `yaml:"vcpus"`
	MemoryMB          int    `yaml:"memoryMB"`
	Pool              string `yaml:"pool"`
	Network           string `yaml:"network"`
	Node              string `yaml:"node"`
	DesiredPhase      string `yaml:"desiredPhase"`
	CloudInit         string `yaml:"cloudInit"`
	CloudInitDisabled bool   `yaml:"cloudInitDisabled"`
}
