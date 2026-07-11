// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

func validSpec() VMSpec {
	return VMSpec{
		Name:            "test-vm",
		UUID:            uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VCPUs:           2,
		MemoryMB:        2048,
		Accelerator:     "tcg",
		DiskPath:        "/var/lib/otherix/pools/default/vms/abc/disk.qcow2",
		QMPSocket:       "/var/lib/otherix/vms/abc/qmp.sock",
		ConsoleSocket:   "/var/lib/otherix/vms/abc/console.sock",
		PIDFile:         "/var/lib/otherix/vms/abc/qemu.pid",
		AArch64Firmware: "/usr/share/AAVMF/AAVMF_CODE.fd",
	}
}

func TestBuildArgs_AMD64(t *testing.T) {
	spec := validSpec()
	spec.Architecture = ArchAMD64

	args, err := BuildArgs(spec)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-name test-vm",
		"-uuid 11111111-2222-3333-4444-555555555555",
		"-smp 2",
		"-m 2048",
		"-accel tcg,thread=multi",
		"-cpu host",
		"-daemonize",
		"-display none",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("amd64 args missing %q: %s", want, joined)
		}
	}

	// -nographic conflicts with -daemonize on qemu 8.x — must not appear.
	if strings.Contains(joined, "-nographic") {
		t.Errorf("amd64 must not use -nographic (incompatible with -daemonize): %s", joined)
	}
	if strings.Contains(joined, "-machine virt") {
		t.Errorf("amd64 should not use -machine virt: %s", joined)
	}
	if strings.Contains(joined, "-bios") {
		t.Errorf("amd64 should not pass -bios: %s", joined)
	}
}

func TestBuildArgs_ARM64(t *testing.T) {
	spec := validSpec()
	spec.Architecture = ArchARM64

	args, err := BuildArgs(spec)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-machine virt",
		"-cpu max",
		"-bios /usr/share/AAVMF/AAVMF_CODE.fd",
		"-accel tcg,thread=multi",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("arm64 args missing %q: %s", want, joined)
		}
	}

	if strings.Contains(joined, "-cpu host") {
		t.Errorf("arm64 should use -cpu max, not host: %s", joined)
	}
}

func TestBuildArgs_ARM64MissingFirmware(t *testing.T) {
	spec := validSpec()
	spec.Architecture = ArchARM64
	spec.AArch64Firmware = ""

	if _, err := BuildArgs(spec); err == nil {
		t.Fatalf("BuildArgs(arm64, no firmware) = nil, want error")
	}
}

func TestBuildArgs_UnsupportedArch(t *testing.T) {
	spec := validSpec()
	spec.Architecture = Architecture("riscv64")

	if _, err := BuildArgs(spec); err == nil {
		t.Fatalf("BuildArgs(riscv64) = nil, want error")
	}
}

func TestBuildArgs_ValidatesSpec(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*VMSpec)
	}{
		{"empty name", func(s *VMSpec) { s.Name = "" }},
		{"nil uuid", func(s *VMSpec) { s.UUID = uuid.Nil }},
		{"zero vcpus", func(s *VMSpec) { s.VCPUs = 0 }},
		{"low memory", func(s *VMSpec) { s.MemoryMB = 64 }},
		{"empty disk", func(s *VMSpec) { s.DiskPath = "" }},
		{"empty qmp socket", func(s *VMSpec) { s.QMPSocket = "" }},
		{"empty accelerator", func(s *VMSpec) { s.Accelerator = "" }},
		{"bogus accelerator", func(s *VMSpec) { s.Accelerator = "haxm" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			spec.Architecture = ArchAMD64
			tc.mut(&spec)
			if _, err := BuildArgs(spec); err == nil {
				t.Fatalf("BuildArgs(%s) = nil, want error", tc.name)
			}
		})
	}
}

func nicWithID(id, mac, model string, order int) netfabric.NIC {
	return netfabric.NIC{
		ID:          uuid.MustParse(id),
		Bridge:      "br0",
		MAC:         mac,
		Model:       model,
		MTU:         1500,
		DeviceOrder: order,
	}
}

