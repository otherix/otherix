// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cloudinit builds NoCloud cidata ISOs that the QEMU VM
// attaches as a CD-ROM, satisfying the cloud-init NoCloud datasource
// contract (https://cloudinit.readthedocs.io/en/latest/topics/
// datasources/nocloud.html).
//
// Per Otherix L3 Area 3 lock: operators ship raw `#cloud-config`
// YAML; this package does not parse cloud-init semantics beyond
// hostname injection. Resolution between template-baked default and
// VM-level override is CP-side; the agent receives a single
// already-resolved blob via VMSpec.cloud_init.user_data.
//
// The on-disk layout is ISO9660 с volume label "cidata", containing:
//
//	/meta-data       — instance-id + local-hostname (generated)
//	/user-data       — operator-provided `#cloud-config` YAML
//	/network-config  — optional, omitted if NetworkData is empty
//
// The package wraps github.com/diskfs/go-diskfs per Item 5 of the
// L3 spec.
package cloudinit
