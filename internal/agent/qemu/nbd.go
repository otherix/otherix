// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WaitNBDListening blocks until a TCP connection to endpoint succeeds or the
// timeout elapses. SpawnQemuNBD returns as soon as the process forks, but the
// TLS listener binds a beat later; the migration source connects immediately,
// so the target must not advertise the endpoint until qemu-nbd is actually
// accepting connections, or the source races it to "connection refused". A bare
// TCP probe (connect + close) is enough - qemu-nbd is --persistent, so the
// dropped non-NBD probe does not disturb the real transfer.
func WaitNBDListening(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", endpoint, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("qemu-nbd not listening on %s within %s", endpoint, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// migTLSCredsID and migAuthzID are the fixed QOM object ids used inside a
// single migration's qemu-nbd / qemu-img invocation.
const (
	migTLSCredsID = "migtls"
	migAuthzID    = "migauthz"
)

// NBDServerSpec parameterizes the target-side NBD server that receives
// a pushed disk over mutually-authenticated TLS.
type NBDServerSpec struct {
	CredsDir       string // tls-creds-x509 server dir
	SourceIdentity string // "CN=node-<source>"; pins the connecting source DN
	BindHost       string // migration ingress host (ADR 0013)
	Port           int    // reserved ingress port
	Export         string // NBD export name = the correlation auth_token
	DiskPath       string // destination disk (already created, writable)
}

// NBDServerArgs builds the qemu-nbd argument vector for a writable,
// TLS-mutual-auth NBD server. Fail-closed: endpoint=server, verify-peer=on,
// and a tls-authz object pinning the source DN. Writable (no -r) so the
// source can push allocated blocks in.
func NBDServerArgs(s NBDServerSpec) []string {
	return []string{
		"--object", fmt.Sprintf("tls-creds-x509,id=%s,endpoint=server,dir=%s,verify-peer=on", migTLSCredsID, s.CredsDir),
		"--object", fmt.Sprintf("authz-simple,id=%s,identity=%s", migAuthzID, s.SourceIdentity),
		"--tls-creds", migTLSCredsID,
		"--tls-authz", migAuthzID,
		// --persistent keeps the server up across multiple data connections so
		// the source side is free to parallelize the transfer (qemu-img -m /
		// NBD multi-conn now, blockdev-mirror + multifd in the live slice).
		// qemu-nbd holds an exclusive write lock on the disk while it runs, so
		// the CP tears it down (process kill) on the target AFTER cutover and
		// BEFORE the migrated VM boots - deterministic regardless of how many
		// connections the transfer used. A non-persistent server would release
		// the lock by self-exiting after one connection, but that would cap the
		// transfer at a single connection; persistent + explicit teardown keeps
		// both correctness and parallelism.
		"--persistent",
		// O_DIRECT on the destination disk: bypass the host page cache so the
		// received blocks land on disk directly. This matches how the VM later
		// opens the disk (cache=none) - avoiding a page-cache coherency window
		// between qemu-nbd's writes and the guest's O_DIRECT reads - and keeps
		// a large copy from evicting the host's working set.
		"--cache=none",
		"-f", "qcow2",
		"-x", s.Export,
		"-b", s.BindHost,
		"-p", strconv.Itoa(s.Port),
		s.DiskPath,
	}
}

// ImgPushSpec parameterizes the source-side push of a stopped disk into
// the target's NBD server.
type ImgPushSpec struct {
	CredsDir       string // tls-creds-x509 client dir
	SourceDisk     string // local qcow2 to push (VM stopped)
	TargetHost     string // target ingress host
	TargetPort     int    // target ingress port
	TargetIdentity string // "node-<target>.agents.otherix.local"; tls-hostname pin
	Export         string // NBD export name = auth_token
}

// ImgPushArgs builds the qemu-img convert argument vector that pushes a
// stopped qcow2 into the target's NBD export over TLS. The NBD endpoint is
// the *target* (--target-image-opts); -n skips target creation (the target
// pre-created the disk). Sparse-aware: convert queries NBD block-status and
// skips unallocated regions. Fail-closed via endpoint=client,verify-peer=on
// and a non-empty tls-hostname.
func ImgPushArgs(s ImgPushSpec) []string {
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
	// #nosec G204 -- the command is a fixed qemu binary; the args are server-constructed migration parameters (paths/ports/identities resolved by the CP and this agent), never raw user input.
	out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", path, strconv.FormatInt(virtualBytes, 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create %s: %v (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateRawDisk creates an empty raw image of virtualBytes at path (mkdir -p
// the parent first), used for the cidata ISO destination of a live migration
// (the source mirrors the real bytes in; only the container is pre-created).
func CreateRawDisk(ctx context.Context, path string, virtualBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %v", filepath.Dir(path), err)
	}
	// #nosec G204 -- the command is a fixed qemu binary; the args are server-constructed migration parameters (paths/ports/identities resolved by the CP and this agent), never raw user input.
	out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "raw", path, strconv.FormatInt(virtualBytes, 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create %s: %v (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SpawnQemuNBD starts qemu-nbd detached and returns its pid. qemu-nbd does
// not daemonize by default; we start it and let it run until torn down. The
// caller tracks the pid in the migration record for teardown.
func SpawnQemuNBD(ctx context.Context, args []string) (int, error) {
	// #nosec G204 -- the command is a fixed qemu binary; the args are server-constructed migration parameters (paths/ports/identities resolved by the CP and this agent), never raw user input.
	cmd := exec.Command("qemu-nbd", args...)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start qemu-nbd: %v", err)
	}
	return cmd.Process.Pid, nil
}

// RunQemuImgConvert runs qemu-img convert to completion (a blocking push).
// stderr is captured into the returned error on failure.
func RunQemuImgConvert(ctx context.Context, args []string) error {
	// #nosec G204 -- the command is a fixed qemu binary; the args are server-constructed migration parameters (paths/ports/identities resolved by the CP and this agent), never raw user input.
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
