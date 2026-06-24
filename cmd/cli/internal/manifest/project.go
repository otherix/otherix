// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// outDoc is the manifest projection shape. Server-assigned identity fields
// (id, timestamps, owner, reconciliation) are intentionally absent so the
// output is directly re-appliable via `create -f`. Status is the one
// observed-state block we DO surface: it sits top-level (k8s-style, a
// sibling of spec, not a create input) so `create -f` -- which reads only
// spec -- ignores it on re-apply.
type outDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   outMetadata    `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
	Status     *outVMStatus   `yaml:"status,omitempty"`
}

type outMetadata struct {
	Name string `yaml:"name"`
}

// outVMStatus is the projected, system-reported VM status. It mirrors the
// fields the cpclient.VMStatus view carries (phase / reason / message);
// reason and message are emitted only when set. It is informational output
// only -- the create/apply path reads spec, never status.
type outVMStatus struct {
	Phase   string `yaml:"phase"`
	Reason  string `yaml:"reason,omitempty"`
	Message string `yaml:"message,omitempty"`
}

// encodeDoc marshals one outDoc with two-space indentation.
func encodeDoc(d outDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("manifest: encode %s/%s: %v", d.Kind, d.Metadata.Name, err)
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// JoinDocuments concatenates encoded documents with the YAML `---`
// separator, producing a single multi-document stream.
func JoinDocuments(docs [][]byte) []byte {
	return bytes.Join(docs, []byte("---\n"))
}

// ProjectNetwork renders a live Network as an apply-ready manifest. For
// a bridge network it carries mtu (always, since the column is NOT NULL)
// and vlan (only when a tag is set) so a `network get -o yaml | create
// -f` round-trip does not silently reset a non-default MTU or drop the
// VLAN tag. For an overlay network the create API forbids bridgeName /
// mtu / vlan / gateway / egress (all server-derived or fixed) and
// requires subnet, so the projection emits only type + subnet -- the
// minimal valid overlay create body. Re-applying allocates a fresh VNI,
// the same way an id is not preserved across a round-trip. The
// operator-settable `config` blob is not yet manifest-expressible and so
// is dropped on every projection (see the docs round-trip caveat).
func ProjectNetwork(n cpclient.Network) ([]byte, error) {
	var spec map[string]any
	if n.Type == "overlay" {
		spec = overlayNetworkSpec(n)
	} else {
		spec = bridgeNetworkSpec(n)
	}
	return encodeDoc(outDoc{
		APIVersion: APIVersionV1,
		Kind:       KindNetwork,
		Metadata:   outMetadata{Name: n.Name},
		Spec:       spec,
	})
}

// overlayNetworkSpec builds the apply-ready spec map for an overlay
// network: type + subnet (the minimal valid overlay create body) plus the
// user-set fields (egress, dhcp, dns) when non-default. bridgeName / mtu /
// vlan / gateway are server-derived or fixed for overlay (the create API
// forbids them) and so are not emitted.
func overlayNetworkSpec(n cpclient.Network) map[string]any {
	spec := map[string]any{"type": n.Type}
	if n.Subnet != nil && *n.Subnet != "" {
		spec["subnet"] = *n.Subnet
	}
	// egress is user-set for an overlay (none|nat); emit the non-default nat so
	// a nat overlay round-trips instead of silently re-applying as egress=none.
	if n.Egress != "" && n.Egress != "none" {
		spec["egress"] = n.Egress
	}
	if n.Dhcp != nil && *n.Dhcp {
		spec["dhcp"] = true
	}
	// Overlay dns defaults on server-side, so emit only the off case.
	if n.DNS != nil && !*n.DNS {
		spec["dns"] = false
	}
	return spec
}

// bridgeNetworkSpec builds the apply-ready spec map for a bridge
// network. mtu is always emitted (NOT NULL, lossless even at 1500); the
// remaining fields are emitted only when non-default so the round-trip
// stays minimal.
func bridgeNetworkSpec(n cpclient.Network) map[string]any {
	spec := map[string]any{
		"type":       n.Type,
		"bridgeName": n.BridgeName,
	}
	if n.Managed {
		spec["managed"] = true
	}
	if n.Egress != "" && n.Egress != "none" {
		spec["egress"] = n.Egress
	}
	if n.Subnet != nil && *n.Subnet != "" {
		spec["subnet"] = *n.Subnet
	}
	if n.Gateway != nil && *n.Gateway != "" {
		spec["gateway"] = *n.Gateway
	}
	if n.Dhcp != nil && *n.Dhcp {
		spec["dhcp"] = true
	}
	// Bridge dns defaults to the dhcp value server-side, so emit it only when
	// it diverges (dhcp on + dns off, or a dns-only network with dhcp off).
	if dhcpVal := n.Dhcp != nil && *n.Dhcp; n.DNS != nil && *n.DNS != dhcpVal {
		spec["dns"] = *n.DNS
	}
	// MTU is NOT NULL / always populated, so emitting it (even 1500) is
	// lossless. VlanTag is nullable: a nil pointer means untagged.
	spec["mtu"] = n.MTU
	if n.VlanTag != nil {
		spec["vlan"] = *n.VlanTag
	}
	return spec
}

