// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"database/sql/driver"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type CPUArch string

const (
	CpuArchAmd64 CPUArch = "amd64"
	CpuArchArm64 CPUArch = "arm64"
)

func (e *CPUArch) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = CPUArch(s)
	case string:
		*e = CPUArch(s)
	default:
		return fmt.Errorf("unsupported scan type for CPUArch: %T", src)
	}
	return nil
}

type NullCPUArch struct {
	CPUArch CPUArch
	Valid   bool // Valid is true if CPUArch is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullCPUArch) Scan(value interface{}) error {
	if value == nil {
		ns.CPUArch, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.CPUArch.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullCPUArch) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.CPUArch), nil
}

type DiskBus string

const (
	DiskBusVirtio DiskBus = "virtio"
	DiskBusSata   DiskBus = "sata"
	DiskBusScsi   DiskBus = "scsi"
	DiskBusIde    DiskBus = "ide"
)

func (e *DiskBus) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = DiskBus(s)
	case string:
		*e = DiskBus(s)
	default:
		return fmt.Errorf("unsupported scan type for DiskBus: %T", src)
	}
	return nil
}

type NullDiskBus struct {
	DiskBus DiskBus
	Valid   bool // Valid is true if DiskBus is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullDiskBus) Scan(value interface{}) error {
	if value == nil {
		ns.DiskBus, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.DiskBus.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullDiskBus) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.DiskBus), nil
}

type DiskCacheMode string

const (
	DiskCacheModeNone         DiskCacheMode = "none"
	DiskCacheModeWriteback    DiskCacheMode = "writeback"
	DiskCacheModeWritethrough DiskCacheMode = "writethrough"
	DiskCacheModeDirectsync   DiskCacheMode = "directsync"
	DiskCacheModeUnsafe       DiskCacheMode = "unsafe"
)

func (e *DiskCacheMode) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = DiskCacheMode(s)
	case string:
		*e = DiskCacheMode(s)
	default:
		return fmt.Errorf("unsupported scan type for DiskCacheMode: %T", src)
	}
	return nil
}

type NullDiskCacheMode struct {
	DiskCacheMode DiskCacheMode
	Valid         bool // Valid is true if DiskCacheMode is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullDiskCacheMode) Scan(value interface{}) error {
	if value == nil {
		ns.DiskCacheMode, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.DiskCacheMode.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullDiskCacheMode) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.DiskCacheMode), nil
}

type DiskDiscard string

const (
	DiskDiscardIgnore DiskDiscard = "ignore"
	DiskDiscardUnmap  DiskDiscard = "unmap"
)

func (e *DiskDiscard) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = DiskDiscard(s)
	case string:
		*e = DiskDiscard(s)
	default:
		return fmt.Errorf("unsupported scan type for DiskDiscard: %T", src)
	}
	return nil
}

type NullDiskDiscard struct {
	DiskDiscard DiskDiscard
	Valid       bool // Valid is true if DiskDiscard is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullDiskDiscard) Scan(value interface{}) error {
	if value == nil {
		ns.DiskDiscard, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.DiskDiscard.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullDiskDiscard) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.DiskDiscard), nil
}

type FirmwareType string

const (
	FirmwareTypeBios FirmwareType = "bios"
	FirmwareTypeUefi FirmwareType = "uefi"
)

func (e *FirmwareType) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = FirmwareType(s)
	case string:
		*e = FirmwareType(s)
	default:
		return fmt.Errorf("unsupported scan type for FirmwareType: %T", src)
	}
	return nil
}

type NullFirmwareType struct {
	FirmwareType FirmwareType
	Valid        bool // Valid is true if FirmwareType is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullFirmwareType) Scan(value interface{}) error {
	if value == nil {
		ns.FirmwareType, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.FirmwareType.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullFirmwareType) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.FirmwareType), nil
}

type ImageFormat string

const (
	ImageFormatQcow2 ImageFormat = "qcow2"
	ImageFormatRaw   ImageFormat = "raw"
)

func (e *ImageFormat) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = ImageFormat(s)
	case string:
		*e = ImageFormat(s)
	default:
		return fmt.Errorf("unsupported scan type for ImageFormat: %T", src)
	}
	return nil
}

