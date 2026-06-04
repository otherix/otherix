// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned by a store backend when the requested row does not
// exist. Match it with errors.Is.
var ErrNotFound = errors.New("store: not found")

// ErrCACertActiveExists is returned by a store backend's CreateCACert when an
// active cluster CA row already exists, so the CA bootstrap can trap the
// lost-race path uniformly. Match it with errors.Is.
var ErrCACertActiveExists = errors.New("store: active CA cert already exists")

// Domain uniqueness / state sentinels surfaced by the store backends. Each maps
// to a uniqueness or precondition guard the backend enforces; handlers match
// them with errors.Is to produce the 409 conflict envelopes.
var (
	ErrUserEmailExists          = errors.New("store: user email already in use")
	ErrNetworkNameExists        = errors.New("store: network name already in use")
	ErrNodeNameExists           = errors.New("store: node name already in use")
	ErrStoragePoolNameExists    = errors.New("store: storage pool name already in use on node")
	ErrTaskNotCancellable       = errors.New("store: task not cancellable")
	ErrVMNameInUse              = errors.New("store: vm name already in use")
	ErrVMNicMACConflict         = errors.New("store: vm nic mac already in use on network")
	ErrFirmwareNameExists       = errors.New("store: firmware name already in use for architecture")
	ErrFirmwareDefaultExists    = errors.New("store: default firmware already exists for architecture and type")
	ErrTemplateNameExists       = errors.New("store: template name already in use")
	ErrTemplateFirmwareNotFound = errors.New("store: template firmware does not exist")
	ErrJoinTokenInvalid         = errors.New("store: join token unknown or expired")
	ErrJoinTokenExhausted       = errors.New("store: join token max_uses exceeded")
	ErrJoinNodeNameMismatch     = errors.New("store: node name does not match token binding")
	ErrJoinNodeNameTaken        = errors.New("store: node already has an active cert")

	ErrAgentWireguardPubkeyInUse = errors.New("store: wireguard public key already in use by another node")
	ErrOverlaySupernetExhausted  = errors.New("store: overlay supernet has no free host address for a new agent")
	ErrVNIExhausted              = errors.New("store: overlay VNI range exhausted")
)

// ResourceInUseError reports that a resource cannot be deleted because other
// rows still reference it. Resources maps each blocking kind (e.g. "vms",
// "templates") to its count and is non-empty. Handlers project it onto the API
// blocking_resources envelope. Shared across every resource whose delete is
// gated on dependants so the carrier stays uniform.
type ResourceInUseError struct {
	Resources map[string]int64
}

func (e *ResourceInUseError) Error() string {
	return fmt.Sprintf("resource in use: %v", e.Resources)
}

// UnderlayBelowFloorError reports that the cluster underlay MTU is below the
// floor needed to derive a valid (>= 1280) overlay MTU, so an overlay network
// cannot be created until the operator renumbers the underlay.
type UnderlayBelowFloorError struct {
	UnderlayMTU       int32
	MinUnderlayMTU    int32
	DerivedOverlayMTU int32
}

func (e *UnderlayBelowFloorError) Error() string {
	return fmt.Sprintf("underlay mtu %d is below the floor %d (derived overlay mtu %d < 1280)",
		e.UnderlayMTU, e.MinUnderlayMTU, e.DerivedOverlayMTU)
}
