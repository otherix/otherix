// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"testing"

	"github.com/google/nftables/expr"
)

func TestMasqIfaceExprs(t *testing.T) {
	exprs := masqIfaceExprs("otb1000", "eth0")
	if len(exprs) != 5 {
		t.Fatalf("masqIfaceExprs len = %d, want 5", len(exprs))
	}
	in, ok := exprs[0].(*expr.Meta)
	if !ok || in.Key != expr.MetaKeyIIFNAME || in.Register != 1 {
		t.Errorf("expr[0] = %#v, want Meta{IIFNAME, reg1}", exprs[0])
	}
	inCmp, ok := exprs[1].(*expr.Cmp)
	if !ok || inCmp.Op != expr.CmpOpEq {
		t.Errorf("expr[1] = %#v, want Cmp Eq", exprs[1])
	}
	if string(inCmp.Data) != string(ifnameComparand("otb1000")) {
		t.Errorf("expr[1] iif comparand mismatch")
	}
	out, ok := exprs[2].(*expr.Meta)
	if !ok || out.Key != expr.MetaKeyOIFNAME {
		t.Errorf("expr[2] = %#v, want Meta OIFNAME", exprs[2])
	}
	outCmp, ok := exprs[3].(*expr.Cmp)
	if !ok || string(outCmp.Data) != string(ifnameComparand("eth0")) {
		t.Errorf("expr[3] oif comparand mismatch")
	}
	if _, ok := exprs[4].(*expr.Masq); !ok {
		t.Errorf("expr[4] = %#v, want Masq", exprs[4])
	}
}

func TestMasqIfaceUserData(t *testing.T) {
	m := masqIfaceUserData("otb1000", "eth0")
	if string(m) != "otherix:iif:otb1000:eth0" {
		t.Errorf("masqIfaceUserData = %q, want otherix:iif:otb1000:eth0", string(m))
	}
	p := masqIfacePrefix("otb1000")
	if string(p) != "otherix:iif:otb1000:" {
		t.Errorf("masqIfacePrefix = %q, want otherix:iif:otb1000:", string(p))
	}
}
