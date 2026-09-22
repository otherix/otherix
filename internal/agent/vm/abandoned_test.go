// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/state"
)

const abandonedBytes = "abandoned copy bytes"

// abandonedFixture is a manager holding a stopped copy of a VM adopted by the
// offline migration old, with a known disk at its DiskPath.
type abandonedFixture struct {
	m     *Manager
	vmID  uuid.UUID
	old   uuid.UUID
	disk  string
	stops *stopNBDSpy
}

// stopNBDSpy records every migStopNBD call (nil servers included) and fails
// them all while err is set.
type stopNBDSpy struct {
	mu    sync.Mutex
	calls []*qemu.NBDServer
	err   error
}

func (s *stopNBDSpy) stop(srv *qemu.NBDServer, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, srv)
	return s.err
}

func (s *stopNBDSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func newAbandonedFixture(t *testing.T) *abandonedFixture {
	t.Helper()
	m, _ := NewManagerForSeamTest(t)
	f := &abandonedFixture{m: m, vmID: uuid.New(), old: uuid.New(), stops: &stopNBDSpy{}}
	m.migStopNBD = f.stops.stop
	m.migQemuAlive = func(*VM) bool { return false }
	f.disk = f.adopt(t, m.defaultTestPool())
	return f
}

// adopt adopts the fixture VM for old in pool and writes its disk.
func (f *abandonedFixture) adopt(t *testing.T, pool string) string {
	t.Helper()
	v, err := f.m.AdoptForMigration(AdoptSpec{
		UUID: f.vmID, Name: "cold", VCPUs: 1, MemoryMib: 512, PoolName: pool,
		Architecture: qemu.ArchAMD64, MigrationID: f.old,
	})
	if err != nil {
		t.Fatalf("AdoptForMigration: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(v.DiskPath), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(v.DiskPath, []byte(abandonedBytes), 0o600); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	return v.DiskPath
}

// spec is an offline incoming spec for a new migration naming abandoned.
func (f *abandonedFixture) spec(abandoned ...uuid.UUID) IncomingSpec {
	s := offlineIncomingSpec(f.m, uuid.New(), f.vmID)
	s.AbandonedMigrationIDs = abandoned
	return s
}

func (f *abandonedFixture) mutate(fn func(v *VM)) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	fn(f.m.vms[f.vmID])
}

func (f *abandonedFixture) kept(poolRoot string) string {
	return filepath.Join(poolRoot, "abandoned", f.vmID.String()+"-"+f.old.String())
}

// assertNotMoved fails unless the copy is still where it was, with its bytes,
// still recorded as adopted by old, and no abandoned/ entry was made for it.
func (f *abandonedFixture) assertNotMoved(t *testing.T) {
	t.Helper()
	if got, err := os.ReadFile(f.disk); err != nil || string(got) != abandonedBytes {
		t.Errorf("ReadFile(%s) = (%q, %v), want the copy untouched", f.disk, got, err)
	}
	v, err := f.m.Get(f.vmID)
	if err != nil {
		t.Fatalf("Get(%s) = %v, want the copy still recorded", f.vmID, err)
	}
	if v.AdoptedBy != f.old {
		t.Errorf("AdoptedBy = %s, want %s", v.AdoptedBy, f.old)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(f.disk)))
	if matches, _ := filepath.Glob(filepath.Join(root, "abandoned", f.vmID.String()+"-*")); len(matches) != 0 {
		t.Errorf("abandoned entries %v, want none", matches)
	}
}

// fileHashes maps every regular file under root, except those under skip, to a
// hash of its content, keyed by content so a moved file compares equal.
func fileHashes(t *testing.T, root, skip string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p == skip {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() || d.Name() == state.MetaFileName {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%x", sha256.Sum256(b)))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func TestSetAsideMovesAListedAbandonedCopy(t *testing.T) {
	f := newAbandonedFixture(t)
	root := f.m.defaultTestPoolRoot(t)
	vmDir := filepath.Dir(f.disk)
	stale := filepath.Join(f.m.stateDir, f.vmID.String(), "serial.log")
	if err := os.WriteFile(stale, []byte("old guest log"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := f.m.Get(f.vmID)
	filesBefore := fileHashes(t, root, "")
	s := f.spec(f.old)

	if _, err := f.m.StartIncoming(context.Background(), s); err != nil {
		t.Fatalf("StartIncoming(listed) = %v, want success", err)
	}

	kept := f.kept(root)
	if got, err := os.ReadFile(filepath.Join(kept, "disk.qcow2")); err != nil || string(got) != abandonedBytes {
		t.Errorf("kept disk = (%q, %v), want the original bytes", got, err)
	}
	meta, err := state.ReadMeta(kept)
	if err != nil || meta.AdoptedBy != f.old {
		t.Errorf("kept meta.json = (%+v, %v), want the old record adopted by %s", meta, err, f.old)
	}
	v, err := f.m.Get(f.vmID)
	if err != nil {
		t.Fatalf("Get after the new adopt: %v", err)
	}
	if v.AdoptedBy != s.MigrationID || !v.ArrivedAt.After(before.ArrivedAt) {
		t.Errorf("new adopt AdoptedBy/ArrivedAt = %s/%v, want %s and later than %v",
			v.AdoptedBy, v.ArrivedAt, s.MigrationID, before.ArrivedAt)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("old state dir file %s: stat err = %v, want removed", stale, err)
	}
	if diff := cmp.Diff(filesBefore, fileHashes(t, root, vmDir)); diff != "" {
		t.Errorf("pool files changed (-before +after, excluding the new adopt's dir):\n%s", diff)
	}
	// A restarted agent replays the new adopt, not the old copy.
	m2 := reopenManager(t, f.m)
	if got, err := m2.Get(f.vmID); err != nil || got.AdoptedBy != s.MigrationID {
		t.Errorf("reopened Get = (%v, %v), want the new adopt by %s", got.AdoptedBy, err, s.MigrationID)
	}
}

func TestSetAsideLeavesTheCopyWhenAConditionFails(t *testing.T) {
	alive := func(f *abandonedFixture) { f.m.migQemuAlive = func(*VM) bool { return true } }
	tests := []struct {
		name  string
		setup func(f *abandonedFixture) IncomingSpec
	}{
		{name: "not listed", setup: func(f *abandonedFixture) IncomingSpec { return f.spec(uuid.New()) }},
		{name: "empty list", setup: func(f *abandonedFixture) IncomingSpec { return f.spec() }},
		{name: "not adopted by a migration", setup: func(f *abandonedFixture) IncomingSpec {
			f.mutate(func(v *VM) { v.AdoptedBy = uuid.Nil })
			f.old = uuid.Nil
			return f.spec(uuid.Nil)
		}},
		{name: "adopted by this migration", setup: func(f *abandonedFixture) IncomingSpec {
			s := f.spec(f.old)
			s.MigrationID = f.old
			return s
		}},
		{name: "failed", setup: func(f *abandonedFixture) IncomingSpec {
			f.mutate(func(v *VM) { v.Status = StatusFailed })
			return f.spec(f.old)
		}},
		{name: "running", setup: func(f *abandonedFixture) IncomingSpec {
			f.mutate(func(v *VM) { v.Status = StatusRunning })
			return f.spec(f.old)
		}},
		{name: "qemu alive", setup: func(f *abandonedFixture) IncomingSpec {
			alive(f)
			return f.spec(f.old)
		}},
		{name: "source record", setup: func(f *abandonedFixture) IncomingSpec {
			f.m.migrations.Put(&migration.Record{MigrationID: uuid.New(), VMID: f.vmID, Role: migration.RoleSource, Phase: migration.PhaseActive})
			return f.spec(f.old)
		}},
		{name: "other target record", setup: func(f *abandonedFixture) IncomingSpec {
			f.m.migrations.Put(&migration.Record{MigrationID: uuid.New(), VMID: f.vmID, Role: migration.RoleTarget, Mode: migration.ModeOffline, Phase: migration.PhaseSetup})
			return f.spec(f.old)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAbandonedFixture(t)
			s := tc.setup(f)
			// A server the abandoned migration may have left behind: a refused
			// set-aside must not touch it either.
			if f.old != uuid.Nil && s.MigrationID != f.old {
				f.m.migrations.Put(&migration.Record{
					MigrationID: f.old, VMID: f.vmID, Role: migration.RoleTarget, Mode: migration.ModeOffline,
					Phase: migration.PhaseCancelled, NBD: &qemu.NBDServer{Pid: 7},
				})
			}

			_, err := f.m.StartIncoming(context.Background(), s)

			if err == nil || !strings.Contains(err.Error(), "already present") {
				t.Errorf("StartIncoming(%s) = %v, want already present", tc.name, err)
			}
			if n := f.stops.count(); n != 0 {
				t.Errorf("StartIncoming(%s) stopped %d servers, want none", tc.name, n)
			}
			f.assertNotMoved(t)
		})
	}
}

func TestSetAsideStopsTheAbandonedServerFirst(t *testing.T) {
	f := newAbandonedFixture(t)
	f.m.migPorts = migration.NewPortAllocator(49152, 49152) // exactly one port
	port, err := f.m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	srv := &qemu.NBDServer{Pid: 7}
	f.m.migrations.Put(&migration.Record{
		MigrationID: f.old, VMID: f.vmID, Role: migration.RoleTarget, Mode: migration.ModeOffline,
		Phase: migration.PhaseSetup, Port: port, NBD: srv,
	})

	// The new incoming needs the only port, so it succeeds only if the
	// abandoned migration's port was released.
	if _, err := f.m.StartIncoming(context.Background(), f.spec(f.old)); err != nil {
		t.Fatalf("StartIncoming = %v, want success", err)
	}
	if f.stops.count() != 1 || f.stops.calls[0] != srv {
		t.Errorf("stops = %v, want exactly the abandoned server", f.stops.calls)
	}
	if _, ok := f.m.migrations.Get(f.old); ok {
		t.Errorf("abandoned record still present, want it taken")
	}
	if _, err := os.Stat(f.kept(f.m.defaultTestPoolRoot(t))); err != nil {
		t.Errorf("kept copy: %v, want it moved", err)
	}
}

func TestSetAsideKeepsEverythingWhenTheAbandonedServerDoesNotStop(t *testing.T) {
	f := newAbandonedFixture(t)
	f.m.migPorts = migration.NewPortAllocator(49152, 49153)
	port, err := f.m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	f.m.migrations.Put(&migration.Record{
		MigrationID: f.old, VMID: f.vmID, Role: migration.RoleTarget, Mode: migration.ModeOffline,
		Phase: migration.PhaseSetup, Port: port, NBD: &qemu.NBDServer{Pid: 7},
	})
	f.stops.err = errors.New("qemu-nbd did not exit")

	if _, err := f.m.StartIncoming(context.Background(), f.spec(f.old)); err == nil {
		t.Fatalf("StartIncoming = nil, want the stop error")
	}
	if rec, ok := f.m.migrations.Get(f.old); !ok || rec.Terminal() {
		t.Errorf("abandoned record = (%v, %v), want it kept non-terminal", rec.Phase, ok)
	}
	// Only the second port is free: the abandoned one stays held, and the
	// refused incoming reserved nothing.
	if p, err := f.m.migPorts.Reserve(); err != nil || p == port {
		t.Errorf("Reserve() = (%d, %v), want the other port", p, err)
	}
	if p, err := f.m.migPorts.Reserve(); err == nil {
		t.Errorf("Reserve() = %d, want no port left", p)
	}
	f.assertNotMoved(t)
}

func TestSetAsideStopsATerminalRecordsServerAgain(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("qemu-nbd did not exit")} {
		t.Run(func() string {
			if stopErr == nil {
				return "stopped"
			}
			return "stop fails"
		}(), func(t *testing.T) {
			f := newAbandonedFixture(t)
			srv := &qemu.NBDServer{Pid: 7}
			f.m.migrations.Put(&migration.Record{
				MigrationID: f.old, VMID: f.vmID, Role: migration.RoleTarget, Mode: migration.ModeOffline,
				Phase: migration.PhaseCancelled, NBD: srv,
			})
			f.stops.err = stopErr

			_, err := f.m.StartIncoming(context.Background(), f.spec(f.old))

			if f.stops.count() != 1 || f.stops.calls[0] != srv {
				t.Errorf("stops = %v, want the terminal record's server", f.stops.calls)
			}
			if stopErr == nil {
				if err != nil {
					t.Errorf("StartIncoming = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Errorf("StartIncoming = nil, want the stop error")
			}
			f.assertNotMoved(t)
		})
	}
}

func TestSetAsideRefusesWhenACopyIsAlreadyKept(t *testing.T) {
	f := newAbandonedFixture(t)
	other := filepath.Join(f.m.defaultTestPoolRoot(t), "abandoned", f.vmID.String()+"-"+uuid.NewString())
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := f.m.StartIncoming(context.Background(), f.spec(f.old))

	if !errors.Is(err, ErrAbandonedCopyKept) || !strings.Contains(err.Error(), other) {
		t.Errorf("StartIncoming = %v, want ErrAbandonedCopyKept naming %s", err, other)
	}
	if got, rerr := os.ReadFile(f.disk); rerr != nil || string(got) != abandonedBytes {
		t.Errorf("ReadFile(%s) = (%q, %v), want the copy untouched", f.disk, got, rerr)
	}
	if v, gerr := f.m.Get(f.vmID); gerr != nil || v.AdoptedBy != f.old {
		t.Errorf("Get = (%v, %v), want the copy still recorded", v.AdoptedBy, gerr)
	}
}

func TestSetAsideResumesAMoveACrashInterrupted(t *testing.T) {
	f := newAbandonedFixture(t)
	root := f.m.defaultTestPoolRoot(t)
	kept := f.kept(root)
	if err := os.MkdirAll(filepath.Dir(kept), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(filepath.Dir(f.disk), kept); err != nil {
		t.Fatalf("rename: %v", err)
	}
	metaPath := filepath.Join(f.m.stateDir, f.vmID.String(), state.MetaFileName)
	wantMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	s := f.spec(f.old)

	if _, err := f.m.StartIncoming(context.Background(), s); err != nil {
		t.Fatalf("StartIncoming = %v, want the interrupted move completed", err)
	}
	if got, err := os.ReadFile(filepath.Join(kept, state.MetaFileName)); err != nil || string(got) != string(wantMeta) {
		t.Errorf("kept meta.json = (%q, %v), want the original %q", got, err, wantMeta)
	}
	if v, err := f.m.Get(f.vmID); err != nil || v.AdoptedBy != s.MigrationID {
		t.Errorf("Get = (%v, %v), want the new adopt", v.AdoptedBy, err)
	}
	if got, err := os.ReadFile(filepath.Join(kept, "disk.qcow2")); err != nil || string(got) != abandonedBytes {
		t.Errorf("kept disk = (%q, %v), want the original bytes", got, err)
	}
}

func TestSetAsideAbortsWhenTheCopyIsNowhere(t *testing.T) {
	f := newAbandonedFixture(t)
	if err := os.RemoveAll(filepath.Dir(f.disk)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Another VM's kept copy, so the abandoned/ dir itself exists.
	if err := os.MkdirAll(filepath.Join(f.m.defaultTestPoolRoot(t), "abandoned", uuid.NewString()+"-"+uuid.NewString()), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := f.m.StartIncoming(context.Background(), f.spec(f.old)); err == nil {
		t.Fatalf("StartIncoming = nil, want an abort")
	}
	if v, err := f.m.Get(f.vmID); err != nil || v.AdoptedBy != f.old {
		t.Errorf("Get = (%v, %v), want the record unchanged", v.AdoptedBy, err)
	}
	if _, err := os.Stat(filepath.Join(f.m.stateDir, f.vmID.String(), state.MetaFileName)); err != nil {
		t.Errorf("meta.json: %v, want it kept", err)
	}
	if _, err := os.Stat(f.kept(f.m.defaultTestPoolRoot(t))); !os.IsNotExist(err) {
		t.Errorf("kept dir: stat err = %v, want none", err)
	}
}

func TestSetAsideUsesTheCopysOwnPool(t *testing.T) {
	m, _ := NewManagerForSeamTest(t)
	otherRoot := t.TempDir()
	if err := m.AddPool("other", otherRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	f := &abandonedFixture{m: m, vmID: uuid.New(), old: uuid.New(), stops: &stopNBDSpy{}}
	m.migStopNBD = f.stops.stop
	m.migQemuAlive = func(*VM) bool { return false }
	f.disk = f.adopt(t, "other")

	if _, err := m.StartIncoming(context.Background(), f.spec(f.old)); err != nil {
		t.Fatalf("StartIncoming = %v, want success", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.kept(otherRoot), "disk.qcow2")); err != nil || string(got) != abandonedBytes {
		t.Errorf("kept disk in the copy's pool = (%q, %v), want the original bytes", got, err)
	}
	if _, err := os.Stat(filepath.Join(m.defaultTestPoolRoot(t), "abandoned")); !os.IsNotExist(err) {
		t.Errorf("abandoned/ in the request's pool: stat err = %v, want none", err)
	}
}

func TestSetAsideRefusesAnUnexpectedLayout(t *testing.T) {
	f := newAbandonedFixture(t)
	odd := filepath.Join(f.m.defaultTestPoolRoot(t), "elsewhere", f.vmID.String(), "disk.qcow2")
	if err := os.MkdirAll(filepath.Dir(odd), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(odd, []byte(abandonedBytes), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.mutate(func(v *VM) { v.DiskPath = odd })
	f.disk = odd

	if _, err := f.m.StartIncoming(context.Background(), f.spec(f.old)); err == nil {
		t.Fatalf("StartIncoming = nil, want an abort on the unexpected layout")
	}
	f.assertNotMoved(t)
}

// TestSetAsideLeavesACopyClaimedWhileTheServerStops: the create path can claim
// the copy (clearing AdoptedBy) without the lifecycle slot, here while the
// abandoned server is being stopped; the copy is then the VM's own and stays.
func TestSetAsideLeavesACopyClaimedWhileTheServerStops(t *testing.T) {
	f := newAbandonedFixture(t)
	f.m.migrations.Put(&migration.Record{
		MigrationID: f.old, VMID: f.vmID, Role: migration.RoleTarget, Mode: migration.ModeOffline,
		Phase: migration.PhaseCancelled, NBD: &qemu.NBDServer{Pid: 7},
	})
	f.m.migStopNBD = func(*qemu.NBDServer, time.Duration) error {
		f.mutate(func(v *VM) { v.AdoptedBy = uuid.Nil })
		return nil
	}

	_, err := f.m.StartIncoming(context.Background(), f.spec(f.old))

	if err == nil || !strings.Contains(err.Error(), "already present") {
		t.Errorf("StartIncoming = %v, want already present", err)
	}
	if got, rerr := os.ReadFile(f.disk); rerr != nil || string(got) != abandonedBytes {
		t.Errorf("ReadFile(%s) = (%q, %v), want the claimed copy untouched", f.disk, got, rerr)
	}
}

func TestSetAsideRunsOnTheLivePath(t *testing.T) {
	f := newAbandonedFixture(t)
	f.m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error { return nil }
	f.m.migDialQMP = func(string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	s := f.spec(f.old)
	s.Mode = "live"

	if _, err := f.m.StartIncoming(context.Background(), s); err != nil {
		t.Fatalf("StartIncoming(live) = %v, want success", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.kept(f.m.defaultTestPoolRoot(t)), "disk.qcow2")); err != nil || string(got) != abandonedBytes {
		t.Errorf("kept disk = (%q, %v), want the original bytes", got, err)
	}
}

// TestSetAsideMovesNothingOnAReplay: a redelivered request of a migration this
// node already prepared is answered with its endpoints and moves nothing.
func TestSetAsideMovesNothingOnAReplay(t *testing.T) {
	f := newAbandonedFixture(t)
	s := f.spec(f.old)
	f.m.migrations.Put(&migration.Record{
		MigrationID: s.MigrationID, VMID: uuid.New(), Role: migration.RoleTarget, Mode: migration.ModeOffline,
		Phase: migration.PhaseCancelled, ListenEndpt: "10.0.0.2:49152", AuthToken: "tok",
	})

	res, err := f.m.StartIncoming(context.Background(), s)

	if err != nil || res.AuthToken != "tok" {
		t.Errorf("StartIncoming(replay) = (%+v, %v), want the minted endpoints", res, err)
	}
	f.assertNotMoved(t)
}

// TestSetAsideKeepsTheRecordWhenMetaJSONCannotBeRead: meta.json is the only
// recovery record of the moved copy, so a read failure other than "missing"
// aborts before the state dir is removed, and a retry once it reads again
// finishes the move with it kept.
func TestSetAsideKeepsTheRecordWhenMetaJSONCannotBeRead(t *testing.T) {
	f := newAbandonedFixture(t)
	metaPath := filepath.Join(f.m.stateDir, f.vmID.String(), state.MetaFileName)
	wantMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	// A directory in its place fails the read with EISDIR, even as root.
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(metaPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	kept := f.kept(f.m.defaultTestPoolRoot(t))

	if _, err := f.m.StartIncoming(context.Background(), f.spec(f.old)); err == nil {
		t.Fatalf("StartIncoming with an unreadable meta.json = nil, want an error")
	}
	if v, err := f.m.Get(f.vmID); err != nil || v.AdoptedBy != f.old {
		t.Errorf("Get = (%v, %v), want the record kept", v.AdoptedBy, err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Errorf("state dir meta.json: %v, want it kept", err)
	}

	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(metaPath, wantMeta, 0o600); err != nil {
		t.Fatalf("restore meta.json: %v", err)
	}
	s := f.spec(f.old)
	if _, err := f.m.StartIncoming(context.Background(), s); err != nil {
		t.Fatalf("retry StartIncoming = %v, want the move completed", err)
	}
	if got, err := os.ReadFile(filepath.Join(kept, state.MetaFileName)); err != nil || string(got) != string(wantMeta) {
		t.Errorf("kept meta.json = (%q, %v), want the original %q", got, err, wantMeta)
	}
	if got, err := os.ReadFile(filepath.Join(kept, "disk.qcow2")); err != nil || string(got) != abandonedBytes {
		t.Errorf("kept disk = (%q, %v), want the original bytes", got, err)
	}
}