type NullImageFormat struct {
	ImageFormat ImageFormat
	Valid       bool // Valid is true if ImageFormat is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullImageFormat) Scan(value interface{}) error {
	if value == nil {
		ns.ImageFormat, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.ImageFormat.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullImageFormat) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.ImageFormat), nil
}

type MigrationPhase string

const (
	MigrationPhaseSetup          MigrationPhase = "setup"
	MigrationPhaseActive         MigrationPhase = "active"
	MigrationPhasePostcopyActive MigrationPhase = "postcopy_active"
	MigrationPhaseCompleted      MigrationPhase = "completed"
	MigrationPhaseFailed         MigrationPhase = "failed"
	MigrationPhaseCancelled      MigrationPhase = "cancelled"
)

func (e *MigrationPhase) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = MigrationPhase(s)
	case string:
		*e = MigrationPhase(s)
	default:
		return fmt.Errorf("unsupported scan type for MigrationPhase: %T", src)
	}
	return nil
}

type NullMigrationPhase struct {
	MigrationPhase MigrationPhase
	Valid          bool // Valid is true if MigrationPhase is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullMigrationPhase) Scan(value interface{}) error {
	if value == nil {
		ns.MigrationPhase, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.MigrationPhase.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullMigrationPhase) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.MigrationPhase), nil
}

type MigrationReason string

const (
	MigrationReasonManual     MigrationReason = "manual"
	MigrationReasonDrain      MigrationReason = "drain"
	MigrationReasonRebalance  MigrationReason = "rebalance"
	MigrationReasonEvacuation MigrationReason = "evacuation"
)

func (e *MigrationReason) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = MigrationReason(s)
	case string:
		*e = MigrationReason(s)
	default:
		return fmt.Errorf("unsupported scan type for MigrationReason: %T", src)
	}
	return nil
}

type NullMigrationReason struct {
	MigrationReason MigrationReason
	Valid           bool // Valid is true if MigrationReason is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullMigrationReason) Scan(value interface{}) error {
	if value == nil {
		ns.MigrationReason, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.MigrationReason.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullMigrationReason) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.MigrationReason), nil
}

type NetworkType string

const (
	NetworkTypeBridge NetworkType = "bridge"
)

func (e *NetworkType) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = NetworkType(s)
	case string:
		*e = NetworkType(s)
	default:
		return fmt.Errorf("unsupported scan type for NetworkType: %T", src)
	}
	return nil
}

type NullNetworkType struct {
	NetworkType NetworkType
	Valid       bool // Valid is true if NetworkType is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullNetworkType) Scan(value interface{}) error {
	if value == nil {
		ns.NetworkType, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.NetworkType.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullNetworkType) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.NetworkType), nil
}

type NicModel string

const (
	NicModelVirtio  NicModel = "virtio"
	NicModelE1000   NicModel = "e1000"
	NicModelE1000e  NicModel = "e1000e"
	NicModelRtl8139 NicModel = "rtl8139"
)

func (e *NicModel) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = NicModel(s)
	case string:
		*e = NicModel(s)
	default:
		return fmt.Errorf("unsupported scan type for NicModel: %T", src)
	}
	return nil
}

type NullNicModel struct {
	NicModel NicModel
	Valid    bool // Valid is true if NicModel is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullNicModel) Scan(value interface{}) error {
	if value == nil {
		ns.NicModel, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.NicModel.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullNicModel) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.NicModel), nil
}

type NodeStatus string

const (
	NodeStatusPending     NodeStatus = "pending"
	NodeStatusReady       NodeStatus = "ready"
	NodeStatusCordoned    NodeStatus = "cordoned"
	NodeStatusDraining    NodeStatus = "draining"
	NodeStatusUnreachable NodeStatus = "unreachable"
	NodeStatusGone        NodeStatus = "gone"
)

func (e *NodeStatus) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = NodeStatus(s)
	case string:
		*e = NodeStatus(s)
	default:
		return fmt.Errorf("unsupported scan type for NodeStatus: %T", src)
	}
	return nil
}

