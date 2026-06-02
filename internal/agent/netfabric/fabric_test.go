// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNICTapName(t *testing.T) {
	id := uuid.MustParse("0123456f-89ab-cdef-0123-456789abcdef")
	n := NIC{ID: id}
	got := n.TapName()
	want := "ot0123456f89ab"
	if got != want {
		t.Errorf("NIC{ID: %s}.TapName() = %q, want %q", id, got, want)
	}
	if len(got) != 14 {
		t.Errorf("len(TapName()) = %d, want 14", len(got))
	}
}

func TestFakeFabricListTaps(t *testing.T) {
	want := []string{"ot0123456f89ab", "otdeadbeef0000"}
	f := &FakeFabric{ListTapsResult: want}

	got, err := f.ListTaps()
	if err != nil {
		t.Fatalf("ListTaps() error = %v, want nil", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListTaps() mismatch (-want +got):\n%s", diff)
	}
	if f.ListTapsCalls != 1 {
		t.Errorf("ListTapsCalls = %d, want 1", f.ListTapsCalls)
	}
}

func TestFakeFabricListTapsError(t *testing.T) {
	wantErr := errors.New("boom")
	f := &FakeFabric{Errs: map[string]error{"ListTaps": wantErr}}

	got, err := f.ListTaps()
	if !errors.Is(err, wantErr) {
		t.Errorf("ListTaps() error = %v, want %v", err, wantErr)
	}
	if got != nil {
		t.Errorf("ListTaps() result = %v, want nil on error", got)
	}
	if f.ListTapsCalls != 1 {
		t.Errorf("ListTapsCalls = %d, want 1", f.ListTapsCalls)
	}
}

func TestFakeFabricVXLANLifecycle(t *testing.T) {
	f := &FakeFabric{VXLANExistsResult: true}
	cfg := VXLANConfig{VNI: 1000, Local: netip.MustParseAddr("127.0.0.1"), Port: 4789, MTU: 1390}

	if err := f.EnsureVXLAN(cfg); err != nil {
		t.Fatalf("EnsureVXLAN() = %v, want nil", err)
	}
	exists, err := f.VXLANExists(1000)
	if err != nil || !exists {
		t.Fatalf("VXLANExists(1000) = (%v, %v), want (true, nil)", exists, err)
	}
	if err := f.RemoveVXLAN(1000); err != nil {
		t.Fatalf("RemoveVXLAN(1000) = %v, want nil", err)
	}
	// netip.Addr has unexported fields; compare it by its comparable identity.
	addrOpt := cmpopts.EquateComparable(netip.Addr{})
	if diff := cmp.Diff([]VXLANConfig{cfg}, f.EnsureVXLANCalls, addrOpt); diff != "" {
		t.Errorf("EnsureVXLANCalls mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]uint32{1000}, f.RemoveVXLANCalls); diff != "" {
		t.Errorf("RemoveVXLANCalls mismatch (-want +got):\n%s", diff)
	}
}

func TestFakeFabricFDB(t *testing.T) {
	mac, err := net.ParseMAC("52:54:00:aa:bb:cc")
	if err != nil {
		t.Fatalf("ParseMAC = %v", err)
	}
	entry := FDBEntry{MAC: mac, Dst: netip.MustParseAddr("127.0.0.1")}
	f := &FakeFabric{FDBListResult: []FDBEntry{entry}}

	if err := f.FDBAppend(1000, entry); err != nil {
		t.Fatalf("FDBAppend = %v, want nil", err)
	}
	got, err := f.FDBList(1000)
	if err != nil {
		t.Fatalf("FDBList = %v, want nil", err)
	}
	if diff := cmp.Diff([]FDBEntry{entry}, got, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
		t.Errorf("FDBList mismatch (-want +got):\n%s", diff)
	}
	if err := f.FDBDelete(1000, entry); err != nil {
		t.Fatalf("FDBDelete = %v, want nil", err)
	}
	if diff := cmp.Diff([]FDBCall{{VNI: 1000, Entry: entry}}, f.FDBAppendCalls, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
		t.Errorf("FDBAppendCalls mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]FDBCall{{VNI: 1000, Entry: entry}}, f.FDBDeleteCalls, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
		t.Errorf("FDBDeleteCalls mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]uint32{1000}, f.FDBListCalls); diff != "" {
		t.Errorf("FDBListCalls mismatch (-want +got):\n%s", diff)
	}
}

func TestFakeFabricWireGuardLifecycle(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey = %v", err)
	}
	cfg := WGConfig{
		Name:       "otwg0",
		PrivateKey: key,
		ListenPort: 51820,
		Address:    netip.MustParsePrefix("10.42.0.1/24"),
		MTU:        1420,
	}
	f := &FakeFabric{WireGuardExistsResult: true}

	if err := f.EnsureWireGuard(cfg); err != nil {
		t.Fatalf("EnsureWireGuard() = %v, want nil", err)
	}
	exists, err := f.WireGuardExists("otwg0")
	if err != nil || !exists {
		t.Fatalf("WireGuardExists(otwg0) = (%v, %v), want (true, nil)", exists, err)
	}
	if err := f.RemoveWireGuard("otwg0"); err != nil {
		t.Fatalf("RemoveWireGuard(otwg0) = %v, want nil", err)
	}
	opt := cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})
	if diff := cmp.Diff([]WGConfig{cfg}, f.EnsureWireGuardCalls, opt); diff != "" {
		t.Errorf("EnsureWireGuardCalls mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"otwg0"}, f.RemoveWireGuardCalls); diff != "" {
		t.Errorf("RemoveWireGuardCalls mismatch (-want +got):\n%s", diff)
	}
}

func TestValidateMAC(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "valid locally administered", in: "52:54:00:12:34:56", wantErr: false},
		{name: "valid uppercase", in: "AA:BB:CC:DD:EE:FF", wantErr: false},
		{name: "empty", in: "", wantErr: true},
		{name: "garbage", in: "not-a-mac", wantErr: true},
		{name: "eight octet eui64", in: "52:54:00:12:34:56:78:9a", wantErr: true},
		{name: "too short", in: "52:54:00", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMAC(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMAC(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateModel(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "empty defaults later", in: "", wantErr: false},
		{name: "virtio", in: "virtio", wantErr: false},
		{name: "e1000", in: "e1000", wantErr: false},
		{name: "e1000e", in: "e1000e", wantErr: false},
		{name: "rtl8139", in: "rtl8139", wantErr: false},
		{name: "unknown", in: "vmxnet3", wantErr: true},
		{name: "wrong case", in: "Virtio", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateModel(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateModel(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}
