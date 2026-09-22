// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/state"
)

// setAsideAbandonedCopy moves a stopped copy this node holds for s.VMUUID out
// of the way of a new incoming migration, when the control plane has named the
// migration that adopted it as abandoned (an offline migration that ended failed
// or cancelled and so can never cut over). The copy's disk dir is RENAMED into
// the abandoned/ dir of the copy's OWN pool (which can differ from the
// request's) together with a copy of its meta.json; nothing is deleted, so a
// copy moved aside wrongly is recoverable by hand. It returns nil without doing
// anything when there is no copy or any condition fails, leaving
// AdoptForMigration to refuse the VM as before. Each step is idempotent, so a
// crash at any point is finished by the next request. Callers hold the VM's
// lifecycle slot.
func (m *Manager) setAsideAbandonedCopy(s IncomingSpec) error {
	cur, v, ok := m.abandonedCopy(s)
	if !ok {
		return nil
	}
	if err := m.releaseIncomingFor(v.ID, v.AdoptedBy); err != nil {
		return fmt.Errorf("stop abandoned migration %s server: %v", v.AdoptedBy, err)
	}

	src := filepath.Dir(v.DiskPath)             // <pool>/vms/<uuid>
	poolRoot := filepath.Dir(filepath.Dir(src)) // <pool>
	// Fail closed on an unexpected layout: never guess where to move a disk.
	if !filepath.IsAbs(src) || filepath.Base(src) != v.ID.String() || filepath.Base(filepath.Dir(src)) != "vms" {
		return fmt.Errorf("abandoned copy of vm %s: unexpected disk path %s; not moving it", v.ID, v.DiskPath)
	}
	abandonedDir := filepath.Join(poolRoot, "abandoned")
	dst := filepath.Join(abandonedDir, v.ID.String()+"-"+v.AdoptedBy.String())
	if moved, err := m.moveCopyAside(cur, v, src, dst); err != nil || !moved {
		return err
	}

	metaSrc := filepath.Join(m.stateDir, v.ID.String(), state.MetaFileName)
	if b, err := os.ReadFile(metaSrc); err == nil { // #nosec G304 -- the VM's own state dir
		keep := filepath.Join(dst, state.MetaFileName)
		if err := os.WriteFile(keep, b, 0o600); err != nil { // #nosec G703 -- dst is built from the VM's own disk path and ids, checked above
			return fmt.Errorf("keep %s with the abandoned copy: %v", state.MetaFileName, err)
		}
	}
	// The move must be durable before the record that could replay it is gone.
	for _, d := range []string{filepath.Dir(src), abandonedDir, poolRoot} {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("sync %s: %v", d, err)
		}
	}

	m.detachMux(v.ID)
	m.teardownNICs(v.NICs)
	m.dropAdoptedRecord(v.ID)
	m.log.Warn("moved an abandoned migration copy aside",
		"vm_id", v.ID.String(), "adopted_by", v.AdoptedBy.String(), "path", dst)
	return nil
}

// abandonedCopy returns the VM entry for s.VMUUID and a snapshot of it when it
// is a copy setAsideAbandonedCopy may move: adopted by a migration s names as
// abandoned (not s's own), stopped with no live qemu, and with no migration
// record other than that abandoned one still running for it. A replay of a
// migration this node already prepared never qualifies: the idempotent-resume
// guard answers it with the endpoints it minted, and it must move nothing.
func (m *Manager) abandonedCopy(s IncomingSpec) (*VM, VM, bool) {
	if rec, ok := m.migrations.Get(s.MigrationID); ok && rec.Role == migration.RoleTarget {
		return nil, VM{}, false
	}
	m.mu.Lock()
	cur, ok := m.vms[s.VMUUID]
	var v VM
	if ok {
		v = *cur
	}
	m.mu.Unlock()
	if !ok || v.AdoptedBy == uuid.Nil || v.AdoptedBy == s.MigrationID || !slices.Contains(s.AbandonedMigrationIDs, v.AdoptedBy) {
		return nil, VM{}, false
	}
	// Only stopped: a failed VM can still have a live qemu.
	if v.Status != StatusStopped || m.migQemuAlive(&v) {
		return nil, VM{}, false
	}
	if m.migrations.HasActiveForVMExcept(v.ID, v.AdoptedBy) {
		return nil, VM{}, false
	}
	return cur, v, true
}