type NullNodeStatus struct {
	NodeStatus NodeStatus
	Valid      bool // Valid is true if NodeStatus is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullNodeStatus) Scan(value interface{}) error {
	if value == nil {
		ns.NodeStatus, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.NodeStatus.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullNodeStatus) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.NodeStatus), nil
}

type OsFamily string

const (
	OsFamilyLinux   OsFamily = "linux"
	OsFamilyWindows OsFamily = "windows"
	OsFamilyBsd     OsFamily = "bsd"
	OsFamilyOther   OsFamily = "other"
)

func (e *OsFamily) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = OsFamily(s)
	case string:
		*e = OsFamily(s)
	default:
		return fmt.Errorf("unsupported scan type for OsFamily: %T", src)
	}
	return nil
}

type NullOsFamily struct {
	OsFamily OsFamily
	Valid    bool // Valid is true if OsFamily is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullOsFamily) Scan(value interface{}) error {
	if value == nil {
		ns.OsFamily, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.OsFamily.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullOsFamily) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.OsFamily), nil
}

type SnapshotStatus string

const (
	SnapshotStatusCreating  SnapshotStatus = "creating"
	SnapshotStatusReady     SnapshotStatus = "ready"
	SnapshotStatusRestoring SnapshotStatus = "restoring"
	SnapshotStatusDeleting  SnapshotStatus = "deleting"
	SnapshotStatusError     SnapshotStatus = "error"
)

func (e *SnapshotStatus) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = SnapshotStatus(s)
	case string:
		*e = SnapshotStatus(s)
	default:
		return fmt.Errorf("unsupported scan type for SnapshotStatus: %T", src)
	}
	return nil
}

type NullSnapshotStatus struct {
	SnapshotStatus SnapshotStatus
	Valid          bool // Valid is true if SnapshotStatus is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullSnapshotStatus) Scan(value interface{}) error {
	if value == nil {
		ns.SnapshotStatus, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.SnapshotStatus.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullSnapshotStatus) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.SnapshotStatus), nil
}

type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSuccess   TaskStatus = "success"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
)

func (e *TaskStatus) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = TaskStatus(s)
	case string:
		*e = TaskStatus(s)
	default:
		return fmt.Errorf("unsupported scan type for TaskStatus: %T", src)
	}
	return nil
}

type NullTaskStatus struct {
	TaskStatus TaskStatus
	Valid      bool // Valid is true if TaskStatus is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullTaskStatus) Scan(value interface{}) error {
	if value == nil {
		ns.TaskStatus, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.TaskStatus.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullTaskStatus) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.TaskStatus), nil
}

type VMDesiredPhase string

const (
	VmDesiredPhaseRunning VMDesiredPhase = "running"
	VmDesiredPhaseStopped VMDesiredPhase = "stopped"
	VmDesiredPhaseDeleted VMDesiredPhase = "deleted"
)

func (e *VMDesiredPhase) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = VMDesiredPhase(s)
	case string:
		*e = VMDesiredPhase(s)
	default:
		return fmt.Errorf("unsupported scan type for VMDesiredPhase: %T", src)
	}
	return nil
}

type NullVMDesiredPhase struct {
	VMDesiredPhase VMDesiredPhase
	Valid          bool // Valid is true if VMDesiredPhase is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullVMDesiredPhase) Scan(value interface{}) error {
	if value == nil {
		ns.VMDesiredPhase, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.VMDesiredPhase.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullVMDesiredPhase) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.VMDesiredPhase), nil
}

type VMPhase string

const (
	VmPhasePending  VMPhase = "pending"
	VmPhaseRunning  VMPhase = "running"
	VmPhasePaused   VMPhase = "paused"
	VmPhaseStopped  VMPhase = "stopped"
	VmPhaseError    VMPhase = "error"
	VmPhaseGone     VMPhase = "gone"
	VmPhaseOrphaned VMPhase = "orphaned"
)

func (e *VMPhase) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = VMPhase(s)
	case string:
		*e = VMPhase(s)
	default:
		return fmt.Errorf("unsupported scan type for VMPhase: %T", src)
	}
	return nil
}

type NullVMPhase struct {
	VMPhase VMPhase
	Valid   bool // Valid is true if VMPhase is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullVMPhase) Scan(value interface{}) error {
	if value == nil {
		ns.VMPhase, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.VMPhase.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullVMPhase) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.VMPhase), nil
}

