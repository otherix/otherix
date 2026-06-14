// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Role is the agent's side of a migration.
type Role string

// Role values.
const (
	RoleSource Role = "source"
	RoleTarget Role = "target"
)

// Mode mirrors the agent-API migration mode.
type Mode string

// Mode values.
const (
	ModeLive    Mode = "live"
	ModeOffline Mode = "offline"
)

// Phase is the agent-observed migration phase. Slice 2a uses
// setup -> active -> completed (or failed / cancelled); postcopy_active
// is reserved for slice 2c.
type Phase string

// Phase values.
const (
	PhaseSetup     Phase = "setup"
	PhaseActive    Phase = "active"
	PhaseCompleted Phase = "completed"
	PhaseFailed    Phase = "failed"
	PhaseCancelled Phase = "cancelled"
)

// Record is the agent's in-memory view of one migration. Child PIDs and
// the creds dir are tracked so Cancel / teardown can reclaim them. It is
// never persisted.
type Record struct {
	MigrationID uuid.UUID
	VMID        uuid.UUID
	VMName      string
	Role        Role
	Mode        Mode
	Phase       Phase

	// Target side.
	Port        int    // reserved ingress port (target only)
	NBDPid      int    // qemu-nbd pid (target only)
	ListenEndpt string // host:port advertised to the source
	AuthToken   string // correlation id + NBD export name

	// Source side.
	PeerEndpoint string // target host:port
	ConvertPid   int    // qemu-img convert pid (source only)
	AgentTaskID  uuid.UUID

	CredsDir string

	// Progress.
	BytesTotal       int64
	BytesTransferred int64
	ErrorMessage     string

	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}

// Terminal reports whether the phase is committed and no further work runs.
func (r *Record) Terminal() bool {
	return r.Phase == PhaseCompleted || r.Phase == PhaseFailed || r.Phase == PhaseCancelled
}

// Store is a concurrency-safe map of migration records keyed by migration id.
type Store struct {
	mu   sync.Mutex
	recs map[uuid.UUID]*Record
}

// NewStore returns an empty record store.
func NewStore() *Store { return &Store{recs: make(map[uuid.UUID]*Record)} }

// Put inserts or replaces the record for r.MigrationID.
func (s *Store) Put(r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	s.recs[r.MigrationID] = &cp
}

// Get returns a snapshot copy of the record and whether it was present.
func (s *Store) Get(id uuid.UUID) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	if !ok {
		return Record{}, false
	}
	return *r, true
}

// Update applies apply to the stored record under lock. Returns false if
// the record is absent.
func (s *Store) Update(id uuid.UUID, apply func(*Record)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	if !ok {
		return false
	}
	apply(r)
	return true
}

// Delete removes the record.
func (s *Store) Delete(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, id)
}
