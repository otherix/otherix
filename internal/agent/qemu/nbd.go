// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// migTLSCredsID and migAuthzID are the fixed QOM object ids used inside a
// single migration's qemu-nbd / qemu-img invocation.
const (
	migTLSCredsID = "migtls"
	migAuthzID    = "migauthz"
)

// QemuNBDServerSpec parameterizes the target-side NBD server that receives
// a pushed disk over mutually-authenticated TLS.
type QemuNBDServerSpec struct {
	CredsDir       string // tls-creds-x509 server dir
	SourceIdentity string // "CN=node-<source>"; pins the connecting source DN
	BindHost       string // migration ingress host (ADR 0013)
	Port           int    // reserved ingress port
	Export         string // NBD export name = the correlation auth_token
	DiskPath       string // destination disk (already created, writable)
}

// QemuNBDServerArgs builds the qemu-nbd argument vector for a writable,
// TLS-mutual-auth NBD server. Fail-closed: endpoint=server, verify-peer=on,
// and a tls-authz object pinning the source DN. Writable (no -r) so the
// source can push allocated blocks in.
func QemuNBDServerArgs(s QemuNBDServerSpec) []string {
	return []string{
		"--object", fmt.Sprintf("tls-creds-x509,id=%s,endpoint=server,dir=%s,verify-peer=on", migTLSCredsID, s.CredsDir),
		"--object", fmt.Sprintf("authz-simple,id=%s,identity=%s", migAuthzID, s.SourceIdentity),
		"--tls-creds", migTLSCredsID,
		"--tls-authz", migAuthzID,
		"--persistent",
		"-f", "qcow2",
		"-x", s.Export,
		"-b", s.BindHost,
		"-p", strconv.Itoa(s.Port),
		s.DiskPath,
	}
}

// QemuImgPushSpec parameterizes the source-side push of a stopped disk into
// the target's NBD server.
type QemuImgPushSpec struct {
	CredsDir       string // tls-creds-x509 client dir
	SourceDisk     string // local qcow2 to push (VM stopped)
	TargetHost     string // target ingress host
	TargetPort     int    // target ingress port
	TargetIdentity string // "node-<target>.agents.otherix.local"; tls-hostname pin
	Export         string // NBD export name = auth_token
}

// QemuImgPushArgs builds the qemu-img convert argument vector that pushes a
// stopped qcow2 into the target's NBD export over TLS. The NBD endpoint is
// the *target* (--target-image-opts); -n skips target creation (the target
// pre-created the disk). Sparse-aware: convert queries NBD block-status and
// skips unallocated regions. Fail-closed via endpoint=client,verify-peer=on
// and a non-empty tls-hostname.
func QemuImgPushArgs(s QemuImgPushSpec) []string {
	imgOpts := strings.Join([]string{
		"driver=nbd",
		"server.type=inet",
		"server.host=" + s.TargetHost,
		"server.port=" + strconv.Itoa(s.TargetPort),
		"tls-creds=" + migTLSCredsID,
		"tls-hostname=" + s.TargetIdentity,
		"export=" + s.Export,
	}, ",")
	return []string{
		"convert",
		"-n",
		"-f", "qcow2",
		"--object", fmt.Sprintf("tls-creds-x509,id=%s,endpoint=client,dir=%s,verify-peer=on", migTLSCredsID, s.CredsDir),
		"--target-image-opts",
		s.SourceDisk,
		imgOpts,
	}
}

// CreateDisk creates an empty qcow2 of virtualBytes at path (the migration
// destination disk, later filled by the source push). It ensures the parent
// directory exists first so qemu-img create does not fail on a missing
// per-VM state dir.
func CreateDisk(ctx context.Context, path string, virtualBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %v", filepath.Dir(path), err)
	}
	out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", path, strconv.FormatInt(virtualBytes, 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create %s: %v (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SpawnQemuNBD starts qemu-nbd detached and returns its pid. qemu-nbd does
// not daemonize by default; we start it and let it run until torn down. The
// caller tracks the pid in the migration record for teardown.
func SpawnQemuNBD(ctx context.Context, args []string) (int, error) {
	cmd := exec.Command("qemu-nbd", args...)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start qemu-nbd: %v", err)
	}
	return cmd.Process.Pid, nil
}

// RunQemuImgConvert runs qemu-img convert to completion (a blocking push).
// stderr is captured into the returned error on failure.
func RunQemuImgConvert(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
