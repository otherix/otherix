// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/cloudinit"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/state"
	"github.com/otherix/otherix/internal/agent/storage"
	"github.com/otherix/otherix/internal/config"
)

// Errors returned by Manager methods. Sentinel errors so handlers can
// branch on errors.Is for envelope mapping.
var (
	ErrNotFound        = errors.New("vm not found")
	ErrPoolUnknown     = errors.New("pool name does not match a configured pool")
	ErrTemplateMissing = errors.New("template not found on pool")
	ErrInvalidSpec     = errors.New("invalid create spec")

	// ErrInvalidState is returned by sync lifecycle ops (Pause /
	// Resume / Reset) when the VM is not in а phase that accepts
	// the requested transition (e.g. resume-when-running,
	// pause-when-stopped). Handlers map this к 409 conflict.
	ErrInvalidState = errors.New("vm not in a valid state for this operation")

	// ErrQMPUnavailable wraps а failed QMP socket dial or command
	// dispatch. Handlers map this к 500 — the operator can re-
	// invoke the action; the agent does not retry автоматически
	// (Area 4-IV lock).
	ErrQMPUnavailable = errors.New("qmp socket unavailable or rejected command")

	// ErrInFlight is returned by async lifecycle entrypoints when the
	// same VM (by name) already has an operation in progress. Locked
	// via inFlight sync.Map[name]struct{} per L3 D2: prevents the VM
	// reconciler от racing concurrent Start / Stop / Poweroff /
	// Reboot / Delete на the same VM. Handlers map this к 409
	// conflict; the reconciler reads HasInFlight before enqueuing
	// corrective ops к short-circuit без the sentinel round-trip.
	ErrInFlight = errors.New("vm operation already in flight")
)

// shutdownGrace bounds how long Delete и Stop wait for system_powerdown
// to take effect. For Stop the timeout is а task-failure trigger (Area
// 4-II lock — graceful only; operator dispatches к poweroff via CLI
// --force on timeout). For Delete the timeout cascades through Quit /
// SIGKILL.
const shutdownGrace = 60 * time.Second

// poweroffGrace bounds how long Poweroff waits for QMP `quit` to take
// effect before resorting к SIGKILL. Short by design — poweroff is the
// force path; operators reach for it when graceful shutdown would not
// suffice.
const poweroffGrace = 5 * time.Second

// pool is the agent's name-keyed view of one storage pool. The
// registry is name-keyed; the cluster-wide UUID stays at the CP
// edge's storage-pool carve-out.
type pool struct {
	name string
	root string
}

// Manager is the agent-side VM lifecycle orchestrator. One instance per
// agent process. All state is in-memory; Manager.New rebuilds it from
// disk on startup.
//
// The pool registry has its own RWMutex distinct от `mu` (which
// serialises VM-state changes). Pool churn driven by the reconciler
// must not block VM ops, и VM op pool lookups want only а read lock.
type Manager struct {
	log             *slog.Logger
	stateDir        string
	aarch64Firmware string
	accelerator     string

	mu    sync.Mutex
	vms   map[uuid.UUID]*VM
	tasks *TaskStore

	poolsMu sync.RWMutex
	pools   map[string]pool

	// imageLocks serialises concurrent storage-image work on the same
	// (pool, sha256). Keys are imageLockKey, values are *sync.Mutex.
	// The map grows monotonically — bounded по active distinct content
	// the operator imports. Cleanup is а future iteration concern (see
	// ROADMAP). sync.Map's zero value is usable so no init is needed.
	imageLocks sync.Map

	// inFlight tracks in-progress lifecycle operations keyed by VM
	// name. Entry value is struct{}; presence alone signals "busy".
	// Loaded into via LoadOrStore at Start / Stop / Poweroff /
	// Reboot / Delete entry; Deleted by the goroutine on completion
	// (regardless of success / fail). HasInFlight surfaces the
	// state к the reconciler so it can skip enqueuing duplicates.
	inFlight sync.Map
}

// inFlightAcquire records а new in-flight operation for name. Returns
// (release, ok) — ok=true когда the slot was free и the caller may
// proceed (must call release когда the goroutine ends); ok=false когда
// а prior operation is still resident, в which case the caller must
// reject с ErrInFlight без spawning work.
func (m *Manager) inFlightAcquire(name string) (release func(), ok bool) {
	if name == "" {
		return func() {}, true
	}
	if _, loaded := m.inFlight.LoadOrStore(name, struct{}{}); loaded {
		return nil, false
	}
	return func() { m.inFlight.Delete(name) }, true
}

