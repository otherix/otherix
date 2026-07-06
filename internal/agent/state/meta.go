// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package state persists per-VM agent metadata to disk per sketch
// decision G1 — filesystem meta.json, no embedded DB. Each VM's
// directory under StatePath holds a single meta.json plus runtime
// sockets (qmp.sock, console.sock, qemu.pid). On agent startup,
// ScanState rebuilds the in-memory VM map from meta.json files.
//
// All writes use the write-temp + rename atomic pattern so a crash
// between write and rename never leaves a half-written meta.json that
// JSON-decodes successfully into garbage values.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// MetaFileName is the filename used inside each per-VM directory.
const MetaFileName = "meta.json"

// VMMeta captures everything the agent needs to reason about a VM
// across restarts. Path fields are absolute so the manager can act on
// them without re-deriving conventions.
//
// The pool reference is a string name; the pre-landing UUID schema
// is a clean break, not a migration. Existing dev Lima environments
// must `make clean-dev` after landing.
type VMMeta struct {
	VMID          uuid.UUID `json:"vm_id"`
	Name          string    `json:"name"`
	VCPUs         int       `json:"vcpus"`
	MemoryMB      int       `json:"memory_mb"`
	PoolName      string    `json:"pool_name"`
	Architecture  string    `json:"architecture"`
	DiskPath      string    `json:"disk_path"`
	QMPSocket     string    `json:"qmp_socket"`
	ConsoleSocket string    `json:"console_socket"`
	PIDFile       string    `json:"pid_file"`
	CidataPath    string    `json:"cidata_path,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// NICs records the VM's materialised network interfaces so the agent
	// can reconstruct them on startup replay and tear down the host taps
	// on delete. Omitted entirely for legacy VMs with no declared NICs.
	NICs []NICMeta `json:"nics,omitempty"`
	// Migrated marks a VM whose disk was copied in from another node by a
	// migration. The agent must NOT re-clone it from the base image on
	// first start; the disk is already the authoritative copy.
	Migrated bool `json:"migrated,omitempty"`
}

// NICMeta is the persisted form of one VM network interface. TapName is
// the resolved host tap name (netfabric.NIC.TapName) recorded at create
// time so teardown can act on it without re-deriving the convention.
type NICMeta struct {
	ID          uuid.UUID `json:"id"`
	Bridge      string    `json:"bridge"`
	MAC         string    `json:"mac"`
	Model       string    `json:"model"`
	MTU         int       `json:"mtu"`
	DeviceOrder int       `json:"device_order"`
	TapName     string    `json:"tap_name"`
}

// WriteMeta persists m to <vmDir>/meta.json using a temp-file + rename
// atomic write. The directory is created with 0o750 if missing.
func WriteMeta(vmDir string, m *VMMeta) error {
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", vmDir, err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}

	tmpPath := filepath.Join(vmDir, MetaFileName+".tmp")
	finalPath := filepath.Join(vmDir, MetaFileName)

	// Write + fsync the temp file so its bytes are durable BEFORE the rename.
	// os.WriteFile alone leaves the data in the page cache: a power loss after
	// the rename metadata is persisted but before the data is flushed yields a
	// zero-length or torn meta.json that ScanState then decodes into garbage.
	if err := writeFileSync(tmpPath, data); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	// fsync the directory so the rename itself is durable across a power loss.
	if err := syncDir(vmDir); err != nil {
		return err
	}
	return nil
}

// writeFileSync writes data to path (0600) and fsyncs the file before closing,
// so the bytes are durable on disk rather than only in the page cache.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- agent-owned per-VM state temp path
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename/create within it is durable across a
// power loss, mirroring the store-side helpers in artifactstore and vm. A sync
// failure is reported, not swallowed, since it defeats the crash-durability
// guarantee the atomic-write pattern promises.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- agent-owned per-VM state directory
	if err != nil {
		return fmt.Errorf("open dir for sync: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync dir: %w", err)
	}
	return d.Close()
}

// ReadMeta loads <vmDir>/meta.json. Returns os.ErrNotExist when the
// file is absent so callers can branch on errors.Is.
func ReadMeta(vmDir string) (*VMMeta, error) {
	// #nosec G304 -- vmDir is built from the agent's own state directory.
	data, err := os.ReadFile(filepath.Join(vmDir, MetaFileName))
	if err != nil {
		return nil, err
	}
	var m VMMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal meta in %s: %w", vmDir, err)
	}
	return &m, nil
}
