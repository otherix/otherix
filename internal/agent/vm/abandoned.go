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
		return fmt.Errorf("%w: vm %s: unexpected disk path %s; inspect and clear the node's copy of the vm to migrate it here again", ErrAbandonedCopyUnmovable, v.ID, v.DiskPath)
	}
	abandonedDir := filepath.Join(poolRoot, "abandoned")
	dst := filepath.Join(abandonedDir, v.ID.String()+"-"+v.AdoptedBy.String())
	if moved, err := m.moveCopyAside(cur, v, src, dst); err != nil || !moved {
		return err
	}

	if err := m.keepMeta(v.ID, dst); err != nil {
		return err
	}
	// The move must be durable before the record that could replay it is gone.
	for _, d := range []string{dst, filepath.Dir(src), abandonedDir, poolRoot} {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("sync %s: %v", d, err)
		}
	}

	if !m.dropSetAsideRecord(cur, v.AdoptedBy) {
		// The create path claimed the entry after the rename. The disk is safe in
		// abandoned/; the record is the VM's own now and stays, and the adopt
		// that follows refuses the VM as before.
		m.log.Warn("abandoned migration copy moved aside but its record was claimed meanwhile; keeping the record",
			"vm_id", v.ID.String(), "adopted_by", v.AdoptedBy.String(), "path", dst)
		return nil
	}
	m.detachMux(v.ID)
	m.teardownNICs(v.NICs)
	m.log.Warn("moved an abandoned migration copy aside",
		"vm_id", v.ID.String(), "adopted_by", v.AdoptedBy.String(), "path", dst)
	return nil
}

// dropSetAsideRecord drops the record of a copy that was moved aside, and its
// state dir, only while the VM entry is still cur and still adopted by
// adoptedBy. The create path can claim the entry without the lifecycle slot, so
// it is re-checked under m.mu; a changed entry is left alone and false returned.
func (m *Manager) dropSetAsideRecord(cur *VM, adoptedBy uuid.UUID) bool {
	m.mu.Lock()
	if m.vms[cur.ID] != cur || cur.AdoptedBy != adoptedBy {
		m.mu.Unlock()
		return false
	}
	delete(m.vms, cur.ID)
	m.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(m.stateDir, cur.ID.String())); err != nil {
		m.log.Warn("dropSetAsideRecord: remove agent state dir", "vm_id", cur.ID.String(), "err", err)
	}
	return true
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
// kept in the same abandoned/ dir, and with ErrAbandonedCopyUnmovable when the
// copy is at neither place.
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
		// Refuse rather than drop the record: the agent does not vouch for a copy
		// whose disk it cannot find.
		return false, fmt.Errorf("%w: vm %s: disk dir %s not found and not already moved to %s; inspect and clear the node's copy of the vm to migrate it here again",
			ErrAbandonedCopyUnmovable, v.ID, src, dst)
	}
}

// keepMeta durably copies the VM's meta.json into the moved copy's dir dst, the
// record an operator recovers the copy by. A missing meta.json is skipped. Any
// other read or write failure is returned, so the caller aborts before it
// removes the state dir, and the next request finishes the move through the
// already-moved branch.
func (m *Manager) keepMeta(id uuid.UUID, dst string) error {
	b, err := os.ReadFile(filepath.Join(m.stateDir, id.String(), state.MetaFileName)) // #nosec G304 -- the VM's own state dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s of the abandoned copy: %v", state.MetaFileName, err)
	}
	keep := filepath.Join(dst, state.MetaFileName)
	f, err := os.OpenFile(keep, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 G703 -- dst is built from the VM's own disk path and ids
	if err != nil {
		return fmt.Errorf("keep %s with the abandoned copy: %v", state.MetaFileName, err)
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("keep %s with the abandoned copy: %v", state.MetaFileName, err)
	}
	return nil
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