// HasInFlight reports whether а lifecycle operation is currently
// running on name. Read-only — does not register а new operation. The
// VM reconciler uses this к skip diff-driven corrections когда the
// previous tick's enqueue is still in progress.
func (m *Manager) HasInFlight(name string) bool {
	if name == "" {
		return false
	}
	_, ok := m.inFlight.Load(name)
	return ok
}

// PoolView is the read-only projection of а registered pool. Consumed
// by the reconciler's diff loop AND by future external introspection
// (e.g. an admin endpoint listing locally-materialised pools). The
// internal `pool` struct stays unexported.
type PoolView struct {
	Name string
	Root string
}

// New constructs a Manager. The pool registry starts **empty** — the
// reconciler populates it от desired-state delivered through
// heartbeat. The constructor only ensures the state directory и
// replays existing meta.json files. VM ops that reference а pool not
// yet reconciled return ErrPoolUnknown until the reconciler lands the
// entry; the first heartbeat tick typically closes the window.
//
// Probing live qemu processes happens lazily on Get / List rather
// than during startup so transient probe failures do not block agent
// boot.
func New(cfg *config.AgentConfig, log *slog.Logger) (*Manager, error) {
	if cfg.StatePath == "" {
		return nil, fmt.Errorf("state_path is required")
	}
	if err := ensureWritableDir(cfg.StatePath); err != nil {
		return nil, fmt.Errorf("state_path: %w", err)
	}

	accelerator := qemu.DetectAccelerator()
	log.Info("qemu accelerator selected", "accelerator", accelerator)

	m := &Manager{
		log:             log,
		stateDir:        cfg.StatePath,
		pools:           map[string]pool{},
		aarch64Firmware: cfg.QEMU.AArch64FirmwarePath,
		accelerator:     accelerator,
		vms:             map[uuid.UUID]*VM{},
		tasks:           NewTaskStore(),
	}

	metas, err := state.ScanState(cfg.StatePath, log)
	if err != nil {
		return nil, fmt.Errorf("scan state: %w", err)
	}
	for _, meta := range metas {
		v := &VM{
			ID:            meta.VMID,
			Name:          meta.Name,
			VCPUs:         meta.VCPUs,
			MemoryMB:      meta.MemoryMB,
			PoolName:      meta.PoolName,
			Architecture:  qemu.Architecture(meta.Architecture),
			Status:        Status(meta.Status),
			CreatedAt:     meta.CreatedAt,
			UpdatedAt:     meta.UpdatedAt,
			DiskPath:      meta.DiskPath,
			QMPSocket:     meta.QMPSocket,
			ConsoleSocket: meta.ConsoleSocket,
			PIDFile:       meta.PIDFile,
			CidataPath:    meta.CidataPath,
		}
		m.vms[v.ID] = v
	}
	log.Info("vm manager initialized",
		"state_dir", cfg.StatePath,
		"recovered_vms", len(m.vms),
		"note", "pool registry empty; reconciler populates via heartbeat",
	)
	return m, nil
}

// AddPool registers а pool в the in-memory registry. The reconciler
// invokes this when а declared_pools entry arrives that the agent
// has not yet materialised. The function is idempotent - re-adding
// an identical (name, root) pair is а no-op; re-adding with а
// different root replaces the registry entry but does **not** move
// existing files on disk (operator responsibility).
//
// Filesystem-side: ensures root и the conventional pool subdirs
// (templates/, scratch/import/) exist и are writable. Errors here
// propagate up к the reconciler, which records а `failed` outcome on
// the next heartbeat.
func (m *Manager) AddPool(name, root string) error {
	if name == "" {
		return fmt.Errorf("pool: name is required")
	}
	if root == "" {
		return fmt.Errorf("pool %q: root is required", name)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("pool %q: root must be absolute (got %q)", name, root)
	}
	if err := ensureWritableDir(root); err != nil {
		return fmt.Errorf("pool %q root: %w", name, err)
	}
	if err := ensurePoolSubdirs(root); err != nil {
		return fmt.Errorf("pool %q subdirs: %w", name, err)
	}
	m.poolsMu.Lock()
	m.pools[name] = pool{name: name, root: root}
	m.poolsMu.Unlock()
	return nil
}

