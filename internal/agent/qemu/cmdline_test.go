// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validSpec() VMSpec {
	return VMSpec{
		Name:            "test-vm",
		UUID:            uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VCPUs:           2,
		MemoryMB:        2048,
		Accelerator:     "tcg",
		DiskPath:        "/opt/otherix/pools/default/vms/abc/disk.qcow2",
		QMPSocket:       "/opt/otherix/vms/abc/qmp.sock",
		ConsoleSocket:   "/opt/otherix/vms/abc/console.sock",
		PIDFile:         "/opt/otherix/vms/abc/qemu.pid",
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
		"-accel tcg",
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
		"-accel tcg",
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
