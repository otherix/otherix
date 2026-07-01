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
	KindNetwork      = "Network"
	KindStoragePool  = "StoragePool"
	KindVM           = "VM"
	KindLoadBalancer = "LoadBalancer"
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
	Dhcp       bool   `yaml:"dhcp"`
	// Dns is a pointer so an omitted value (nil) lets the server apply its
	// default (for a managed bridge dns follows dhcp; for an overlay dns
	// defaults on), while an explicit dns: true/false is sent verbatim.
	Dns  *bool `yaml:"dns"`
	MTU  *int  `yaml:"mtu"`
	VLAN *int  `yaml:"vlan"`
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
// UserData is the inline cloud-config payload (maps to user_data on
// the wire) and NetworkConfig is the inline cloud-init network-config
// payload (maps to network_config); both are mutually exclusive with
// CloudInitDisabled.
type VMSpec struct {
	ImageURL string `yaml:"imageURL"`
	// SourceSnapshotID recreates the VM from a snapshot instead of an image
	// (the snapshot-source create mode). Mutually exclusive with imageURL;
	// when set, architecture / format / firmware come from the snapshot
	// manifest. Surfaced so a snapshot-sourced VM's `get -o yaml` round-trips
	// its origin instead of collapsing to an empty imageURL.
	SourceSnapshotID string `yaml:"sourceSnapshotID"`
	ImageSHA256      string `yaml:"imageSHA256"`
	Arch             string `yaml:"arch"`
	Firmware         string `yaml:"firmware"`
	FirmwareID       string `yaml:"firmwareID"`
	Format           string `yaml:"format"`
	// ImagePullPolicy mirrors the API image_pull_policy ("if_not_present" |
	// "always"). Surfaced so `get -o yaml` round-trips it through `create -f`.
	ImagePullPolicy   string `yaml:"imagePullPolicy"`
	DiskGiB           int    `yaml:"diskGiB"`
	VCPUs             int    `yaml:"vcpus"`
	MemoryMB          int    `yaml:"memoryMB"`
	Pool              string `yaml:"pool"`
	Network           string `yaml:"network"`
	Node              string `yaml:"node"`
	UserData          string `yaml:"userData"`
	NetworkConfig     string `yaml:"networkConfig"`
	CloudInitDisabled bool   `yaml:"cloudInitDisabled"`
	// SSHIngressEnabled mirrors the API ssh_ingress_enabled per-VM opt-in.
	// Mutually exclusive with CloudInitDisabled. Create-time only; like
	// CloudInitDisabled the live VM view does not surface it, so it is
	// manifest-input-expressible but not emitted by `get -o yaml`.
	SSHIngressEnabled bool `yaml:"sshIngressEnabled"`
}

// LoadBalancerSpec is the spec body for kind LoadBalancer. Both fields
// mirror cpclient.CreateLoadBalancerParams and are required by the server:
// Port is the guest TCP port ingress connections target, and Selector is
// the non-empty label set (every key and value non-empty) selecting the
// backend VMs.
type LoadBalancerSpec struct {
	Port     int               `yaml:"port"`
	Selector map[string]string `yaml:"selector"`
}
