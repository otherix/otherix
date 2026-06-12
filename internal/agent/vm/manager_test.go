// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/state"
	"github.com/otherix/otherix/internal/config"
)

func newTestConfig(t *testing.T) (*config.AgentConfig, string, string) {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "vms")
	poolRoot := filepath.Join(tmp, "pools", "default")
	poolName := "default"
	cfg := &config.AgentConfig{
		StatePath: stateDir,
		QEMU: config.QEMUConfig{
			AArch64FirmwarePath: "/usr/share/AAVMF/AAVMF_CODE.fd",
		},
	}
	return cfg, poolRoot, poolName
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestManager_New_ReattachesMuxForRunningVM pins the agent-restart
// recovery contract: a VM that was running when the agent stopped has
// its serial multiplexer RE-ATTACHED on the next New(), so `vm console`
// and `vm logs` keep working without restarting the VM. Before this the
// mux was attached only on create/start/reboot, so an agent restart
// silently broke console/logs for every running VM (GetMux returned nil
// and both endpoints reported "restart the vm to re-enable") until the
// VM itself was rebooted. A stopped VM gets no multiplexer.
func TestManager_New_ReattachesMuxForRunningVM(t *testing.T) {
	cfg, _, _ := newTestConfig(t)

	// Short-pathed unix socket standing in for the running qemu's
	// -serial chardev. The real console.sock under the state dir would
	// overflow the ~104-char sun_path limit on some platforms, so the
	// fake lives under /tmp.
	sockDir, err := os.MkdirTemp("/tmp", "oxsock")
	if err != nil {
		t.Skipf("cannot create short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "c.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) }(c)
		}
	}()

	writeMeta := func(name string, status Status, sock string) {
		t.Helper()
		id := uuid.New()
		if werr := state.WriteMeta(filepath.Join(cfg.StatePath, id.String()), &state.VMMeta{
			VMID:          id,
			Name:          name,
			VCPUs:         2,
			MemoryMB:      1024,
			PoolName:      "default",
			Architecture:  string(qemu.HostArch()),
			ConsoleSocket: sock,
			Status:        string(status),
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		}); werr != nil {
			t.Fatalf("WriteMeta(%s): %v", name, werr)
		}
	}

	writeMeta("live-vm", StatusRunning, sockPath)
	writeMeta("idle-vm", StatusStopped, filepath.Join(sockDir, "absent.sock"))

	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.GetMux("live-vm") == nil {
		t.Error("GetMux(live-vm) = nil, want a re-attached multiplexer for the running VM")
	}
	if m.GetMux("idle-vm") != nil {
		t.Error("GetMux(idle-vm) != nil, want no multiplexer for a stopped VM")
	}
}