// RemovePool drops а pool от the in-memory registry. Per SL11 the
// filesystem is left intact — operator responsibility to reclaim
// space if desired. Subsequent VM ops referencing this pool name
// return ErrPoolUnknown.
func (m *Manager) RemovePool(name string) {
	m.poolsMu.Lock()
	delete(m.pools, name)
	m.poolsMu.Unlock()
}

// ListPools returns а snapshot of the registered pools. Order is not
// guaranteed (map iteration); callers that want deterministic order
// sort by Name. Read-only — mutations require AddPool / RemovePool.
func (m *Manager) ListPools() []PoolView {
	m.poolsMu.RLock()
	defer m.poolsMu.RUnlock()
	out := make([]PoolView, 0, len(m.pools))
	for _, p := range m.pools {
		out = append(out, PoolView{Name: p.name, Root: p.root})
	}
	return out
}

func ensureWritableDir(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	probe := filepath.Join(path, ".otherix-write-probe")
	if err := os.WriteFile(probe, []byte{}, 0o600); err != nil {
		return fmt.Errorf("write probe in %s: %w", path, err)
	}
	_ = os.Remove(probe)
	return nil
}

// ensurePoolSubdirs creates the per-pool subdirectories the storage-
// image surface depends on: `templates/` for committed image files
// (the path Manager.Create clones from) и `scratch/import/` for
// in-progress downloads. Idempotent — MkdirAll returns nil on
// existing directories.
func ensurePoolSubdirs(root string) error {
	for _, sub := range []string{
		filepath.Join(root, "templates"),
		filepath.Join(root, "scratch", "import"),
	} {
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return nil
}

// Get returns a snapshot of the VM identified by id. Returns ErrNotFound
// when no such VM exists. The Status field reflects what's persisted on
// disk plus a fresh process-supervision probe so callers see live state.
func (m *Manager) Get(id uuid.UUID) (*VM, error) {
	m.mu.Lock()
	v, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	cp := *v
	m.mu.Unlock()
	cp.Status = m.observedStatus(&cp)
	return &cp, nil
}

// GetByName returns a snapshot of the VM identified by name. Per
// Pre-L1 Path D rekey the agent's wire surface addresses VMs by
// name; this is the resolver the handlers hit on every name-keyed
// request. Returns ErrNotFound when no live (non-deleted) entry
// matches.
func (m *Manager) GetByName(name string) (*VM, error) {
	m.mu.Lock()
	var found *VM
	for _, v := range m.vms {
		if v.Name == name {
			cp := *v
			found = &cp
			break
		}
	}
	m.mu.Unlock()
	if found == nil {
		return nil, ErrNotFound
	}
	found.Status = m.observedStatus(found)
	return found, nil
}

// List returns a snapshot of every VM, ordered by CreatedAt ascending.
func (m *Manager) List() []*VM {
	m.mu.Lock()
	out := make([]*VM, 0, len(m.vms))
	for _, v := range m.vms {
		cp := *v
		out = append(out, &cp)
	}
	m.mu.Unlock()
	for _, v := range out {
		v.Status = m.observedStatus(v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Task returns the agent task identified by id, or nil when missing.
func (m *Manager) Task(id uuid.UUID) *AgentTask { return m.tasks.Get(id) }

// observedStatus combines the persisted status with a quick liveness
// probe. For terminal statuses (failed, stopped) the persisted value
// wins; for running it gets revalidated against the pidfile.
func (m *Manager) observedStatus(v *VM) Status {
	switch v.Status {
	case StatusRunning:
		pid, err := qemu.ReadPIDFile(v.PIDFile)
		if err != nil || !qemu.IsAlive(pid) {
			return StatusStopped
		}
		return StatusRunning
	default:
		return v.Status
	}
}

// Create begins async VM creation and returns the agent task tracking it.
// The HTTP layer responds 202 immediately with the task ID; subsequent
// GET /v1/tasks/{id} polls reveal progress.
func (m *Manager) Create(ctx context.Context, spec CreateSpec) (*AgentTask, error) {
	if err := validateCreateSpec(spec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	m.poolsMu.RLock()
	p, ok := m.pools[spec.PoolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}
	templatePath := filepath.Join(p.root, "templates", spec.TemplateChecksum+".qcow2")
	if _, err := os.Stat(templatePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrTemplateMissing, templatePath)
		}
		return nil, fmt.Errorf("stat template: %w", err)
	}

	vmID := spec.UUID
	if vmID == uuid.Nil {
		vmID = uuid.New()
	}
	arch := qemu.HostArch()

	v := &VM{
		ID:            vmID,
		Name:          spec.Name,
		VCPUs:         spec.VCPUs,
		MemoryMB:      spec.MemoryMB,
		PoolName:      spec.PoolName,
		Architecture:  arch,
		Status:        StatusPending,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
		DiskPath:      filepath.Join(p.root, "vms", vmID.String(), "disk.qcow2"),
		QMPSocket:     filepath.Join(m.stateDir, vmID.String(), "qmp.sock"),
		ConsoleSocket: filepath.Join(m.stateDir, vmID.String(), "console.sock"),
		PIDFile:       filepath.Join(m.stateDir, vmID.String(), "qemu.pid"),
	}
	if len(spec.UserData) > 0 {
		v.CidataPath = filepath.Join(m.stateDir, vmID.String(), "cidata.iso")
	}

	m.mu.Lock()
	m.vms[vmID] = v
	m.mu.Unlock()

	task := m.tasks.Create(TaskKindVMCreate, vmID)

	// #nosec G118 -- async task work intentionally outlives the HTTP request;
	// the task surface (GET /v1/tasks/{id}) is how clients track progress.
	go m.runCreate(task.ID, v, templatePath, spec.UserData)
	return task, nil
}

func (m *Manager) runCreate(taskID uuid.UUID, v *VM, templatePath string, userData []byte) {
	log := m.log.With("vm_id", v.ID.String(), "task_id", taskID.String())
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	m.transitionVM(v.ID, StatusCreating, "")
	if err := m.persistVM(v.ID); err != nil {
		log.Error("persist meta.json (creating)", "err", err)
		m.failTask(taskID, v.ID, "internal", err.Error())
		return
	}

	if err := os.MkdirAll(filepath.Join(m.stateDir, v.ID.String()), 0o750); err != nil {
		log.Error("mkdir state vm dir", "err", err)
		m.failTask(taskID, v.ID, "internal", err.Error())
		return
	}

	if err := storage.CloneTemplate(templatePath, v.DiskPath); err != nil {
		log.Error("clone template", "err", err)
		m.failTask(taskID, v.ID, "clone_failed", err.Error())
		return
	}

	if v.CidataPath != "" {
		builder := &cloudinit.Builder{
			Hostname: v.Name,
			UserData: userData,
		}
		if _, err := builder.Build(v.CidataPath); err != nil {
			log.Error("build cidata iso", "err", err)
			m.failTask(taskID, v.ID, "cloudinit_failed", err.Error())
			return
		}
	}

	if code, err := m.spawnAndVerify(log, v); err != nil {
		m.failTask(taskID, v.ID, code, err.Error())
		return
	}

	m.transitionVM(v.ID, StatusRunning, "")
	if err := m.persistVM(v.ID); err != nil {
		log.Warn("persist meta.json (running)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": v.ID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
}

// spawnAndVerify boots а qemu process for v, verifies the pidfile is
// alive, и confirms QMP responds к query-status. Returns ("", nil) on
// success или (code, err) where `code` is the agent task error code
// the caller should pass к failTask. Pure side-effect: forks а
// daemonized qemu process; does not mutate Manager state. Shared by
// runCreate (fresh VM, first boot) и runStart (existing VM, restart
// after stop / poweroff).
func (m *Manager) spawnAndVerify(log *slog.Logger, v *VM) (string, error) {
	binary, err := qemu.Binary(v.Architecture)
	if err != nil {
		log.Error("qemu binary", "err", err)
		return "internal", err
	}
	args, err := qemu.BuildArgs(qemu.VMSpec{
		Name:            v.Name,
		UUID:            v.ID,
		VCPUs:           v.VCPUs,
		MemoryMB:        v.MemoryMB,
		Architecture:    v.Architecture,
		Accelerator:     m.accelerator,
		DiskPath:        v.DiskPath,
		QMPSocket:       v.QMPSocket,
		ConsoleSocket:   v.ConsoleSocket,
		PIDFile:         v.PIDFile,
		AArch64Firmware: m.aarch64Firmware,
		CidataPath:      v.CidataPath,
	})
	if err != nil {
		log.Error("build qemu args", "err", err)
		return "internal", err
	}

	spawnCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := qemu.Spawn(spawnCtx, binary, args); err != nil {
		log.Error("spawn qemu", "err", err)
		return "qemu_spawn_failed", err
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil {
		log.Error("read pidfile", "err", err)
		return "qemu_supervision_failed", err
	}
	if !qemu.IsAlive(pid) {
		log.Error("qemu exited immediately after daemonize", "pid", pid)
		return "qemu_supervision_failed", fmt.Errorf("pid %d is not alive", pid)
	}

	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		log.Error("qmp dial", "err", err)
		return "qmp_unavailable", err
	}
	status, err := client.QueryStatus()
	_ = client.Close()
	if err != nil {
		log.Error("qmp query-status", "err", err)
		return "qmp_unavailable", err
	}
	log.Info("vm running", "qmp_status", status, "pid", pid)
	return "", nil
}

// Pause issues QMP `stop` against the running guest и transitions
// the persisted status к paused. Returns ErrNotFound when no VM
// matches; ErrInvalidState when the VM is not currently observed
// as running; ErrQMPUnavailable when the QMP dial or command call
// fails. Synchronous — the QMP `stop` command returns immediately.
//
// Per Area 4-IV the agent does not auto-retry on QMP failure: the
// VM stays в its prior phase, the error surfaces to the operator,
// and they decide whether к re-invoke.
func (m *Manager) Pause(ctx context.Context, name string) (*VM, error) {
	return m.runSyncLifecycle(ctx, name, "pause", func(v *VM, observed Status) (Status, error) {
		if observed != StatusRunning {
			return "", fmt.Errorf("%w: pause requires phase=running, got %q", ErrInvalidState, observed)
		}
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("%w: dial: %v", ErrQMPUnavailable, err)
		}
		defer func() { _ = client.Close() }()
		if err := client.Stop(); err != nil {
			return "", fmt.Errorf("%w: stop: %v", ErrQMPUnavailable, err)
		}
		return StatusPaused, nil
	})
}

// Resume issues QMP `cont` against the paused guest и transitions
// the persisted status back к running. Returns ErrInvalidState
// when the observed status is not paused.
func (m *Manager) Resume(ctx context.Context, name string) (*VM, error) {
	return m.runSyncLifecycle(ctx, name, "resume", func(v *VM, observed Status) (Status, error) {
		if observed != StatusPaused {
			return "", fmt.Errorf("%w: resume requires phase=paused, got %q", ErrInvalidState, observed)
		}
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("%w: dial: %v", ErrQMPUnavailable, err)
		}
		defer func() { _ = client.Close() }()
		if err := client.Cont(); err != nil {
			return "", fmt.Errorf("%w: cont: %v", ErrQMPUnavailable, err)
		}
		return StatusRunning, nil
	})
}

// Reset issues QMP `system_reset` against the running guest. The
// QEMU process keeps running and the guest CPU is reset; the
// persisted status stays running (the wire `phase` is unchanged
// because runtime-side identity is preserved — operators detect
// the reboot through guest uptime, не through CP-side state).
// Returns ErrInvalidState when the observed status is not running.
func (m *Manager) Reset(ctx context.Context, name string) (*VM, error) {
	return m.runSyncLifecycle(ctx, name, "reset", func(v *VM, observed Status) (Status, error) {
		if observed != StatusRunning {
			return "", fmt.Errorf("%w: reset requires phase=running, got %q", ErrInvalidState, observed)
		}
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("%w: dial: %v", ErrQMPUnavailable, err)
		}
		defer func() { _ = client.Close() }()
		if err := client.SystemReset(); err != nil {
			return "", fmt.Errorf("%w: system_reset: %v", ErrQMPUnavailable, err)
		}
		return StatusRunning, nil
	})
}

// runSyncLifecycle is the shared engine для Pause / Resume / Reset.
// It resolves the VM by name, snapshots the entry under the manager
// mutex, runs the supplied action without holding the lock (so QMP
// dial does not block other handlers), и applies the resulting
// status transition + meta.json persist под the mutex. Returns the
// post-transition VM snapshot for the handler's 200 response.
//
// Failures are logged at the agent's component logger so operators
// can correlate the structured handler 5xx с the underlying QMP
// fault; the persisted phase is unchanged on failure (Area 4-IV).
func (m *Manager) runSyncLifecycle(
	_ context.Context,
	name, op string,
	action func(v *VM, observed Status) (Status, error),
) (*VM, error) {
	v, err := m.GetByName(name)
	if err != nil {
		return nil, err
	}
	observed := v.Status

	newStatus, err := action(v, observed)
	if err != nil {
		m.log.Error("vm sync lifecycle action failed",
			"op", op, "vm_name", name, "vm_id", v.ID.String(), "observed_status", string(observed), "err", err)
		return nil, err
	}

	m.transitionVM(v.ID, newStatus, "")
	if persistErr := m.persistVM(v.ID); persistErr != nil {
		m.log.Warn("vm sync lifecycle persist meta failed",
			"op", op, "vm_name", name, "vm_id", v.ID.String(), "err", persistErr)
	}

	updated, err := m.GetByName(name)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Start begins async VM boot и returns the agent task tracking it.
// Validates that the VM is in а startable observed phase (stopped,
// failed, or already running — а start-when-running is а task-success
// no-op per CP spec). Spawns the QEMU process, verifies QMP responds,
// then transitions persisted status к running. Returns ErrNotFound
// when no VM matches; ErrInvalidState when phase is paused / creating
// / deleting (use resume или wait, не start).
func (m *Manager) Start(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.GetByName(name)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case StatusStopped, StatusFailed, StatusRunning:
	default:
		return nil, fmt.Errorf("%w: start requires phase ∈ {stopped, failed, running}, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMStart, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runStart(task.ID, v.ID, v.Status)
	}()
	return task, nil
}

func (m *Manager) runStart(taskID, vmID uuid.UUID, observed Status) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "start")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	if observed == StatusRunning {
		log.Info("vm already running; start is а no-op")
		result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusSuccess
			t.Result = result
		})
		return
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	if code, err := m.spawnAndVerify(log, v); err != nil {
		m.failTask(taskID, vmID, code, err.Error())
		return
	}

	m.transitionVM(vmID, StatusRunning, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (running)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
}

// Stop begins async graceful VM shutdown via ACPI (QMP
// system_powerdown). The agent waits up к shutdownGrace (60s) for the
// guest к honour the signal; if the guest does not exit within the
// window the task completes as failed с code `stop_timeout` и the
// observed phase stays running per Area 4-II lock (operators dispatch
// к poweroff via the CLI `--force` flag or the explicit poweroff
// endpoint).
func (m *Manager) Stop(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.GetByName(name)
	if err != nil {
		return nil, err
	}
	if v.Status != StatusRunning {
		return nil, fmt.Errorf("%w: stop requires phase=running, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMStop, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runStop(task.ID, v.ID)
	}()
	return task, nil
}

func (m *Manager) runStop(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "stop")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	m.transitionVM(vmID, StatusStopping, "")
	if persistErr := m.persistVM(vmID); persistErr != nil {
		log.Warn("persist meta.json (stopping)", "err", persistErr)
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil {
		log.Error("read pidfile", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qemu_supervision_failed", err.Error())
		return
	}

	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		log.Error("qmp dial", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	if err := client.SystemPowerdown(); err != nil {
		_ = client.Close()
		log.Error("qmp system_powerdown", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	_ = client.Close()

	waitCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := qemu.WaitGone(waitCtx, pid, shutdownGrace); err != nil {
		log.Warn("graceful shutdown timed out", "pid", pid, "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "stop_timeout",
			fmt.Sprintf("guest did not honour ACPI shutdown within %s; use poweroff or stop --force", shutdownGrace))
		return
	}

	m.transitionVM(vmID, StatusStopped, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (stopped)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("vm stopped (acpi)")
}

// Poweroff begins async hard shutdown. Attempts QMP `quit` first (so
// qemu releases sockets / pidfile cleanly) и falls back к SIGKILL
// после poweroffGrace (5s) if quit does not land. The guest OS is not
// notified per spec; data loss inside the guest is а documented
// possibility. Returns ErrInvalidState only when phase is pending or
// creating (the VM is mid-spawn — operators wait); every other phase
// admits poweroff (the wire intent is "make it stop").
func (m *Manager) Poweroff(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.GetByName(name)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case StatusPending, StatusCreating, StatusDeleting:
		return nil, fmt.Errorf("%w: poweroff requires а past-spawn phase, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMPoweroff, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runPoweroff(task.ID, v.ID)
	}()
	return task, nil
}

func (m *Manager) runPoweroff(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "poweroff")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	m.transitionVM(vmID, StatusStopping, "")
	if persistErr := m.persistVM(vmID); persistErr != nil {
		log.Warn("persist meta.json (stopping)", "err", persistErr)
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil || pid <= 0 || !qemu.IsAlive(pid) {
		log.Info("qemu already gone; poweroff is а no-op", "pid", pid, "err", err)
		m.transitionVM(vmID, StatusStopped, "")
		_ = m.persistVM(vmID)
		result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusSuccess
			t.Result = result
		})
		return
	}

	if client, dialErr := qemu.DialQMP(v.QMPSocket, 5*time.Second); dialErr == nil {
		if err := client.Quit(); err != nil {
			log.Warn("qmp quit failed; falling through к SIGKILL", "err", err)
		}
		_ = client.Close()
	} else {
		log.Warn("qmp dial during poweroff failed; falling through к SIGKILL", "err", dialErr)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), poweroffGrace)
	if err := qemu.WaitGone(waitCtx, pid, poweroffGrace); err != nil {
		cancel()
		log.Warn("qmp quit did not land within poweroffGrace; sending SIGKILL", "pid", pid)
		if killErr := qemu.Kill(pid); killErr != nil {
			log.Error("SIGKILL failed", "pid", pid, "err", killErr)
			m.failTask(taskID, vmID, "qemu_supervision_failed", killErr.Error())
			return
		}
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := qemu.WaitGone(killCtx, pid, 5*time.Second); err != nil {
			killCancel()
			log.Error("SIGKILL did not land", "pid", pid, "err", err)
			m.failTask(taskID, vmID, "qemu_supervision_failed", err.Error())
			return
		}
		killCancel()
	} else {
		cancel()
	}

	m.transitionVM(vmID, StatusStopped, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (stopped)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("vm powered off")
}

// Reboot begins async graceful reboot — orchestrates an internal stop
// followed by an internal start (Area 4-III lock — reboot ≠ reset; the
// QEMU process is replaced so the PID changes). Stop honours
// shutdownGrace; if the guest does not exit within the window the
// reboot task fails с code `stop_timeout` и the observed phase stays
// running (operators dispatch к reset via the dedicated endpoint).
func (m *Manager) Reboot(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.GetByName(name)
	if err != nil {
		return nil, err
	}
	if v.Status != StatusRunning {
		return nil, fmt.Errorf("%w: reboot requires phase=running, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMReboot, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runReboot(task.ID, v.ID)
	}()
	return task, nil
}

func (m *Manager) runReboot(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "reboot")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	m.transitionVM(vmID, StatusStopping, "")
	if persistErr := m.persistVM(vmID); persistErr != nil {
		log.Warn("persist meta.json (stopping)", "err", persistErr)
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil {
		log.Error("read pidfile", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qemu_supervision_failed", err.Error())
		return
	}

	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		log.Error("qmp dial", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	if err := client.SystemPowerdown(); err != nil {
		_ = client.Close()
		log.Error("qmp system_powerdown", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	_ = client.Close()

	waitCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	if err := qemu.WaitGone(waitCtx, pid, shutdownGrace); err != nil {
		cancel()
		log.Warn("reboot stop phase timed out", "pid", pid, "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "stop_timeout",
			fmt.Sprintf("guest did not honour ACPI shutdown within %s; use reset to force", shutdownGrace))
		return
	}
	cancel()

	m.transitionVM(vmID, StatusStopped, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (intermediate stopped)", "err", err)
	}

	v2, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm post-stop", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	if code, err := m.spawnAndVerify(log, v2); err != nil {
		m.failTask(taskID, vmID, code, err.Error())
		return
	}

	m.transitionVM(vmID, StatusRunning, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (running)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("vm rebooted")
}

// DeleteByName begins async VM teardown addressed by name, used by
// the Path D-rekeyed `DELETE /v1/vms/{vm_name}` agent handler.
// Resolves name → in-memory VM under the manager mutex, then delegates
// to Delete. ErrNotFound when no entry matches.
func (m *Manager) DeleteByName(ctx context.Context, name string) (*AgentTask, error) {
	m.mu.Lock()
	var id uuid.UUID
	for _, v := range m.vms {
		if v.Name == name {
			id = v.ID
			break
		}
	}
	m.mu.Unlock()
	if id == uuid.Nil {
		return nil, ErrNotFound
	}
	return m.Delete(ctx, id)
}

// Delete begins async VM teardown and returns the agent task tracking
// it. ErrNotFound when the VM is unknown.
func (m *Manager) Delete(ctx context.Context, vmID uuid.UUID) (*AgentTask, error) {
	m.mu.Lock()
	v, ok := m.vms[vmID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	name := v.Name
	v.Status = StatusDeleting
	v.UpdatedAt = time.Now().UTC()
	m.mu.Unlock()

	release, ok := m.inFlightAcquire(name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMDelete, vmID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runDelete(task.ID, vmID)
	}()
	return task, nil
}

func (m *Manager) runDelete(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String())
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (deleting)", "err", err)
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}

	pid, _ := qemu.ReadPIDFile(v.PIDFile) // ignore error — VM may already be gone
	if pid > 0 && qemu.IsAlive(pid) {
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err == nil {
			if err := client.SystemPowerdown(); err != nil {
				log.Warn("system_powerdown failed; will fall back к force", "err", err)
			}
			_ = client.Close()
		} else {
			log.Warn("qmp dial during delete; falling back к kill", "err", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if err := qemu.WaitGone(ctx, pid, shutdownGrace); err != nil {
			cancel()
			log.Warn("graceful shutdown timed out, sending SIGKILL", "pid", pid, "err", err)
			if killErr := qemu.Kill(pid); killErr != nil {
				log.Error("SIGKILL failed", "pid", pid, "err", killErr)
			}
			killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = qemu.WaitGone(killCtx, pid, 5*time.Second)
			killCancel()
		} else {
			cancel()
		}
	}

	// Cleanup disk + per-VM dirs. Errors logged but не fatal — operator
	// can clean stale files manually if needed.
	if err := os.RemoveAll(filepath.Dir(v.DiskPath)); err != nil {
		log.Warn("remove disk dir", "path", filepath.Dir(v.DiskPath), "err", err)
	}
	if err := os.RemoveAll(filepath.Join(m.stateDir, vmID.String())); err != nil {
		log.Warn("remove agent state dir", "err", err)
	}

	m.mu.Lock()
	delete(m.vms, vmID)
	m.mu.Unlock()

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
	log.Info("vm deleted")
}

func (m *Manager) snapshotVM(id uuid.UUID) (*VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (m *Manager) transitionVM(id uuid.UUID, status Status, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return
	}
	v.Status = status
	v.UpdatedAt = time.Now().UTC()
}

func (m *Manager) persistVM(id uuid.UUID) error {
	v, err := m.snapshotVM(id)
	if err != nil {
		return err
	}
	meta := &state.VMMeta{
		VMID:          v.ID,
		Name:          v.Name,
		VCPUs:         v.VCPUs,
		MemoryMB:      v.MemoryMB,
		PoolName:      v.PoolName,
		Architecture:  string(v.Architecture),
		DiskPath:      v.DiskPath,
		QMPSocket:     v.QMPSocket,
		ConsoleSocket: v.ConsoleSocket,
		PIDFile:       v.PIDFile,
		CidataPath:    v.CidataPath,
		Status:        string(v.Status),
		CreatedAt:     v.CreatedAt,
		UpdatedAt:     v.UpdatedAt,
	}
	return state.WriteMeta(filepath.Join(m.stateDir, v.ID.String()), meta)
}

func (m *Manager) failTask(taskID, vmID uuid.UUID, code, message string) {
	m.transitionVM(vmID, StatusFailed, message)
	_ = m.persistVM(vmID)
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{Code: code, Message: message}
	})
}

// failTaskOnly marks the agent task as failed without mutating VM
// status. Reserved for stop / reboot failure paths where the QEMU
// process is still alive (ACPI ignored, QMP socket flapped, etc.);
// the orchestration failed but the VM is still running, so callers
// transition the VM back to StatusRunning + persist before calling
// this. Using failTask there would override the revert and persist
// a misleading StatusFailed, masking the live qemu process from the
// operator's recovery path (Start would re-spawn against a live
// pidfile lock; observedStatus on persisted Failed does not re-probe
// the pid).
func (m *Manager) failTaskOnly(taskID uuid.UUID, code, message string) {
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{Code: code, Message: message}
	})
}

func validateCreateSpec(s CreateSpec) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(s.Name) > 255 {
		return fmt.Errorf("name must be ≤ 255 chars")
	}
	if s.VCPUs < 1 || s.VCPUs > 128 {
		return fmt.Errorf("vcpus must be in [1, 128]")
	}
	if s.MemoryMB < 128 || s.MemoryMB > 524288 {
		return fmt.Errorf("memory_mb must be in [128, 524288]")
	}
	if s.PoolName == "" {
		return fmt.Errorf("pool is required")
	}
	return validateChecksum(s.TemplateChecksum)
}

func validateChecksum(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("template_checksum must be a 64-char sha256 hex digest")
	}
	for _, ch := range s {
		isHex := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
		if !isHex {
			return fmt.Errorf("template_checksum must be hex")
		}
	}
	return nil
}