type VMStateAtSnapshot string

const (
	VmStateAtSnapshotRunning VMStateAtSnapshot = "running"
	VmStateAtSnapshotPaused  VMStateAtSnapshot = "paused"
	VmStateAtSnapshotStopped VMStateAtSnapshot = "stopped"
)

func (e *VMStateAtSnapshot) Scan(src interface{}) error {
	switch s := src.(type) {
	case []byte:
		*e = VMStateAtSnapshot(s)
	case string:
		*e = VMStateAtSnapshot(s)
	default:
		return fmt.Errorf("unsupported scan type for VMStateAtSnapshot: %T", src)
	}
	return nil
}

type NullVMStateAtSnapshot struct {
	VMStateAtSnapshot VMStateAtSnapshot
	Valid             bool // Valid is true if VMStateAtSnapshot is not NULL
}

// Scan implements the Scanner interface.
func (ns *NullVMStateAtSnapshot) Scan(value interface{}) error {
	if value == nil {
		ns.VMStateAtSnapshot, ns.Valid = "", false
		return nil
	}
	ns.Valid = true
	return ns.VMStateAtSnapshot.Scan(value)
}

// Value implements the driver Valuer interface.
func (ns NullVMStateAtSnapshot) Value() (driver.Value, error) {
	if !ns.Valid {
		return nil, nil
	}
	return string(ns.VMStateAtSnapshot), nil
}

type AgentCert struct {
	ID                uuid.UUID
	NodeID            uuid.UUID
	Serial            []byte
	FingerprintSha256 []byte
	SubjectDn         string
	NotBefore         time.Time
	NotAfter          time.Time
	IssuedAt          time.Time
	RevokedAt         *time.Time
	RevocationReason  *string
}

type ApiToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	TokenHash  []byte
	Prefix     string
	Scopes     []string
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

type AuditLog struct {
	ID           uuid.UUID
	ActorUserID  *uuid.UUID
	ActorTokenID *uuid.UUID
	ActorKind    string
	Action       string
	ResourceType *string
	ResourceID   *uuid.UUID
	RequestID    *uuid.UUID
	IpAddr       *netip.Addr
	UserAgent    *string
	Before       []byte
	After        []byte
	Metadata     []byte
	CreatedAt    time.Time
}

type AuditLogDefault struct {
	ID           uuid.UUID
	ActorUserID  *uuid.UUID
	ActorTokenID *uuid.UUID
	ActorKind    string
	Action       string
	ResourceType *string
	ResourceID   *uuid.UUID
	RequestID    *uuid.UUID
	IpAddr       *netip.Addr
	UserAgent    *string
	Before       []byte
	After        []byte
	Metadata     []byte
	CreatedAt    time.Time
}

type AuditLogY2026M05 struct {
	ID           uuid.UUID
	ActorUserID  *uuid.UUID
	ActorTokenID *uuid.UUID
	ActorKind    string
	Action       string
	ResourceType *string
	ResourceID   *uuid.UUID
	RequestID    *uuid.UUID
	IpAddr       *netip.Addr
	UserAgent    *string
	Before       []byte
	After        []byte
	Metadata     []byte
	CreatedAt    time.Time
}

type AuditLogY2026M06 struct {
	ID           uuid.UUID
	ActorUserID  *uuid.UUID
	ActorTokenID *uuid.UUID
	ActorKind    string
	Action       string
	ResourceType *string
	ResourceID   *uuid.UUID
	RequestID    *uuid.UUID
	IpAddr       *netip.Addr
	UserAgent    *string
	Before       []byte
	After        []byte
	Metadata     []byte
	CreatedAt    time.Time
}

type AuditLogY2026M07 struct {
	ID           uuid.UUID
	ActorUserID  *uuid.UUID
	ActorTokenID *uuid.UUID
	ActorKind    string
	Action       string
	ResourceType *string
	ResourceID   *uuid.UUID
	RequestID    *uuid.UUID
	IpAddr       *netip.Addr
	UserAgent    *string
	Before       []byte
	After        []byte
	Metadata     []byte
	CreatedAt    time.Time
}