func TestManager_New_ValidatesStatePath(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*config.AgentConfig)
		wantErr bool
	}{
		{"empty state path", func(c *config.AgentConfig) { c.StatePath = "" }, true},
		{"valid", func(*config.AgentConfig) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTestConfig(t)
			tc.mut(cfg)
			_, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
			if tc.wantErr && err == nil {
				t.Fatalf("New(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestManager_AddPool_Validates(t *testing.T) {
	cases := []struct {
		name    string
		pool    string
		root    func(t *testing.T) string
		wantErr bool
	}{
		{"empty name", "", func(*testing.T) string { return "/tmp" }, true},
		{"empty root", "p", func(*testing.T) string { return "" }, true},
		{"relative root", "p", func(*testing.T) string { return "relative" }, true},
		{"valid", "p", func(t *testing.T) string { return t.TempDir() }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTestConfig(t)
			m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = m.AddPool(tc.pool, tc.root(t))
			if tc.wantErr && err == nil {
				t.Fatalf("AddPool(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("AddPool(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestManager_Create_ValidationErrors(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	const imageURL = "https://example.test/ubuntu.img"
	cases := []struct {
		name string
		spec CreateSpec
	}{
		{"empty name", CreateSpec{Name: "", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, ImageURL: imageURL}},
		{"low vcpus", CreateSpec{Name: "x", VCPUs: 0, MemoryMB: 1024, PoolName: poolName, ImageURL: imageURL}},
		{"high vcpus", CreateSpec{Name: "x", VCPUs: 200, MemoryMB: 1024, PoolName: poolName, ImageURL: imageURL}},
		{"low memory", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 64, PoolName: poolName, ImageURL: imageURL}},
		{"empty image url", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, ImageURL: ""}},
		{"short expected sha", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, ImageURL: imageURL, ExpectedSHA256: "deadbeef"}},
		{"empty pool", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: "", ImageURL: imageURL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Create(t.Context(), tc.spec)
			if err == nil {
				t.Fatalf("Create(%s) = nil, want error", tc.name)
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("Create(%s) error = %v, want ErrInvalidSpec", tc.name, err)
			}
		})
	}
}

func TestManager_Create_UnknownPool(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	_, err = m.Create(t.Context(), CreateSpec{
		Name:     "x",
		VCPUs:    2,
		MemoryMB: 1024,
		PoolName: "not-the-configured-pool",
		ImageURL: "https://example.test/ubuntu.img",
	})
	if !errors.Is(err, ErrPoolUnknown) {
		t.Errorf("err = %v, want ErrPoolUnknown", err)
	}
}

// TestManager_Create_DuplicateVMID_IsIdempotent confirms that a second
// Create for a vmID already present does NOT overwrite the in-flight VM
// entry or re-materialise its NICs. The CP mints a fresh idempotency key
// per attempt and the agent has no idempotency middleware, so a job
// redelivered before the CP persisted the agent_task_id re-POSTs and would
// otherwise clobber the live VM (tap churn / state loss). The duplicate
// must re-accept the original create task so the CP worker resumes against
// the same agent_task_id.
func TestManager_Create_DuplicateVMID_IsIdempotent(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	fab := &netfabric.FakeFabric{}
	m, err := New(cfg, fab, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	vmID := uuid.New()
	nic := sampleNIC()

	// Seed an in-flight VM the way the first Create would, plus its create
	// task, without racing the async runCreate goroutine.
	original := &VM{
		ID:           vmID,
		Name:         "live-vm",
		PoolName:     poolName,
		Architecture: qemu.HostArch(),
		Status:       StatusCreating,
		NICs:         []netfabric.NIC{nic},
	}
	origTask := m.tasks.Create(TaskKindVMCreate, vmID)
	m.mu.Lock()
	m.vms[vmID] = original
	m.createTasks[vmID] = origTask.ID
	m.mu.Unlock()

	task, err := m.Create(t.Context(), CreateSpec{
		UUID:     vmID,
		Name:     "live-vm",
		VCPUs:    2,
		MemoryMB: 1024,
		PoolName: poolName,
		ImageURL: "https://example.test/ubuntu.img",
		NICs:     []netfabric.NIC{nic},
	})
	if err != nil {
		t.Fatalf("duplicate Create = %v, want idempotent re-accept", err)
	}

	// The original create task is re-accepted so the CP worker resumes
	// against the same agent_task_id.
	if task.ID != origTask.ID {
		t.Errorf("duplicate Create task ID = %s, want original %s", task.ID, origTask.ID)
	}

	// The in-flight VM entry must be the SAME pointer: not overwritten.
	m.mu.Lock()
	got := m.vms[vmID]
	m.mu.Unlock()
	if got != original {
		t.Errorf("vms[%s] pointer changed; duplicate Create clobbered the in-flight VM", vmID)
	}

	// No second materialiseNICs: the duplicate must not spawn a runCreate,
	// so the fabric saw no tap churn.
	if len(fab.CreateTapCalls) != 0 || len(fab.AttachTapCalls) != 0 {
		t.Errorf("duplicate Create touched the fabric: createTap=%v attachTap=%v",
			fab.CreateTapCalls, fab.AttachTapCalls)
	}
}

// TestManager_Create_DuplicateRunningVM_NoCreateTask models a post-restart
// redelivery: m.vms is reloaded from disk (so a completed VM is present) but
// m.createTasks is empty (rebuilt empty on restart). A redelivered vm.create
// for that vmID must return an idempotent SUCCESS task reflecting the reloaded
// status WITHOUT overwriting the live VM record or spawning a second runCreate
// that would clobber its disk and taps.
func TestManager_Create_DuplicateRunningVM_NoCreateTask(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	fab := &netfabric.FakeFabric{}
	m, err := New(cfg, fab, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	vmID := uuid.New()
	// A reloaded, completed VM with NO live create task (createTasks empty).
	orig := &VM{
		ID:           vmID,
		Name:         "vm-x",
		PoolName:     poolName,
		Architecture: qemu.HostArch(),
		Status:       StatusRunning,
		DiskPath:     "/orig/disk.qcow2",
	}
	m.mu.Lock()
	m.vms[vmID] = orig
	m.mu.Unlock()

	task, err := m.Create(t.Context(), CreateSpec{
		UUID:     vmID,
		Name:     "vm-x",
		VCPUs:    2,
		MemoryMB: 1024,
		PoolName: poolName,
		ImageURL: "https://example.test/ubuntu.img",
	})
	if err != nil {
		t.Fatalf("Create(dup running) = %v, want nil", err)
	}
	if task.Status != TaskStatusSuccess {
		t.Errorf("idempotent task status = %q, want %q", task.Status, TaskStatusSuccess)
	}

	m.mu.Lock()
	got := m.vms[vmID]
	m.mu.Unlock()
	if got != orig || got.Status != StatusRunning || got.DiskPath != "/orig/disk.qcow2" {
		t.Errorf("live VM was overwritten: got %+v", got)
	}
	if len(fab.CreateTapCalls) != 0 || len(fab.AttachTapCalls) != 0 {
		t.Errorf("redelivered Create spawned a runCreate: createTap=%v attachTap=%v",
			fab.CreateTapCalls, fab.AttachTapCalls)
	}
}

// TestManager_Create_DuplicateFailedVM_NoCreateTask: a reloaded VM in failed
// status (create genuinely failed before) redelivered with createTasks empty
// returns an idempotent FAILED task without overwriting the record.
func TestManager_Create_DuplicateFailedVM_NoCreateTask(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	fab := &netfabric.FakeFabric{}
	m, err := New(cfg, fab, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	vmID := uuid.New()
	orig := &VM{
		ID:           vmID,
		Name:         "vm-x",
		PoolName:     poolName,
		Architecture: qemu.HostArch(),
		Status:       StatusFailed,
		DiskPath:     "/orig/disk.qcow2",
	}
	m.mu.Lock()
	m.vms[vmID] = orig
	m.mu.Unlock()

	task, err := m.Create(t.Context(), CreateSpec{
		UUID:     vmID,
		Name:     "vm-x",
		VCPUs:    2,
		MemoryMB: 1024,
		PoolName: poolName,
		ImageURL: "https://example.test/ubuntu.img",
	})
	if err != nil {
		t.Fatalf("Create(dup failed) = %v, want nil", err)
	}
	if task.Status != TaskStatusFailed {
		t.Errorf("idempotent task status = %q, want %q", task.Status, TaskStatusFailed)
	}
	if task.Error == nil || task.Error.Code != "vm_create_failed" {
		t.Errorf("task error = %+v, want code vm_create_failed", task.Error)
	}
	m.mu.Lock()
	got := m.vms[vmID]
	m.mu.Unlock()
	if got != orig {
		t.Errorf("live VM was overwritten on duplicate failed create")
	}
	if len(fab.CreateTapCalls) != 0 || len(fab.AttachTapCalls) != 0 {
		t.Errorf("redelivered Create spawned a runCreate: createTap=%v attachTap=%v",
			fab.CreateTapCalls, fab.AttachTapCalls)
	}
}

// TestManager_Create_DuplicateInterruptedVM_NoCreateTask: a reloaded VM stuck
// in an in-progress status (creating) with no live create task was interrupted
// mid-create (crash). The redelivery reports a clean terminal failed task
// (vm_create_interrupted) rather than re-spawning a runCreate that could clobber.
func TestManager_Create_DuplicateInterruptedVM_NoCreateTask(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	fab := &netfabric.FakeFabric{}
	m, err := New(cfg, fab, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	vmID := uuid.New()
	orig := &VM{
		ID:           vmID,
		Name:         "vm-x",
		PoolName:     poolName,
		Architecture: qemu.HostArch(),
		Status:       StatusCreating,
		DiskPath:     "/orig/disk.qcow2",
	}
	m.mu.Lock()
	m.vms[vmID] = orig
	m.mu.Unlock()

	task, err := m.Create(t.Context(), CreateSpec{
		UUID:     vmID,
		Name:     "vm-x",
		VCPUs:    2,
		MemoryMB: 1024,
		PoolName: poolName,
		ImageURL: "https://example.test/ubuntu.img",
	})
	if err != nil {
		t.Fatalf("Create(dup interrupted) = %v, want nil", err)
	}
	if task.Status != TaskStatusFailed {
		t.Errorf("idempotent task status = %q, want %q", task.Status, TaskStatusFailed)
	}
	if task.Error == nil || task.Error.Code != "vm_create_interrupted" {
		t.Errorf("task error = %+v, want code vm_create_interrupted", task.Error)
	}
	m.mu.Lock()
	got := m.vms[vmID]
	m.mu.Unlock()
	if got != orig {
		t.Errorf("live VM was overwritten on duplicate interrupted create")
	}
	if len(fab.CreateTapCalls) != 0 || len(fab.AttachTapCalls) != 0 {
		t.Errorf("redelivered Create spawned a runCreate: createTap=%v attachTap=%v",
			fab.CreateTapCalls, fab.AttachTapCalls)
	}
}

// waitTaskTerminal polls the task store until the task reaches a terminal
// status (success / failed) or the deadline elapses. Returns the terminal
// task. Used by the create-flow tests that drive the async runCreate
// goroutine.
func waitTaskTerminal(t *testing.T, m *Manager, taskID uuid.UUID) *AgentTask {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		task := m.tasks.Get(taskID)
		if task != nil && (task.Status == TaskStatusSuccess || task.Status == TaskStatusFailed) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach a terminal status within the deadline", taskID)
	return nil
}

// realQcow2 uses qemu-img to create a qcow2 file with the given virtual
// size (in bytes) and returns its bytes, so a test httptest.Server can
// serve a real image whose virtual size qemu-img info will report. Skips
// the test when qemu-img is absent.
func realQcow2(t *testing.T, virtualBytes int64) []byte {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "src.qcow2")
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", dst, strconv.FormatInt(virtualBytes, 10))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %v (%s)", err, out)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read created qcow2: %v", err)
	}
	return b
}

// TestManager_Create_DiskTooSmall_Rejected drives the real Create flow:
// EnsureImage downloads the served qcow2 (2 GiB virtual size), CloneImage
// copies it to the per-VM disk, qemu-img info reports the virtual size,
// and a DiskGiB below that virtual size fails the task with code
// disk_too_small BEFORE any resize. Needs real qemu-img; skips cleanly
// when the binary is absent.
func TestManager_Create_DiskTooSmall_Rejected(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH; sizing step is uncovered in this env")
	}
	m, poolName, _ := newImageTestManager(t)

	body := realQcow2(t, 2*1073741824) // 2 GiB virtual size
	url := serve(t, body)

	task, err := m.Create(t.Context(), CreateSpec{
		Name:     "too-small-vm",
		VCPUs:    1,
		MemoryMB: 256,
		PoolName: poolName,
		ImageURL: url,
		Format:   "qcow2",
		DiskGiB:  1, // 1 GiB < 2 GiB virtual size -> reject
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	term := waitTaskTerminal(t, m, task.ID)
	if term.Status != TaskStatusFailed {
		t.Fatalf("task status = %q, want %q", term.Status, TaskStatusFailed)
	}
	if term.Error == nil || term.Error.Code != "disk_too_small" {
		t.Fatalf("task error = %+v, want code disk_too_small", term.Error)
	}
}

// TestManager_Create_EnsuresImageAndSizes drives Create with DiskGiB=0:
// EnsureImage downloads the served qcow2, CloneImage copies it, qemu-img
// info reports the virtual size, and the disk defaults to that virtual
// size (no resize). The flow then fails at the qemu spawn step (no real
// guest in unit tests), but NOT for an image/sizing reason - proving the
// inline ensure + clone + info all ran against the served URL. Needs real
// qemu-img; skips cleanly when absent.
func TestManager_Create_EnsuresImageAndSizes(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH; sizing step is uncovered in this env")
	}
	m, poolName, _ := newImageTestManager(t)

	body := realQcow2(t, 1073741824) // 1 GiB virtual size
	wantSHA := shaHex(body)
	url := serve(t, body)

	task, err := m.Create(t.Context(), CreateSpec{
		Name:           "sizing-vm",
		VCPUs:          1,
		MemoryMB:       256,
		PoolName:       poolName,
		ImageURL:       url,
		ExpectedSHA256: wantSHA,
		Format:         "qcow2",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	term := waitTaskTerminal(t, m, task.ID)
	if term.Status != TaskStatusFailed {
		// Reaching success would require a real qemu boot; in this env the
		// spawn step fails. Either way it must not fail on the image path.
		return
	}
	if term.Error == nil {
		t.Fatal("terminal failed task Error = nil, want populated")
	}
	switch term.Error.Code {
	case "image_unavailable", "checksum_mismatch", "clone_failed", "disk_too_small":
		t.Fatalf("create failed on the image/sizing path with code %q (%s); ensure+clone+info did not all run",
			term.Error.Code, term.Error.Message)
	}
}

// TestManager_Create_BadChecksum_FailsTask drives Create with an
// ExpectedSHA256 that does not match the served body, asserting the task
// fails with code checksum_mismatch. This path is independent of qemu-img
// (EnsureImage rejects before clone/sizing), so it always runs.
func TestManager_Create_BadChecksum_FailsTask(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)

	body := qcow2Body(0x22)
	url := serve(t, body)
	wrongSHA := shaHex(qcow2Body(0x33))

	task, err := m.Create(t.Context(), CreateSpec{
		Name:           "bad-sha-vm",
		VCPUs:          1,
		MemoryMB:       256,
		PoolName:       poolName,
		ImageURL:       url,
		ExpectedSHA256: wrongSHA,
		Format:         "qcow2",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	term := waitTaskTerminal(t, m, task.ID)
	if term.Status != TaskStatusFailed {
		t.Fatalf("task status = %q, want %q", term.Status, TaskStatusFailed)
	}
	if term.Error == nil || term.Error.Code != "checksum_mismatch" {
		t.Fatalf("task error = %+v, want code checksum_mismatch", term.Error)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Get(uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestManager_List_Empty(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := m.List(); len(got) != 0 {
		t.Errorf("List() = %d entries, want 0", len(got))
	}
}

func TestManager_InFlightGuard_AcquireReleaseAndQuery(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = true before any acquire")
	}
	release, ok := m.inFlightAcquire("foo")
	if !ok {
		t.Fatalf("first acquire on foo failed")
	}
	if !m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = false after acquire")
	}
	if _, ok := m.inFlightAcquire("foo"); ok {
		t.Errorf("second acquire on foo succeeded (must reject)")
	}
	if _, ok := m.inFlightAcquire("bar"); !ok {
		t.Errorf("acquire on independent name bar failed")
	}
	release()
	if m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = true after release")
	}
	if _, ok := m.inFlightAcquire("foo"); !ok {
		t.Errorf("re-acquire on foo after release failed")
	}
}

// TestManager_FailTaskOnly_DoesNotMutateVMStatus pins the contract of
// failTaskOnly: it marks the task as failed but leaves the VM's
// persisted status unchanged. Used by stop / reboot timeout paths
// where the VM is still running (qemu refused to honour ACPI); the
// task surfaces the failure but the VM itself is not in StatusFailed.
func TestManager_FailTaskOnly_DoesNotMutateVMStatus(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vmID := uuid.New()
	m.mu.Lock()
	m.vms[vmID] = &VM{
		ID:     vmID,
		Name:   "test-vm",
		Status: StatusRunning,
	}
	m.mu.Unlock()

	task := m.tasks.Create(TaskKindVMStop, vmID)
	m.failTaskOnly(task.ID, "stop_timeout", "guest ignored ACPI")

	got := m.tasks.Get(task.ID)
	if got == nil {
		t.Fatal("task not found after failTaskOnly")
	}
	if got.Status != TaskStatusFailed {
		t.Errorf("task.Status = %q, want %q", got.Status, TaskStatusFailed)
	}
	if got.Error == nil {
		t.Fatal("task.Error = nil, want populated")
	}
	if got.Error.Code != "stop_timeout" || got.Error.Message != "guest ignored ACPI" {
		t.Errorf("task.Error = {%q, %q}, want {%q, %q}",
			got.Error.Code, got.Error.Message, "stop_timeout", "guest ignored ACPI")
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		t.Fatalf("snapshotVM: %v", err)
	}
	if v.Status != StatusRunning {
		t.Errorf("VM status mutated by failTaskOnly: got %q, want %q (unchanged)",
			v.Status, StatusRunning)
	}
}

// TestManager_FailTask_MutatesVMStatusToFailed locks the existing
// semantics of failTask: VM transitions to StatusFailed AND task is
// recorded as failed. Reserved for paths where the VM legitimately
// entered a failed state (create / start spawn failures, stuck-qemu
// poweroff escalation).
func TestManager_FailTask_MutatesVMStatusToFailed(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vmID := uuid.New()
	m.mu.Lock()
	m.vms[vmID] = &VM{
		ID:     vmID,
		Name:   "test-vm",
		Status: StatusCreating,
	}
	m.mu.Unlock()

	task := m.tasks.Create(TaskKindVMCreate, vmID)
	m.failTask(task.ID, vmID, "qemu_spawn_failed", "boom")

	got := m.tasks.Get(task.ID)
	if got == nil {
		t.Fatal("task not found after failTask")
	}
	if got.Status != TaskStatusFailed {
		t.Errorf("task.Status = %q, want %q", got.Status, TaskStatusFailed)
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		t.Fatalf("snapshotVM: %v", err)
	}
	if v.Status != StatusFailed {
		t.Errorf("VM status = %q, want %q (failTask must mark VM failed)",
			v.Status, StatusFailed)
	}
}

// TestManager_New_SweepsOrphanTaps locks the startup orphan-tap sweep:
// taps the host reports that do NOT belong to any replayed VM are
// reclaimed; taps that DO belong to a replayed VM are left alone.
func TestManager_New_SweepsOrphanTaps(t *testing.T) {
	cfg, _, _ := newTestConfig(t)

	// A replayed VM whose single NIC implies a kept tap.
	vmID := uuid.New()
	nic := sampleNIC()
	keepTap := nic.TapName()
	meta := &state.VMMeta{
		VMID:         vmID,
		Name:         "kept-vm",
		VCPUs:        2,
		MemoryMB:     1024,
		PoolName:     "default",
		Architecture: string(qemu.HostArch()),
		Status:       string(StatusStopped),
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		NICs:         nicsToMeta([]netfabric.NIC{nic}),
	}
	if err := state.WriteMeta(filepath.Join(cfg.StatePath, vmID.String()), meta); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	orphanTap := "otdeadbeef0000"
	fake := &netfabric.FakeFabric{ListTapsResult: []string{keepTap, orphanTap}}

	if _, err := New(cfg, fake, discardLogger()); err != nil {
		t.Fatalf("New: %v", err)
	}

	if fake.ListTapsCalls != 1 {
		t.Errorf("ListTapsCalls = %d, want 1", fake.ListTapsCalls)
	}
	want := []string{orphanTap}
	if diff := cmp.Diff(want, fake.DeleteTapCalls); diff != "" {
		t.Errorf("DeleteTapCalls mismatch (-want +got):\n%s", diff)
	}
}

// TestManager_New_SweepsOrphanTaps_NoVMs covers the VM-less manager: a
// host tap with no owning VM is reclaimed.
func TestManager_New_SweepsOrphanTaps_NoVMs(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	orphanTap := "otcccccccc0000"
	fake := &netfabric.FakeFabric{ListTapsResult: []string{orphanTap}}

	if _, err := New(cfg, fake, discardLogger()); err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []string{orphanTap}
	if diff := cmp.Diff(want, fake.DeleteTapCalls); diff != "" {
		t.Errorf("DeleteTapCalls mismatch (-want +got):\n%s", diff)
	}
}

// TestManager_New_OrphanSweep_ToleratesListTapsError ensures a ListTaps
// failure skips the sweep without failing manager startup.
func TestManager_New_OrphanSweep_ToleratesListTapsError(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	fake := &netfabric.FakeFabric{Errs: map[string]error{"ListTaps": errors.New("boom")}}

	m, err := New(cfg, fake, discardLogger())
	if err != nil {
		t.Fatalf("New must not fail on ListTaps error: %v", err)
	}
	if m == nil {
		t.Fatal("New returned nil manager")
	}
	if len(fake.DeleteTapCalls) != 0 {
		t.Errorf("DeleteTapCalls = %v, want none (sweep skipped on ListTaps error)", fake.DeleteTapCalls)
	}
}

func TestManager_InFlightGuard_EmptyName_IsNoOp(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	release, ok := m.inFlightAcquire("")
	if !ok {
		t.Errorf("empty name acquire returned ok=false (must be no-op true)")
	}
	if m.HasInFlight("") {
		t.Errorf("HasInFlight(\"\") = true (must be no-op false)")
	}
	release()
}
