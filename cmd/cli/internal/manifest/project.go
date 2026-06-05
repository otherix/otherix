// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// outDoc is the manifest projection shape. Server-assigned fields (id,
// timestamps, status, owner, reconciliation) are intentionally absent
// so the output is directly re-appliable via `create -f`.
type outDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   outMetadata    `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
}

type outMetadata struct {
	Name string `yaml:"name"`
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

// ProjectNetwork renders a live Network as an apply-ready manifest.
func ProjectNetwork(n cpclient.Network) ([]byte, error) {
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
	return encodeDoc(outDoc{
		APIVersion: APIVersionV1,
		Kind:       KindNetwork,
		Metadata:   outMetadata{Name: n.Name},
		Spec:       spec,
	})
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
// of a name) as one manifest with `spec.nodeList` - the inverse of the
// create-time expansion. Type/path are taken from the first instance.
func ProjectPoolConcept(c cpclient.PoolConceptView) ([]byte, error) {
	if len(c.Instances) == 0 {
		return nil, fmt.Errorf("manifest: pool concept %q has no instances to project", c.Name)
	}
	nodes := make([]string, 0, len(c.Instances))
	for _, inst := range c.Instances {
		nodes = append(nodes, inst.Node)
	}
	first := c.Instances[0]
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

// ProjectVM renders a live VM as an apply-ready manifest. cloudInit is
// intentionally omitted: user_data is create-time and not surfaced by
// the API view, so it cannot round-trip (documented limitation).
// desiredPhase is also omitted (not part of the v1 VM manifest schema).
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
	if v.Pool != "" {
		spec["pool"] = v.Pool
	}
	if v.Node != nil && *v.Node != "" {
		spec["node"] = *v.Node
	}
	if len(v.Networks) > 0 {
		spec["network"] = v.Networks[0]
	}
	return encodeDoc(outDoc{
		APIVersion: APIVersionV1,
		Kind:       KindVM,
		Metadata:   outMetadata{Name: v.Name},
		Spec:       spec,
	})
}