// ProjectPoolInstance renders a single pool instance (node-scoped) as a
// manifest with `spec.node`.
func ProjectPoolInstance(p cpclient.Pool) ([]byte, error) {
	return encodeDoc(outDoc{
		APIVersion: APIVersionV1,
		Kind:       KindStoragePool,
		Metadata:   outMetadata{Name: p.Name},
		Spec: map[string]any{
			"type": p.Type,
			"path": p.Path,
			"node": p.Node,
		},
	})
}

// ProjectPoolConcept renders an aggregated pool concept (all instances
// of a name) as an apply-ready manifest. When every instance shares the
// same type and path, it emits one document with `spec.nodeList` - the
// compact inverse of the create-time expansion. When instances diverge
// (path is a free per-instance column, so a heterogeneous cluster can
// hold different paths under one name), a single nodeList document would
// rewrite every node to the first instance's path on re-apply, silently
// dropping the others; in that case it emits one document per instance so
// each node keeps its own type/path.
func ProjectPoolConcept(c cpclient.PoolConceptView) ([]byte, error) {
	if len(c.Instances) == 0 {
		return nil, fmt.Errorf("manifest: pool concept %q has no instances to project", c.Name)
	}
	first := c.Instances[0]
	uniform := true
	for _, inst := range c.Instances[1:] {
		if inst.Type != first.Type || inst.Path != first.Path {
			uniform = false
			break
		}
	}
	if !uniform {
		docs := make([][]byte, 0, len(c.Instances))
		for _, inst := range c.Instances {
			doc, err := ProjectPoolInstance(inst)
			if err != nil {
				return nil, err
			}
			docs = append(docs, doc)
		}
		return JoinDocuments(docs), nil
	}
	nodes := make([]string, 0, len(c.Instances))
	for _, inst := range c.Instances {
		nodes = append(nodes, inst.Node)
	}
	return encodeDoc(outDoc{
		APIVersion: APIVersionV1,
		Kind:       KindStoragePool,
		Metadata:   outMetadata{Name: c.Name},
		Spec: map[string]any{
			"type":     first.Type,
			"path":     first.Path,
			"nodeList": nodes,
		},
	})
}

// ProjectVM renders a live VM as an apply-ready manifest. The cloud-init
// payloads (userData / networkConfig) round-trip when the view surfaces
// them. Several create-time fields are still omitted because the API view
// does not carry them (documented limitation): cloudInitDisabled,
// firmware/firmwareID, and diskGiB. desiredPhase is also omitted (not part
// of the v1 VM manifest schema). Only the first network is projected: the
// v1 VM manifest schema has a single `network` field, so multi-NIC VMs are
// not manifest-expressible and any NICs beyond the first are omitted.
func ProjectVM(v cpclient.VM) ([]byte, error) {
	spec := map[string]any{
		"imageURL": v.ImageURL,
		"arch":     v.Architecture,
		"vcpus":    v.VCPUs,
		"memoryMB": v.MemoryMB,
	}
	if v.ImageSHA256 != "" {
		spec["imageSHA256"] = v.ImageSHA256
	}
	if v.Format != "" {
		spec["format"] = v.Format
	}
	if v.ImagePullPolicy != "" {
		spec["imagePullPolicy"] = v.ImagePullPolicy
	}
	if v.Pool != "" {
		spec["pool"] = v.Pool
	}
	if v.Node != nil && *v.Node != "" {
		spec["node"] = *v.Node
	}
	if len(v.Networks) > 0 {
		spec["network"] = v.Networks[0]
	}
	if v.UserData != nil && *v.UserData != "" {
		spec["userData"] = *v.UserData
	}
	if v.NetworkConfig != nil && *v.NetworkConfig != "" {
		spec["networkConfig"] = *v.NetworkConfig
	}
	if v.SourceSnapshotID != nil && *v.SourceSnapshotID != "" {
		spec["sourceSnapshotID"] = *v.SourceSnapshotID
	}
	// status is observed state, projected top-level (sibling of spec) so a
	// re-apply -- which reads only spec -- ignores it. Emit it only when the
	// VM has a reported phase, so a freshly-created VM with no status yet does
	// not carry a noisy empty block.
	var status *outVMStatus
	if v.Status.Phase != "" {
		status = &outVMStatus{
			Phase:   v.Status.Phase,
			Reason:  v.Status.Reason,
			Message: v.Status.Message,
		}
	}
	return encodeDoc(outDoc{
		APIVersion: APIVersionV1,
		Kind:       KindVM,
		Metadata:   outMetadata{Name: v.Name},
		Spec:       spec,
		Status:     status,
	})
}