func TestBuildArgs_Networking(t *testing.T) {
	// Tap names are derived from the NIC id via netfabric.NIC.TapName,
	// so compute them from the same ids the cases use.
	nic0 := nicWithID("aaaaaaaa-1111-2222-3333-444444444444", "52:54:00:11:22:33", "virtio", 0)
	nic1 := nicWithID("bbbbbbbb-5555-6666-7777-888888888888", "52:54:00:aa:bb:cc", "virtio", 1)
	e1000NIC := nicWithID("cccccccc-9999-0000-1111-222222222222", "52:54:00:de:ad:be", "e1000", 0)

	cases := []struct {
		name        string
		nics        []netfabric.NIC
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "empty nics keeps slirp",
			nics: nil,
			wantContain: []string{
				"-netdev user,id=net0",
				"-device virtio-net-pci,netdev=net0",
			},
		},
		{
			name: "single virtio nic",
			nics: []netfabric.NIC{nic0},
			wantContain: []string{
				"-netdev tap,id=net0,ifname=" + nic0.TapName() + ",script=no,downscript=no",
				"-device virtio-net-pci,netdev=net0,mac=52:54:00:11:22:33",
			},
			wantAbsent: []string{"-netdev user,id=net0"},
		},
		{
			name: "two nics sorted by device order",
			// nic1 (order 1) listed first to prove sort by DeviceOrder.
			nics: []netfabric.NIC{nic1, nic0},
			wantContain: []string{
				"-netdev tap,id=net0,ifname=" + nic0.TapName() + ",script=no,downscript=no",
				"-device virtio-net-pci,netdev=net0,mac=52:54:00:11:22:33",
				"-netdev tap,id=net1,ifname=" + nic1.TapName() + ",script=no,downscript=no",
				"-device virtio-net-pci,netdev=net1,mac=52:54:00:aa:bb:cc",
			},
			wantAbsent: []string{"-netdev user,id=net0"},
		},
		{
			name: "e1000 model maps to e1000 device",
			nics: []netfabric.NIC{e1000NIC},
			wantContain: []string{
				"-device e1000,netdev=net0,mac=52:54:00:de:ad:be",
			},
			wantAbsent: []string{"-device virtio-net-pci,netdev=net0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			spec.Architecture = ArchAMD64
			spec.NICs = tc.nics

			args, err := BuildArgs(spec)
			if err != nil {
				t.Fatalf("BuildArgs: %v", err)
			}
			joined := strings.Join(args, " ")
			for _, want := range tc.wantContain {
				if !strings.Contains(joined, want) {
					t.Errorf("args missing %q: %s", want, joined)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(joined, absent) {
					t.Errorf("args should not contain %q: %s", absent, joined)
				}
			}
		})
	}
}

func TestBuildArgs_RejectsBadNIC(t *testing.T) {
	cases := []struct {
		name string
		nic  netfabric.NIC
	}{
		{
			name: "invalid mac",
			nic:  nicWithID("aaaaaaaa-1111-2222-3333-444444444444", "not-a-mac", "virtio", 0),
		},
		{
			name: "invalid model",
			nic:  nicWithID("aaaaaaaa-1111-2222-3333-444444444444", "52:54:00:11:22:33", "bogus", 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			spec.Architecture = ArchAMD64
			spec.NICs = []netfabric.NIC{tc.nic}
			if _, err := BuildArgs(spec); err == nil {
				t.Fatalf("BuildArgs(%s) = nil, want error", tc.name)
			}
		})
	}
}

func TestBinary(t *testing.T) {
	cases := []struct {
		arch Architecture
		want string
	}{
		{ArchAMD64, "qemu-system-x86_64"},
		{ArchARM64, "qemu-system-aarch64"},
	}
	for _, tc := range cases {
		t.Run(string(tc.arch), func(t *testing.T) {
			got, err := Binary(tc.arch)
			if err != nil {
				t.Fatalf("Binary(%s): %v", tc.arch, err)
			}
			if got != tc.want {
				t.Errorf("Binary(%s) = %q, want %q", tc.arch, got, tc.want)
			}
		})
	}

	if _, err := Binary(Architecture("foo")); err == nil {
		t.Fatalf("Binary(foo) = nil, want error")
	}
}

func TestBuildArgs_BalloonDevicePresentBothArches(t *testing.T) {
	base := VMSpec{
		Name: "vm1", UUID: uuid.New(), VCPUs: 1, MemoryMB: 512,
		Accelerator: "tcg", DiskPath: "/d.qcow2", QMPSocket: "/q.sock",
		ConsoleSocket: "/c.sock", PIDFile: "/p.pid",
	}
	want := "-device virtio-balloon,id=balloon0,free-page-reporting=on,guest-stats-polling-interval=2"
	for _, tc := range []struct {
		name string
		spec VMSpec
	}{
		{"amd64", func() VMSpec { s := base; s.Architecture = ArchAMD64; return s }()},
		{"arm64", func() VMSpec { s := base; s.Architecture = ArchARM64; s.AArch64Firmware = "/fw.fd"; return s }()},
		{"migration-target-omitbootdisk", func() VMSpec { s := base; s.Architecture = ArchAMD64; s.OmitBootDisk = true; return s }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := BuildArgs(tc.spec)
			if err != nil {
				t.Fatalf("BuildArgs(%s) error: %v", tc.name, err)
			}
			if got := strings.Join(args, " "); !strings.Contains(got, want) {
				t.Errorf("BuildArgs(%s) missing balloon device.\n got: %s\nwant substring: %s", tc.name, got, want)
			}
		})
	}
}
