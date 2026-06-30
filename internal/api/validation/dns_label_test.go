// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidateDNSLabel(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "single char", input: "a", wantErr: false},
		{name: "node-1", input: "node-1", wantErr: false},
		{name: "vmlc-smoke", input: "vmlc-smoke", wantErr: false},
		{name: "max len 63", input: strings.Repeat("a", DNSLabelMaxLength), wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "too long 64", input: strings.Repeat("a", DNSLabelMaxLength+1), wantErr: true},
		{name: "uppercase", input: "Demo", wantErr: true},
		{name: "underscore", input: "node_1", wantErr: true},
		{name: "dot", input: "node.1", wantErr: true},
		{name: "leading hyphen", input: "-node", wantErr: true},
		{name: "trailing hyphen", input: "node-", wantErr: true},
		{name: "embedded space", input: "no de", wantErr: true},
		{name: "slash", input: "node/1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDNSLabel(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateDNSLabel(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDNSDomain(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "single label", input: "local", wantErr: false},
		{name: "two labels", input: "ssh.otherix.local", wantErr: false},
		{name: "deep", input: "vms.cluster.example.com", wantErr: false},
		{name: "label with hyphen", input: "ssh-ingress.example.com", wantErr: false},
		{name: "numeric label", input: "1.example.com", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "leading dot", input: ".example.com", wantErr: true},
		{name: "trailing dot", input: "example.com.", wantErr: true},
		{name: "double dot", input: "ssh..example.com", wantErr: true},
		{name: "uppercase", input: "SSH.example.com", wantErr: true},
		{name: "underscore", input: "ssh_x.example.com", wantErr: true},
		{name: "space", input: "ssh x.example.com", wantErr: true},
		{name: "leading hyphen label", input: "-ssh.example.com", wantErr: true},
		{name: "trailing hyphen label", input: "ssh-.example.com", wantErr: true},
		{name: "label too long", input: strings.Repeat("a", DNSLabelMaxLength+1) + ".com", wantErr: true},
		{name: "domain too long", input: strings.Repeat("a.", 130) + "com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDNSDomain(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateDNSDomain(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateVMNameAndNodeNameDelegate(t *testing.T) {
	if err := ValidateVMName("vmlc-smoke"); err != nil {
		t.Errorf("ValidateVMName(%q) err = %v, want nil", "vmlc-smoke", err)
	}
	if err := ValidateVMName("Bad_Name"); err == nil {
		t.Errorf("ValidateVMName(%q) err = nil, want non-nil", "Bad_Name")
	}
	if err := ValidateNodeName("node-1"); err != nil {
		t.Errorf("ValidateNodeName(%q) err = %v, want nil", "node-1", err)
	}
	if err := ValidateNodeName("a/b"); err == nil {
		t.Errorf("ValidateNodeName(%q) err = nil, want non-nil", "a/b")
	}
}
