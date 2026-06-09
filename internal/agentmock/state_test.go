// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStoragePoolMutators(t *testing.T) {
	m := Start(t, Options{})
	name := "default"
	m.AddStoragePool(StoragePool{
		ID:   uuid.New(),
		Name: name,
		Type: "local_dir",
		Path: "/var/lib/otherix/pools/default",
	})

	if err := m.SetPoolCapacity(name, 10<<30, 5<<30); err != nil {
		t.Fatalf("SetPoolCapacity: %v", err)
	}

	m.state.mu.Lock()
	p, ok := m.state.pools[name]
	m.state.mu.Unlock()
	if !ok {
		t.Fatal("pool missing after AddStoragePool")
	}
	if p.CapacityBytes != 10<<30 || p.AvailableBytes != 5<<30 {
		t.Errorf("capacity=%d available=%d; want 10G/5G", p.CapacityBytes, p.AvailableBytes)
	}
	if p.reportedAt.IsZero() {
		t.Errorf("reportedAt should be stamped after SetPoolCapacity")
	}
}

func TestSetPoolCapacity_UnregisteredPool(t *testing.T) {
	m := Start(t, Options{})
	err := m.SetPoolCapacity("nonexistent", 1, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want wrapped ErrNotFound", err)
	}
}

func TestImageMutators(t *testing.T) {
	m := Start(t, Options{})
	name := "p"
	m.AddStoragePool(StoragePool{ID: uuid.New(), Name: name, Type: "local_dir", Path: "/p"})

	img := CachedImage{
		ChecksumSHA256: "abc123",
		Format:         "qcow2",
		SizeBytes:      1024,
		Path:           "/p/img.qcow2",
		LastUsedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceURL:      "https://example.invalid/img.qcow2",
	}
	if err := m.AddImage(name, img); err != nil {
		t.Fatalf("AddImage: %v", err)
	}

	if !m.EvictImage(name, "abc123") {
		t.Errorf("EvictImage on staged image = false, want true")
	}
	if m.EvictImage(name, "abc123") {
		t.Errorf("EvictImage second call = true, want false")
	}
}

func TestAddImage_UnregisteredPool(t *testing.T) {
	m := Start(t, Options{})
	err := m.AddImage("nonexistent", CachedImage{ChecksumSHA256: "x", Format: "raw", Path: "/x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want wrapped ErrNotFound", err)
	}
}

func TestSetCapability_KnownPaths(t *testing.T) {
	m := Start(t, Options{})

	cases := []struct {
		path  string
		value any
	}{
		{"hostname", "host-1"},
		{"cpu_model", "Xeon"},
		{"cpu_features", []string{"vmx", "aes"}},
		{"cpu_cores_total", 16},
		{"cpu_cores_available", 14},
		{"memory_total_mib", int64(65536)},
		{"memory_available_mib", int64(60000)},
		{"kernel_version", "6.10"},
		{"qemu_version", "8.2"},
		{"kvm_available", true},
		{"nested_virt", true},
		{"qemu_binaries", map[string]string{"amd64": "/usr/bin/qemu-system-x86_64"}},
		{"hugepages_2mib_total", 1024},
		{"hugepages_1gib_total", 16},
		{"labels.zone", "eu-west-1"},
	}
	for _, c := range cases {
		if err := m.SetCapability(c.path, c.value); err != nil {
			t.Errorf("SetCapability(%q, ...): %v", c.path, err)
		}
	}

	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.hostname != "host-1" {
		t.Errorf("hostname = %q, want host-1", m.state.hostname)
	}
	if m.state.cpuCoresTotal != 16 {
		t.Errorf("cpu_cores_total = %d, want 16", m.state.cpuCoresTotal)
	}
	if m.state.labels["zone"] != "eu-west-1" {
		t.Errorf("labels[zone] = %q, want eu-west-1", m.state.labels["zone"])
	}
}

func TestSetCapability_InvalidPath(t *testing.T) {
	m := Start(t, Options{})

	cases := []struct {
		name  string
		path  string
		value any
	}{
		{"unknown_field", "weird_field", "x"},
		{"wrong_type_string_for_int", "cpu_cores_total", "sixteen"},
		{"wrong_type_for_bool", "kvm_available", 1},
		{"empty_label_key", "labels.", "x"},
		{"non_string_label_value", "labels.zone", 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.SetCapability(c.path, c.value)
			if !errors.Is(err, ErrInvalidPath) {
				t.Errorf("err = %v, want wrapped ErrInvalidPath", err)
			}
		})
	}
}

func TestSetMigrationCapability(t *testing.T) {
	m := Start(t, Options{})
	cap := MigrationCapability{
		Host:           "10.0.0.5",
		PortRangeStart: 50000,
		PortRangeEnd:   50100,
	}
	m.SetMigrationCapability(cap)

	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.migration.Host != "10.0.0.5" {
		t.Errorf("migration.Host = %q, want 10.0.0.5", m.state.migration.Host)
	}
}
