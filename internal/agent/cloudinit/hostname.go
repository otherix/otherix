// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cloudinit

import (
	"bytes"
	"fmt"

	yaml "gopkg.in/yaml.v3"
)

// userDataHeader is the literal `#cloud-config` marker cloud-init
// requires on the first line. Preserved verbatim through hostname
// injection so the resulting blob remains a valid cloud-config.
const userDataHeader = "#cloud-config"

// injectHostname returns user-data with a top-level `hostname:` key
// matching the VM name. If the input already pins a top-level
// `hostname:` (regardless of value), the input is returned unchanged
// — operator intent takes precedence over the VM name even when the
// values differ, the "full replace" rule.
//
// Empty or whitespace-only user-data materialises to a minimal
// `#cloud-config\nhostname: <name>\n` so the resulting ISO still
// satisfies the NoCloud datasource.
func injectHostname(userData []byte, hostname string) ([]byte, error) {
	if hostname == "" {
		return nil, ErrEmptyHostname
	}
	trimmed := bytes.TrimSpace(userData)
	if len(trimmed) == 0 {
		return minimalCloudConfig(hostname)
	}

	// A pre-formed multipart-MIME archive (e.g. the control plane wraps an
	// operator #cloud-config and an injected SSH CA-trust document into one
	// MIME blob) is not a YAML cloud-config and must not be parsed/rewritten -
	// doing so corrupts the archive. Pass it through verbatim; the hostname
	// still reaches the guest through the NoCloud meta-data local-hostname.
	if isMIMEUserData(trimmed) {
		return userData, nil
	}

	body := trimmed
	header := userDataHeader
	if bytes.HasPrefix(body, []byte(userDataHeader)) {
		// Strip the marker so yaml.Unmarshal sees pure YAML;
		// re-attach on output. The marker is a cloud-init
		// directive, not part of the YAML document.
		body = body[len(userDataHeader):]
	} else {
		header = ""
	}
	if len(bytes.TrimSpace(body)) == 0 {
		// Header-only input ("#cloud-config\n") collapses to
		// the same empty-input path — minimal cloud-config
		// with the VM hostname so the guest still picks it up.
		return minimalCloudConfig(hostname)
	}

	var node yaml.Node
	if err := yaml.Unmarshal(body, &node); err != nil {
		return nil, fmt.Errorf("parse user-data yaml: %w", err)
	}

	root := documentRoot(&node)
	switch {
	case root == nil:
		// Empty document after the header — still emit a minimal
		// `hostname:` mapping so the guest takes the VM name.
		return minimalCloudConfig(hostname)
	case root.Kind != yaml.MappingNode:
		// Non-mapping top-level (e.g. a bare scalar or sequence)
		// is a user-supplied cloud-config that cannot have keys
		// merged into it. Leave it untouched — operator's
		// problem to interpret, not ours to rewrite.
		return userData, nil
	}

	if hasMappingKey(root, "hostname") {
		return userData, nil
	}

	hostnameKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "hostname"}
	hostnameVal := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: hostname}
	root.Content = append([]*yaml.Node{hostnameKey, hostnameVal}, root.Content...)

	var buf bytes.Buffer
	if header != "" {
		buf.WriteString(header)
		buf.WriteByte('\n')
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, fmt.Errorf("re-encode user-data: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// minimalCloudConfig emits a #cloud-config blob carrying only a yaml-encoded
// hostname, so the name is encoded (never interpolated) even on the fallback
// paths.
func minimalCloudConfig(hostname string) ([]byte, error) {
	b, err := yaml.Marshal(map[string]string{"hostname": hostname})
	if err != nil {
		return nil, fmt.Errorf("marshal minimal cloud-config: %w", err)
	}
	return append([]byte(userDataHeader+"\n"), b...), nil
}

// isMIMEUserData reports whether the user-data is a pre-formed MIME archive,
// detected by a leading MIME header line the way cloud-init's own format
// detection is. cloud-init treats user-data beginning with a `Content-Type:` or
// `MIME-Version:` header as a MIME message rather than a #cloud-config document.
func isMIMEUserData(trimmed []byte) bool {
	return bytes.HasPrefix(trimmed, []byte("Content-Type:")) ||
		bytes.HasPrefix(trimmed, []byte("MIME-Version:"))
}

func documentRoot(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

func hasMappingKey(mapping *yaml.Node, key string) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return true
		}
	}
	return false
}