type CaCert struct {
	ID                uuid.UUID
	CertPem           []byte
	KeyPem            []byte
	FingerprintSha256 []byte
	NotBefore         time.Time
	NotAfter          time.Time
	Active            bool
	CreatedAt         time.Time
}

type ClusterSetting struct {
	ID              int32
	DefaultPoolName *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Firmware struct {
	ID           uuid.UUID
	Name         string
	Architecture CPUArch
	Type         FirmwareType
	Version      *string
	SecureBoot   bool
	IsDefault    bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type IdempotencyKey struct {
	Key             string
	UserID          *uuid.UUID
	RequestMethod   string
	RequestPath     string
	RequestHash     []byte
	ResponseStatus  *int32
	ResponseHeaders []byte
	ResponseBody    []byte
	State           string
	CreatedAt       time.Time
	CompletedAt     *time.Time
	ExpiresAt       time.Time
}

type JoinToken struct {
	ID               uuid.UUID
	TokenHash        []byte
	IntendedNodeName *string
	CreatedByUserID  *uuid.UUID
	ExpiresAt        time.Time
	MaxUses          *int32
	CreatedAt        time.Time
}

type JoinTokenConsumption struct {
	ID               uuid.UUID
	JoinTokenID      uuid.UUID
	ConsumedByNodeID *uuid.UUID
	ConsumedAt       time.Time
	SourceIp         *netip.Addr
}

type Migration struct {
	ID                uuid.UUID
	VmID              uuid.UUID
	SourceNodeID      *uuid.UUID
	TargetNodeID      *uuid.UUID
	InitiatedByUserID *uuid.UUID
	Reason            MigrationReason
	Phase             MigrationPhase
	ProgressPercent   int16
	BytesTotal        *int64
	BytesTransferred  int64
	BytesRemaining    *int64
	PagesTotal        *int64
	PagesTransferred  *int64
	DowntimeMs        *int32
	SetupTimeMs       *int32
	QmpCapabilities   []byte
	MaxBandwidthBytes *int64
	MaxDowntimeMs     *int32
	StartedAt         *time.Time
	CompletedAt       *time.Time
	ErrorMessage      *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Network struct {
	ID         uuid.UUID
	Name       string
	Type       NetworkType
	BridgeName string
	VlanTag    *int32
	Mtu        int32
	Config     []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
}

type Node struct {
	ID                      uuid.UUID
	Name                    string
	Architecture            CPUArch
	AdvertisedEndpoint      string
	MigrationHost           string
	MigrationPortRangeStart int32
	MigrationPortRangeEnd   int32
	Status                  NodeStatus
	CordonedAt              *time.Time
	CPUCoresTotal           *int32
	CPUCoresAvailable       *int32
	CPUModel                *string
	CpuFlags                []string
	MemoryTotalMib          *int64
	MemoryAvailableMib      *int64
	Hugepages2mibTotal      *int32
	Hugepages1gibTotal      *int32
	KernelVersion           *string
	QEMUVersion             *string
	NumaTopology            []byte
	Capabilities            []byte
	LastHeartbeatAt         *time.Time
	AgentVersion            *string
	Labels                  []byte
	// Timestamp when memory pressure condition was set. NULL when condition not active. CP-side computed from heartbeat metrics against placement.pressure.memory.threshold_percent.
	MemoryPressureSince *time.Time
	// Consecutive heartbeat observations below memory pressure threshold. Used for debouncing — count reaches placement.pressure.memory.consecutive_required → pressure set. Reset to 0 on first observation at-or-above threshold.
	MemoryPressureCount int32
	// Root filesystem total capacity (bytes) reported by the agent via syscall.Statfs("/"). NULL when the agent could not read the metric — CP carries existing pressure state forward.
	SystemDiskTotalBytes *int64
	// Root filesystem available bytes reported by the agent. Pairs with system_disk_total_bytes; both nullable together. Volatile per heartbeat.
	SystemDiskAvailableBytes *int64
	// Timestamp when system_disk pressure condition was set. NULL when condition not active. CP-side computed from heartbeat metrics against placement.pressure.system_disk.threshold_percent.
	SystemDiskPressureSince *time.Time
	// Consecutive heartbeat observations below system_disk pressure threshold. Used for debouncing — count reaches placement.pressure.system_disk.consecutive_required → pressure set. Reset to 0 on first observation at-or-above threshold.
	SystemDiskPressureCount int32
	CreatedAt               time.Time
	UpdatedAt               time.Time
	DeletedAt               *time.Time
}

// Node availability with pending-VM accounting. Subtracts vms.cpu_cores / vms.memory_mib pinned after last heartbeat (CP committed but agent has not yet observed). Self-correcting once next heartbeat arrives. Read by the scheduler at placement time.
type NodeEffectiveAvailability struct {
	ID                       uuid.UUID
	Name                     string
	Architecture             CPUArch
	AdvertisedEndpoint       string
	MigrationHost            string
	MigrationPortRangeStart  int32
	MigrationPortRangeEnd    int32
	Status                   NodeStatus
	CordonedAt               *time.Time
	CPUCoresTotal            *int32
	CPUCoresAvailable        *int32
	CPUModel                 *string
	CpuFlags                 []string
	MemoryTotalMib           *int64
	MemoryAvailableMib       *int64
	Hugepages2mibTotal       *int32
	Hugepages1gibTotal       *int32
	KernelVersion            *string
	QEMUVersion              *string
	NumaTopology             []byte
	Capabilities             []byte
	LastHeartbeatAt          *time.Time
	AgentVersion             *string
	Labels                   []byte
	MemoryPressureSince      *time.Time
	MemoryPressureCount      int32
	SystemDiskTotalBytes     *int64
	SystemDiskAvailableBytes *int64
	SystemDiskPressureSince  *time.Time
	SystemDiskPressureCount  int32
	CreatedAt                time.Time
	UpdatedAt                time.Time
	DeletedAt                *time.Time
	CPUCoresEffective        *int32
	MemoryEffectiveMib       *int64
}

type NodeFirmware struct {
	NodeID     uuid.UUID
	FirmwareID uuid.UUID
	CodePath   string
	VarsPath   *string
	Available  bool
	ReportedAt time.Time
}

// Pool availability with pending-VM-disk accounting. Subtracts vm_disks committed after last scan (CP committed but agent has not yet observed). Self-correcting once next scan completes. Operator-visible via the pool API + CLI; future scheduler integration.
type PoolEffectiveCapacity struct {
	ID                      uuid.UUID
	NodeID                  uuid.UUID
	Name                    string
	Type                    string
	Path                    string
	CapacityBytes           *int64
	AvailableBytes          *int64
	ReportedAt              *time.Time
	Config                  []byte
	DiskPressureSince       *time.Time
	DiskPressureCount       int32
	ReconciliationStatus    string
	LastReconciledAt        *time.Time
	ReconciliationError     *string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	DeletedAt               *time.Time
	AvailableBytesEffective *int64
}

type RefreshToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	FamilyID   uuid.UUID
	ParentID   *uuid.UUID
	UserAgent  *string
	IpAddress  *netip.Addr
	IssuedAt   time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

type Snapshot struct {
	ID                uuid.UUID
	VmID              uuid.UUID
	ParentSnapshotID  *uuid.UUID
	OwnerID           uuid.UUID
	Name              string
	Description       string
	Status            SnapshotStatus
	WithMemory        bool
	VMStateAtSnapshot VMStateAtSnapshot
	DiskSizeBytes     *int64
	MemorySizeBytes   *int64
	ErrorMessage      *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

type StorageImage struct {
	ID             uuid.UUID
	TemplateID     uuid.UUID
	PoolID         uuid.UUID
	ChecksumSha256 string
	SizeBytes      int64
	Format         string
	ImportedAt     time.Time
}

type StoragePool struct {
	ID             uuid.UUID
	NodeID         uuid.UUID
	Name           string
	Type           string
	Path           string
	CapacityBytes  *int64
	AvailableBytes *int64
	ReportedAt     *time.Time
	Config         []byte
	// Timestamp when pool disk pressure was set. NULL when condition not active. CP-side computed from scan metrics against placement.pressure.disk.threshold_percent.
	DiskPressureSince *time.Time
	// Consecutive scan observations below pool disk pressure threshold. Used for debouncing — default consecutive_required=1 means a single scan sets pressure. Reset to 0 on first observation at-or-above threshold.
	DiskPressureCount    int32
	ReconciliationStatus string
	LastReconciledAt     *time.Time
	ReconciliationError  *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
}

type Task struct {
	ID           uuid.UUID
	Type         string
	Status       TaskStatus
	ResourceType string
	ResourceID   *uuid.UUID
	Progress     *int16
	Args         []byte
	Result       []byte
	Error        []byte
	Attempts     int32
	MaxAttempts  int32
	RiverJobID   *int64
	AgentTaskID  *uuid.UUID
	CreatedBy    *uuid.UUID
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

type Template struct {
	ID                     uuid.UUID
	OwnerID                uuid.UUID
	Name                   string
	Description            string
	Visibility             string
	Architecture           CPUArch
	OsFamily               OsFamily
	OsVariant              string
	ImageUrl               string
	ImageChecksumSha256    []byte
	ImageFormat            ImageFormat
	ImageSizeBytes         *int64
	FirmwareType           FirmwareType
	FirmwareID             *uuid.UUID
	DefaultCpuCores        int32
	DefaultMemoryMib       int32
	DefaultDiskGib         int32
	CloudInitSupported     bool
	CloudInitUserData      *string
	QemuGuestAgentExpected bool
	Metadata               []byte
	DerivedVmCount         int32
	CreatedAt              time.Time
	UpdatedAt              time.Time
	DeletedAt              *time.Time
}

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	DisplayName  string
	Role         string
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

type VM struct {
	ID                uuid.UUID
	OwnerID           uuid.UUID
	Name              string
	Description       string
	DesiredPhase      VMDesiredPhase
	TemplateID        *uuid.UUID
	Architecture      CPUArch
	CpuCores          int32
	MemoryMib         int32
	CPUModel          string
	MachineType       string
	FirmwareID        *uuid.UUID
	PinnedNodeID      *uuid.UUID
	SchedulerHints    []byte
	UserData          *string
	CloudInitDisabled bool
	MetaData          *string
	NetworkConfig     *string
	SshAuthorizedKeys []string
	Labels            []byte
	Generation        int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

type VMDisk struct {
	ID               uuid.UUID
	VmID             uuid.UUID
	StoragePoolID    uuid.UUID
	DeviceOrder      int32
	Bus              DiskBus
	SizeGib          int32
	SourceKind       string
	SourceTemplateID *uuid.UUID
	Format           ImageFormat
	ReadOnly         bool
	CacheMode        DiskCacheMode
	Discard          DiskDiscard
	BootOrder        *int32
	Generation       int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

type VMDiskRuntime struct {
	VmDiskID           uuid.UUID
	CurrentNodeID      *uuid.UUID
	StoragePoolID      *uuid.UUID
	FilePath           *string
	ActualSizeBytes    *int64
	VirtualSizeBytes   *int64
	ObservedGeneration int64
	LastObservedAt     *time.Time
	UpdatedAt          time.Time
}

type VMNic struct {
	ID          uuid.UUID
	VmID        uuid.UUID
	NetworkID   uuid.UUID
	DeviceOrder int32
	Model       NicModel
	MacAddress  net.HardwareAddr
	Ipv4Address *netip.Addr
	Ipv6Address *netip.Addr
	Generation  int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

type VMNicRuntime struct {
	VmNicID            uuid.UUID
	CurrentNodeID      *uuid.UUID
	TapDevice          *string
	ObservedGeneration int64
	LastObservedAt     *time.Time
	UpdatedAt          time.Time
}

type VMRuntime struct {
	VmID               uuid.UUID
	CurrentNodeID      *uuid.UUID
	Phase              VMPhase
	Conditions         []byte
	ObservedGeneration int64
	QEMUPID            *int32
	QmpSocketPath      *string
	VncPort            *int32
	SpicePort          *int32
	SerialConsolePath  *string
	GuestOs            *string
	GuestHostname      *string
	GuestIpAddresses   []netip.Addr
	CpuUsagePercent    *float32
	MemoryUsedMib      *int64
	LastStartedAt      *time.Time
	LastStoppedAt      *time.Time
	LastErrorAt        *time.Time
	LastErrorMessage   *string
	LastObservedAt     *time.Time
	UpdatedAt          time.Time
}