// moveCopyAside renames the copy's disk dir src to dst, or finds it already
// there from an earlier attempt, and reports whether the copy now sits at dst.
// It refuses with ErrAbandonedCopyKept while another abandoned copy of the VM is
// kept in the same abandoned/ dir, and aborts when the copy is at neither place.
func (m *Manager) moveCopyAside(cur *VM, v VM, src, dst string) (bool, error) {
	abandonedDir := filepath.Dir(dst)
	_, srcErr := os.Stat(src)
	_, dstErr := os.Stat(dst)
	switch {
	case srcErr == nil:
		kept, err := filepath.Glob(filepath.Join(abandonedDir, v.ID.String()+"-*"))
		if err != nil {
			return false, fmt.Errorf("look for kept copies of vm %s: %v", v.ID, err)
		}
		if len(kept) > 0 {
			return false, fmt.Errorf("%w: %s; remove it to migrate the vm here again", ErrAbandonedCopyKept, kept[0])
		}
		if err := os.MkdirAll(abandonedDir, 0o750); err != nil {
			return false, fmt.Errorf("create %s: %v", abandonedDir, err)
		}
		return m.renameIfUnchanged(cur, v, src, dst)
	case errors.Is(srcErr, fs.ErrNotExist) && dstErr == nil:
		// A previous attempt already moved it.
		return true, nil
	default:
		return false, fmt.Errorf("abandoned copy of vm %s: disk dir %s not found and not already moved to %s", v.ID, src, dst)
	}
}

// renameIfUnchanged renames src to dst only while the VM entry is still cur and
// still the stopped copy snapshot v describes. The create path can claim a copy
// (clearing AdoptedBy) without the lifecycle slot, so the checks made earlier
// may be stale by now; the entry is re-checked, and held, under m.mu across the
// rename. When anything changed it moves nothing and returns (false, nil): the
// adopt that follows then refuses the VM as before. Stopping the abandoned
// migration's server first is harmless either way, as that migration can never
// cut over. A rename error, EXDEV included, changes nothing and is never turned
// into a copy and delete.
func (m *Manager) renameIfUnchanged(cur *VM, v VM, src, dst string) (bool, error) {
	// ponytail: m.mu is held across one same-filesystem rename, a single
	// metadata syscall; a hung pool filesystem would stall every m.mu user.
	m.mu.Lock()
	defer m.mu.Unlock()
	now, ok := m.vms[v.ID]
	if !ok || now != cur || now.AdoptedBy != v.AdoptedBy || now.Status != StatusStopped || now.DiskPath != v.DiskPath {
		return false, nil
	}
	if err := os.Rename(src, dst); err != nil {
		return false, fmt.Errorf("move abandoned copy %s to %s: %v", src, dst, err)
	}
	return true, nil
}

// releaseIncomingFor stops the qemu-nbd of vmID's incoming migration migID and
// releases its ports, returning the stop error. A non-terminal offline record is
// taken; a failed stop puts it back and keeps the ports. A terminal record keeps
// its ports (its finalizer released them) but its server is stopped again, in
// case the finalizer's own stop failed and the server outlived it. No record:
// nil.
func (m *Manager) releaseIncomingFor(vmID, migID uuid.UUID) error {
	if rec, ok := m.migrations.TakeIncomingByID(migID); ok {
		if rec.VMID != vmID {
			m.migrations.Put(&rec)
			return nil
		}
		if err := m.migStopNBD(rec.NBD, nbdStopGrace); err != nil {
			m.migrations.Put(&rec)
			return err
		}
		m.migPorts.ReleasePair(rec.Port, rec.NBDPort)
		return nil
	}
	rec, ok := m.migrations.Get(migID)
	if !ok || rec.VMID != vmID || rec.Role != migration.RoleTarget || rec.NBD == nil {
		return nil
	}
	return m.migStopNBD(rec.NBD, nbdStopGrace)
}
